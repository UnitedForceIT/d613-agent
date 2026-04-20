// e2e-test: integration test that exercises the full auth + tunnel flow
// without sudo, SSH daemon, bore, cloudflared, or Claude Code.
package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/UnitedForceIT/d613-agent/internal/server"
)

func main() {
	os.Exit(run())
}

func run() int {
	fmt.Println("=== d613-agent E2E integration test ===")
	fmt.Println()

	// 1. Start a mock SSH backend
	sshLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		fmt.Println("FAIL: listen:", err)
		return 1
	}
	defer sshLn.Close()
	sshPort := sshLn.Addr().(*net.TCPAddr).Port
	fmt.Printf("[1/8] Mock SSH backend listening on %d\n", sshPort)

	go func() {
		for {
			c, err := sshLn.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				c.Write([]byte("SSH-2.0-Mock_1.0\r\n"))
				io.Copy(c, c) // echo
			}(c)
		}
	}()

	// 2. Start agent HTTP server
	token := "test-agent-token-abc123"
	srv, err := server.New(token, server.Config{
		SSHUser: "testuser",
	})
	if err != nil {
		fmt.Println("FAIL: server.New:", err)
		return 1
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go srv.Start(ctx)
	time.Sleep(200 * time.Millisecond)
	agentURL := fmt.Sprintf("http://127.0.0.1:%d", srv.Port)
	fmt.Printf("[2/8] Agent started at %s\n", agentURL)

	// 3. Ping
	resp, err := http.Get(agentURL + "/ping")
	if err != nil || resp.StatusCode != 200 {
		fmt.Println("FAIL: ping")
		return 1
	}
	resp.Body.Close()
	fmt.Println("[3/8] GET /ping OK")

	// 4. Transfer OAuth token
	fakeOAuth := "sk-ant-oat01-FAKE-TEST-TOKEN"
	body, _ := json.Marshal(map[string]string{"oauth_token": fakeOAuth})
	req, _ := http.NewRequest("POST", agentURL+"/auth", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err = http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != 200 {
		fmt.Println("FAIL: /auth")
		return 1
	}
	var info map[string]string
	json.NewDecoder(resp.Body).Decode(&info)
	resp.Body.Close()
	if info["ssh_user"] != "testuser" {
		fmt.Println("FAIL: ssh_user mismatch:", info["ssh_user"])
		return 1
	}
	if srv.OAuthToken() != fakeOAuth {
		fmt.Println("FAIL: server did not store token")
		return 1
	}
	fmt.Println("[4/8] POST /auth transferred OAuth token")

	// 5. Reject bad tokens
	req, _ = http.NewRequest("POST", agentURL+"/auth", strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer WRONG")
	resp, err = http.DefaultClient.Do(req)
	if err == nil && resp != nil {
		if resp.StatusCode != 401 {
			fmt.Printf("FAIL: wrong token returned %d\n", resp.StatusCode)
			return 1
		}
		resp.Body.Close()
	}
	fmt.Println("[5/8] Bad agent token → 401 (expected)")

	// 6. Exec a command through /exec
	body, _ = json.Marshal(map[string]interface{}{
		"command": "echo e2e-exec-ok",
		"timeout": 5,
	})
	req, _ = http.NewRequest("POST", agentURL+"/exec", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err = http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != 200 {
		fmt.Println("FAIL: /exec")
		return 1
	}
	var execResult struct {
		Stdout   string `json:"stdout"`
		ExitCode int    `json:"exit_code"`
	}
	json.NewDecoder(resp.Body).Decode(&execResult)
	resp.Body.Close()
	if !strings.Contains(execResult.Stdout, "e2e-exec-ok") {
		fmt.Println("FAIL: /exec returned:", execResult.Stdout)
		return 1
	}
	fmt.Println("[6/8] POST /exec runs shell command")

	// 7. SSH tunnel — simulates what d613-connect --tunnel does
	u, _ := url.Parse(fmt.Sprintf("%s/ssh-tunnel?port=%d", agentURL, sshPort))
	var tunnelConn net.Conn
	if u.Scheme == "https" {
		tunnelConn, err = tls.Dial("tcp", u.Host, &tls.Config{ServerName: u.Hostname()})
	} else {
		tunnelConn, err = net.Dial("tcp", u.Host)
	}
	if err != nil {
		fmt.Println("FAIL: dial agent:", err)
		return 1
	}
	defer tunnelConn.Close()

	reqHdr := fmt.Sprintf("GET %s HTTP/1.1\r\nHost: %s\r\nAuthorization: Bearer %s\r\n\r\n",
		u.RequestURI(), u.Hostname(), token)
	if _, err := tunnelConn.Write([]byte(reqHdr)); err != nil {
		fmt.Println("FAIL: write request:", err)
		return 1
	}

	// Read HTTP response header
	tunnelConn.SetReadDeadline(time.Now().Add(5 * time.Second))
	buf := make([]byte, 0, 1024)
	one := make([]byte, 1)
	for {
		n, err := tunnelConn.Read(one)
		if err != nil {
			fmt.Println("FAIL: read response:", err)
			return 1
		}
		if n > 0 {
			buf = append(buf, one[0])
			if len(buf) >= 4 && string(buf[len(buf)-4:]) == "\r\n\r\n" {
				break
			}
		}
	}
	if !strings.HasPrefix(string(buf), "HTTP/1.1 200") {
		fmt.Printf("FAIL: tunnel rejected: %s\n", string(buf))
		return 1
	}
	fmt.Println("[7/8] /ssh-tunnel hijack accepted")

	// Read mock SSH banner through the tunnel
	tunnelConn.SetReadDeadline(time.Now().Add(3 * time.Second))
	banner := make([]byte, 64)
	n, err := tunnelConn.Read(banner)
	if err != nil {
		fmt.Println("FAIL: read banner:", err)
		return 1
	}
	if !strings.Contains(string(banner[:n]), "SSH-2.0-Mock") {
		fmt.Printf("FAIL: got banner %q\n", string(banner[:n]))
		return 1
	}
	fmt.Printf("[8/8] Tunnel forwarded SSH banner: %q\n", strings.TrimSpace(string(banner[:n])))

	fmt.Println()
	fmt.Println("=== ALL E2E TESTS PASSED ===")
	fmt.Println()
	fmt.Println("Verified pipeline:")
	fmt.Println("  d613-connect → /ping          ✓")
	fmt.Println("  d613-connect → /auth  (store) ✓")
	fmt.Println("  d613-connect → /exec          ✓")
	fmt.Println("  d613-connect → /ssh-tunnel    ✓ (forwards raw TCP to :22)")
	fmt.Println("  401 rejection on bad token    ✓")
	return 0
}
