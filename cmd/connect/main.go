// d613-connect — admin-side companion for d613-agent.
//
// Usage:
//
//	d613-connect <URL> <TOKEN>            # interactive Claude Code session
//	d613-connect --tunnel <URL> <TOKEN>   # stdio TCP tunnel (for ProxyCommand)
//
// Connects to a running d613-agent, transfers your local Claude Code
// authentication token to the remote machine, and opens an interactive
// SSH session in which Claude Code runs directly on the remote machine.
// All temporary credentials and settings are cleaned up when you disconnect.
package main

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

var version = "dev"

func main() {
	// Tunnel mode: used by SSH ProxyCommand to bridge stdio ↔ remote HTTPS tunnel.
	if len(os.Args) >= 4 && os.Args[1] == "--tunnel" {
		runTunnel(os.Args[2], os.Args[3])
		return
	}

	if len(os.Args) < 3 {
		fmt.Fprintf(os.Stderr, "D613 Labs Connect  v%s\n\nUsage: d613-connect <URL> <TOKEN>\n", version)
		os.Exit(1)
	}

	remoteURL := strings.TrimRight(os.Args[1], "/")
	agentToken := os.Args[2]

	fmt.Printf("D613 Labs Connect  v%s\n", version)
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Printf("Connecting to %s ...\n", remoteURL)

	if err := ping(remoteURL); err != nil {
		fmt.Fprintf(os.Stderr, "Connection failed: %v\n", err)
		os.Exit(1)
	}
	fmt.Println("Connected!")

	// ── Get Claude OAuth token from local keychain ─────────────────────────
	fmt.Println("Reading Claude Code credentials from local keychain...")
	oauthToken, err := getLocalClaudeToken()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Could not read Claude credentials: %v\n", err)
		fmt.Fprintln(os.Stderr, "Make sure Claude Code is authenticated on this machine.")
		os.Exit(1)
	}
	fmt.Println("Claude credentials found.")

	// ── Send auth token to remote agent ───────────────────────────────────
	fmt.Println("Transferring credentials to remote agent...")
	info, err := postAuth(remoteURL, agentToken, oauthToken)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Auth transfer failed: %v\n", err)
		os.Exit(1)
	}
	fmt.Println("Credentials transferred.")

	// ── Download temporary SSH private key ────────────────────────────────
	keyFile, err := downloadSSHKey(remoteURL, agentToken)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Could not get SSH key: %v\n", err)
		fmt.Fprintln(os.Stderr, "Falling back to password-based SSH.")
	} else {
		defer os.Remove(keyFile)
	}

	// ── Determine SSH target ───────────────────────────────────────────────
	sshUser := info["ssh_user"]
	borePath := info["bore"]

	fmt.Println()
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println("Opening remote Claude Code session...")
	fmt.Println("Claude Code is running on the remote machine.")
	fmt.Println("Type 'exit' or press Ctrl+C to end the session.")
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println()

	if err := sshIntoClaude(sshUser, borePath, keyFile, oauthToken, remoteURL, agentToken); err != nil {
		fmt.Fprintf(os.Stderr, "\nSession ended: %v\n", err)
	}

	fmt.Println("\nSession closed.")
}

// ── Credential extraction ──────────────────────────────────────────────────

// getLocalClaudeToken reads the Claude Code OAuth access token from the local
// OS keychain (macOS Keychain or Linux secret service).
func getLocalClaudeToken() (string, error) {
	switch runtime.GOOS {
	case "darwin":
		return getMacToken()
	case "linux":
		return getLinuxToken()
	default:
		return "", fmt.Errorf("unsupported platform: %s", runtime.GOOS)
	}
}

func getMacToken() (string, error) {
	out, err := exec.Command("security", "find-generic-password",
		"-s", "Claude Code-credentials", "-w").Output()
	if err != nil {
		return "", fmt.Errorf("keychain lookup failed: %w", err)
	}
	return parseOAuthToken(strings.TrimSpace(string(out)))
}

