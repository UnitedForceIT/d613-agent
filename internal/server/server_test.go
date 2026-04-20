package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

// startTestServer spins up a real Server on a random port and returns its URL +
// a cleanup function. Uses a real executor.Session (so the shell must work).
func startTestServer(t *testing.T, cfg Config) (url, token string, cleanup func()) {
	t.Helper()
	token = "test-token-abcdef"
	srv, err := New(token, cfg)
	if err != nil {
		t.Fatalf("server.New failed: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go srv.Start(ctx)
	// Wait for the server to be ready.
	url = fmt.Sprintf("http://127.0.0.1:%d", srv.Port)
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get(url + "/ping")
		if err == nil {
			resp.Body.Close()
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	cleanup = func() { cancel(); time.Sleep(100 * time.Millisecond) }
	return
}

func TestPingUnauthenticated(t *testing.T) {
	url, _, cleanup := startTestServer(t, Config{})
	defer cleanup()

	resp, err := http.Get(url + "/ping")
	if err != nil {
		t.Fatalf("GET /ping: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("ping returned %d, want 200", resp.StatusCode)
	}
}

func TestAuthRequiredForExec(t *testing.T) {
	url, _, cleanup := startTestServer(t, Config{})
	defer cleanup()

	body, _ := json.Marshal(map[string]string{"command": "echo hello"})
	resp, err := http.Post(url+"/exec", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST /exec without token: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("unauthenticated /exec returned %d, want 401", resp.StatusCode)
	}
}

func TestExecWithAuth(t *testing.T) {
	url, token, cleanup := startTestServer(t, Config{})
	defer cleanup()

	body, _ := json.Marshal(map[string]interface{}{
		"command": "echo hello-world",
		"timeout": 5,
	})
	req, _ := http.NewRequest("POST", url+"/exec", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST /exec: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("exec returned %d, want 200", resp.StatusCode)
	}

	var r struct {
		Stdout   string `json:"stdout"`
		ExitCode int    `json:"exit_code"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !strings.Contains(r.Stdout, "hello-world") {
		t.Errorf("stdout=%q, want contains 'hello-world'", r.Stdout)
	}
	if r.ExitCode != 0 {
		t.Errorf("exit_code=%d, want 0", r.ExitCode)
	}
}

func TestAuthEndpointStoresOAuthToken(t *testing.T) {
	url, token, cleanup := startTestServer(t, Config{SSHUser: "testuser"})
	defer cleanup()

	oauth := "sk-ant-oat01-test-token-value-12345"
	body, _ := json.Marshal(map[string]string{"oauth_token": oauth})
	req, _ := http.NewRequest("POST", url+"/auth", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST /auth: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("auth returned %d: %s", resp.StatusCode, raw)
	}

	var r map[string]interface{}
	json.NewDecoder(resp.Body).Decode(&r)
	if r["ssh_user"] != "testuser" {
		t.Errorf("ssh_user=%v, want testuser", r["ssh_user"])
	}
	if r["status"] != "ok" {
		t.Errorf("status=%v, want ok", r["status"])
	}
}

func TestSSHKeyEndpoint(t *testing.T) {
	// Write a fake private key to a temp file
	tmp, err := os.CreateTemp("", "test-key-*")
	if err != nil {
		t.Fatalf("temp file: %v", err)
	}
	defer os.Remove(tmp.Name())
	fakeKey := "-----BEGIN OPENSSH PRIVATE KEY-----\ntest-key-contents\n-----END OPENSSH PRIVATE KEY-----\n"
	tmp.WriteString(fakeKey)
	tmp.Close()

	url, token, cleanup := startTestServer(t, Config{SSHKeyPath: tmp.Name()})
	defer cleanup()

	req, _ := http.NewRequest("GET", url+"/ssh-key", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /ssh-key: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("ssh-key returned %d, want 200", resp.StatusCode)
	}
	raw, _ := io.ReadAll(resp.Body)
	if string(raw) != fakeKey {
		t.Errorf("returned key does not match stored key")
	}
}

func TestSSHKeyEndpoint404IfNoKey(t *testing.T) {
	url, token, cleanup := startTestServer(t, Config{}) // no SSHKeyPath
	defer cleanup()

	req, _ := http.NewRequest("GET", url+"/ssh-key", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /ssh-key: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("ssh-key with no key returned %d, want 404", resp.StatusCode)
	}
}

func TestWrongTokenRejected(t *testing.T) {
	url, _, cleanup := startTestServer(t, Config{})
	defer cleanup()

	req, _ := http.NewRequest("GET", url+"/info", nil)
	req.Header.Set("Authorization", "Bearer wrong-token")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /info: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("wrong token returned %d, want 401", resp.StatusCode)
	}
}

// TestSSHTunnelBidirectional verifies the hijacked TCP proxy forwards bytes
// both ways between the HTTP connection and a local target.
func TestSSHTunnelBidirectional(t *testing.T) {
	// Start a tiny echo-style TCP server on a random port to represent "sshd".
	backendLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen backend: %v", err)
	}
	defer backendLn.Close()
	backendPort := backendLn.Addr().(*net.TCPAddr).Port

	go func() {
		for {
			c, err := backendLn.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				buf := make([]byte, 1024)
				n, _ := c.Read(buf)
				// Echo with a prefix
				c.Write(append([]byte("ECHO:"), buf[:n]...))
			}(c)
		}
	}()

	url, token, cleanup := startTestServer(t, Config{})
	defer cleanup()

	// Make a raw TCP call to the hijacker endpoint, pointing at our fake sshd.
	u := strings.TrimPrefix(url, "http://")
	rawConn, err := net.Dial("tcp", u)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer rawConn.Close()

	req := fmt.Sprintf("GET /ssh-tunnel?port=%d HTTP/1.1\r\nHost: %s\r\nAuthorization: Bearer %s\r\nConnection: Upgrade\r\nUpgrade: tcp\r\n\r\n",
		backendPort, u, token)
	if _, err := rawConn.Write([]byte(req)); err != nil {
		t.Fatalf("write request: %v", err)
	}

	// Read response headers until \r\n\r\n
	buf := make([]byte, 0, 1024)
	one := make([]byte, 1)
	rawConn.SetReadDeadline(time.Now().Add(3 * time.Second))
	for {
		n, err := rawConn.Read(one)
		if err != nil {
			t.Fatalf("read response: %v", err)
		}
		if n == 0 {
			continue
		}
		buf = append(buf, one[0])
		if len(buf) >= 4 && string(buf[len(buf)-4:]) == "\r\n\r\n" {
			break
		}
	}
	if !strings.HasPrefix(string(buf), "HTTP/1.1 200") {
		t.Fatalf("expected 200 OK, got: %q", string(buf[:100]))
	}

	// Now send a payload through the tunnel and expect echoed response.
	rawConn.SetReadDeadline(time.Now().Add(3 * time.Second))
	rawConn.Write([]byte("hello-tunnel"))

	// Read echo reply
	reply := make([]byte, 64)
	n, err := rawConn.Read(reply)
	if err != nil {
		t.Fatalf("read echo: %v", err)
	}
	got := string(reply[:n])
	if !strings.HasPrefix(got, "ECHO:hello-tunnel") {
		t.Errorf("got %q, want prefix 'ECHO:hello-tunnel'", got)
	}
}

func TestSSHTunnelBadBackend(t *testing.T) {
	url, token, cleanup := startTestServer(t, Config{})
	defer cleanup()

	u := strings.TrimPrefix(url, "http://")
	rawConn, err := net.Dial("tcp", u)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer rawConn.Close()

	// Point at a closed port — agent should return 502
	req := fmt.Sprintf("GET /ssh-tunnel?port=1 HTTP/1.1\r\nHost: %s\r\nAuthorization: Bearer %s\r\n\r\n",
		u, token)
	rawConn.Write([]byte(req))

	buf := make([]byte, 256)
	rawConn.SetReadDeadline(time.Now().Add(6 * time.Second))
	n, _ := rawConn.Read(buf)
	if !strings.Contains(string(buf[:n]), "502") {
		t.Errorf("expected 502, got: %q", string(buf[:n]))
	}
}

func TestInfoWithAuth(t *testing.T) {
	url, token, cleanup := startTestServer(t, Config{SSHUser: "alice", BorePath: "bore.pub:12345"})
	defer cleanup()

	req, _ := http.NewRequest("GET", url+"/info", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /info: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("info returned %d", resp.StatusCode)
	}
	var r map[string]string
	json.NewDecoder(resp.Body).Decode(&r)
	if r["ssh_user"] != "alice" {
		t.Errorf("ssh_user=%q, want alice", r["ssh_user"])
	}
	if r["bore_path"] != "bore.pub:12345" {
		t.Errorf("bore_path=%q, want bore.pub:12345", r["bore_path"])
	}
}