func getLinuxToken() (string, error) {
	// Try secret-tool (GNOME keyring)
	out, err := exec.Command("secret-tool", "lookup",
		"service", "Claude Code-credentials").Output()
	if err == nil {
		return parseOAuthToken(strings.TrimSpace(string(out)))
	}
	// Fallback: check known file path used by Claude Code in remote/headless mode
	paths := []string{
		filepath.Join(os.Getenv("HOME"), ".claude", "remote", ".oauth_token"),
		"/home/claude/.claude/remote/.oauth_token",
	}
	for _, p := range paths {
		if data, err := os.ReadFile(p); err == nil {
			return strings.TrimSpace(string(data)), nil
		}
	}
	return "", fmt.Errorf("could not find Claude credentials (try: secret-tool or ~/.claude/remote/.oauth_token)")
}

// parseOAuthToken extracts the access token from the JSON credential blob.
func parseOAuthToken(raw string) (string, error) {
	var creds struct {
		ClaudeAiOauth struct {
			AccessToken string `json:"accessToken"`
		} `json:"claudeAiOauth"`
	}
	if err := json.Unmarshal([]byte(raw), &creds); err != nil {
		// Maybe the raw value is already just the token
		if strings.HasPrefix(raw, "sk-ant-") {
			return raw, nil
		}
		return "", fmt.Errorf("parse credentials: %w", err)
	}
	if creds.ClaudeAiOauth.AccessToken == "" {
		return "", fmt.Errorf("no access token in credentials")
	}
	return creds.ClaudeAiOauth.AccessToken, nil
}

// ── HTTP helpers ───────────────────────────────────────────────────────────

func ping(remoteURL string) error {
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Get(remoteURL + "/ping")
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("server returned HTTP %d", resp.StatusCode)
	}
	return nil
}

// postAuth sends the Claude OAuth token to the remote agent and returns the
// session info (ssh_user, bore path, etc.).
func postAuth(remoteURL, agentToken, oauthToken string) (map[string]string, error) {
	body, _ := json.Marshal(map[string]string{"oauth_token": oauthToken})
	client := &http.Client{Timeout: 15 * time.Second}
	req, _ := http.NewRequest(http.MethodPost, remoteURL+"/auth", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+agentToken)
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized {
		return nil, fmt.Errorf("invalid agent token")
	}
	var result map[string]string
	raw, _ := io.ReadAll(resp.Body)
	json.Unmarshal(raw, &result)
	return result, nil
}

// downloadSSHKey fetches the temporary private key from the remote agent and
// writes it to a temp file with 0600 permissions.
func downloadSSHKey(remoteURL, agentToken string) (string, error) {
	client := &http.Client{Timeout: 10 * time.Second}
	req, _ := http.NewRequest(http.MethodGet, remoteURL+"/ssh-key", nil)
	req.Header.Set("Authorization", "Bearer "+agentToken)
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("no SSH key available (HTTP %d)", resp.StatusCode)
	}
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	f, err := os.CreateTemp("", "d613-key-*")
	if err != nil {
		return "", err
	}
	f.Write(data)
	f.Close()
	os.Chmod(f.Name(), 0600)
	return f.Name(), nil
}

// ── SSH session ────────────────────────────────────────────────────────────

// sshIntoClaude opens an interactive SSH session and starts Claude Code on
// the remote machine with the provided OAuth token set as an environment var.
//
// It tries two SSH transports:
//  1. Direct TCP via bore tunnel (if available)
//  2. Hijacked HTTPS over the Cloudflare tunnel via `d613-connect --tunnel`
//     (used as SSH ProxyCommand)
//
// #2 works on every platform and requires no downloaded tunnel binary.
func sshIntoClaude(sshUser, borePath, keyFile, oauthToken, remoteURL, agentToken string) error {
	if sshUser == "" {
		return fmt.Errorf("remote agent did not provide SSH user info")
	}

	args := []string{
		"-t", // force PTY (needed for interactive TUI)
		"-o", "StrictHostKeyChecking=no",
		"-o", "UserKnownHostsFile=/dev/null",
		"-o", "LogLevel=ERROR",
	}

	var host string

	if borePath != "" {
		// bore.pub:PORT — direct TCP
		parts := strings.Split(borePath, ":")
		if len(parts) == 2 {
			host = parts[0]
			args = append(args, "-p", parts[1])
		}
	}

	if host == "" {
		// Fallback: use d613-connect as a ProxyCommand over the HTTPS tunnel
		selfPath, err := os.Executable()
		if err != nil {
			return fmt.Errorf("cannot resolve self path: %w", err)
		}
		selfPath, _ = filepath.EvalSymlinks(selfPath)

		proxyCmd := fmt.Sprintf("%s --tunnel %s %s",
			shellQuote(selfPath), shellQuote(remoteURL), shellQuote(agentToken))
		args = append(args, "-o", "ProxyCommand="+proxyCmd)
		host = "remote-agent" // hostname is arbitrary when using ProxyCommand
	}

	if keyFile != "" {
		args = append(args, "-i", keyFile)
	}
	args = append(args, fmt.Sprintf("%s@%s", sshUser, host))

	// Remote command: set env var and launch claude via npx (cross-shell compatible).
	// npx -y skips the install prompt; works even if claude isn't globally installed.
	remoteCmd := fmt.Sprintf(
		"CLAUDE_CODE_OAUTH_TOKEN=%q npx -y @anthropic-ai/claude-code --dangerously-skip-permissions",
		oauthToken,
	)
	args = append(args, remoteCmd)

	cmd := exec.Command("ssh", args...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// shellQuote wraps s in single quotes for POSIX shells, handling embedded quotes.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// ── Tunnel mode (SSH ProxyCommand) ─────────────────────────────────────────

// runTunnel opens an HTTP connection to /ssh-tunnel on the remote agent,
// hijacks it, and copies bytes between stdin/stdout and the remote TCP stream.
// This is used as an SSH ProxyCommand to bridge the stdio pipe of the SSH
// client to the remote SSH daemon over the Cloudflare HTTPS tunnel.
func runTunnel(remoteURL, agentToken string) {
	remoteURL = strings.TrimRight(remoteURL, "/")
	u, err := url.Parse(remoteURL + "/ssh-tunnel")
	if err != nil {
		fmt.Fprintf(os.Stderr, "d613-connect tunnel: invalid URL: %v\n", err)
		os.Exit(1)
	}

	var conn net.Conn
	host := u.Host
	if u.Scheme == "https" {
		if !strings.Contains(host, ":") {
			host += ":443"
		}
		conn, err = tls.Dial("tcp", host, &tls.Config{ServerName: u.Hostname()})
	} else {
		if !strings.Contains(host, ":") {
			host += ":80"
		}
		conn, err = net.Dial("tcp", host)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "d613-connect tunnel: dial: %v\n", err)
		os.Exit(1)
	}
	defer conn.Close()

	// Send GET request with Bearer token — we'll upgrade via hijack on 200.
	req := fmt.Sprintf("GET %s HTTP/1.1\r\nHost: %s\r\nAuthorization: Bearer %s\r\nConnection: Upgrade\r\nUpgrade: tcp\r\n\r\n",
		u.RequestURI(), u.Hostname(), agentToken)
	if _, err := conn.Write([]byte(req)); err != nil {
		fmt.Fprintf(os.Stderr, "d613-connect tunnel: write: %v\n", err)
		os.Exit(1)
	}

	// Read the HTTP response header up to \r\n\r\n, then stream raw bytes.
	if err := readHTTPResponseHeader(conn); err != nil {
		fmt.Fprintf(os.Stderr, "d613-connect tunnel: %v\n", err)
		os.Exit(1)
	}

	// Now bidirectionally copy stdin/stdout ↔ conn.
	done := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(conn, os.Stdin); done <- struct{}{} }()
	go func() { _, _ = io.Copy(os.Stdout, conn); done <- struct{}{} }()
	<-done
}

// readHTTPResponseHeader reads bytes until \r\n\r\n and validates 200 status.
func readHTTPResponseHeader(r io.Reader) error {
	buf := make([]byte, 0, 1024)
	one := make([]byte, 1)
	for {
		n, err := r.Read(one)
		if err != nil {
			return fmt.Errorf("read response: %w", err)
		}
		if n == 0 {
			continue
		}
		buf = append(buf, one[0])
		if len(buf) >= 4 && bytes.Equal(buf[len(buf)-4:], []byte("\r\n\r\n")) {
			break
		}
		if len(buf) > 8192 {
			return fmt.Errorf("response header too large")
		}
	}
	firstLine := string(buf[:bytes.IndexByte(buf, '\r')])
	if !strings.HasPrefix(firstLine, "HTTP/1.1 200") && !strings.HasPrefix(firstLine, "HTTP/1.0 200") {
		return fmt.Errorf("tunnel rejected: %s", firstLine)
	}
	return nil
}

// bytes package is used above; alias for readability.
var _ = time.Millisecond
