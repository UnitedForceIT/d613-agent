// d613-connect — client-side companion for d613-agent.
//
// This binary has two operating modes selected by the first argument:
//
// Launcher mode (what the user runs):
//
//	d613-connect <URL> <TOKEN>
//
//	  Verifies connectivity, writes a temporary shell-proxy wrapper, then
//	  launches Claude Code with SHELL and PATH overridden so that every
//	  bash invocation is transparently forwarded to the remote agent.
//
// Proxy mode (called by Claude Code internally):
//
//	d613-connect --proxy [-c "command"]
//
//	  Reads D613_REMOTE_URL and D613_REMOTE_TOKEN from the environment,
//	  POSTs the command to the remote agent, writes stdout/stderr to the
//	  appropriate streams, and exits with the remote exit code.
//	  Users never need to call this mode directly.
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

var version = "dev"

func main() {
	if len(os.Args) >= 2 && os.Args[1] == "--proxy" {
		runProxy()
		return
	}
	runLauncher()
}

// ── Proxy mode ────────────────────────────────────────────────────────────────

// runProxy is invoked as a shell by Claude Code (SHELL=/tmp/.../d613-bash).
// Claude Code calls shells as:  shell -c "command string"
func runProxy() {
	remoteURL := strings.TrimRight(os.Getenv("D613_REMOTE_URL"), "/")
	token := os.Getenv("D613_REMOTE_TOKEN")

	if remoteURL == "" || token == "" {
		fmt.Fprintln(os.Stderr, "d613-connect --proxy: D613_REMOTE_URL or D613_REMOTE_TOKEN not set")
		os.Exit(1)
	}

	// Parse -c "command" from args (standard POSIX shell invocation).
	args := os.Args[2:] // drop binary name and "--proxy"
	var command string
	for i, a := range args {
		if a == "-c" && i+1 < len(args) {
			command = args[i+1]
			break
		}
	}
	if command == "" {
		command = strings.Join(args, " ")
	}
	if command == "" {
		os.Exit(0) // no-op
	}

	res := execRemote(remoteURL, token, command, 120)
	os.Stdout.WriteString(res.Stdout)
	if res.Stdout != "" && !strings.HasSuffix(res.Stdout, "\n") {
		os.Stdout.WriteString("\n")
	}
	os.Exit(res.ExitCode)
}

// ── Launcher mode ─────────────────────────────────────────────────────────────

func runLauncher() {
	if len(os.Args) < 3 {
		fmt.Fprintf(os.Stderr, "D613 Labs Connect  v%s\n\nUsage: d613-connect <URL> <TOKEN>\n", version)
		os.Exit(1)
	}

	remoteURL := strings.TrimRight(os.Args[1], "/")
	token := os.Args[2]

	fmt.Printf("D613 Labs Connect  v%s\n", version)
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Printf("Connecting to %s ...\n", remoteURL)

	if err := ping(remoteURL); err != nil {
		fmt.Fprintf(os.Stderr, "Connection failed: %v\n", err)
		os.Exit(1)
	}
	fmt.Println("Connected!")

	// Resolve this binary's path so the proxy wrapper can call it.
	self, err := os.Executable()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Cannot resolve own path: %v\n", err)
		os.Exit(1)
	}
	self, _ = filepath.EvalSymlinks(self)

	// Create a temp directory and write a fake "bash" wrapper inside it.
	// We prepend this directory to PATH so that even if Claude Code
	// hard-codes "bash" (rather than honouring $SHELL), it finds our proxy.
	tmpBin, err := os.MkdirTemp("", "d613-bin-*")
	if err != nil {
		fmt.Fprintf(os.Stderr, "MkdirTemp: %v\n", err)
		os.Exit(1)
	}
	defer os.RemoveAll(tmpBin)

	var fakeBash string
	if runtime.GOOS == "windows" {
		fakeBash = filepath.Join(tmpBin, "bash.bat")
		content := fmt.Sprintf("@echo off\n\"%s\" --proxy %%*\n", self)
		os.WriteFile(fakeBash, []byte(content), 0755)
	} else {
		fakeBash = filepath.Join(tmpBin, "bash")
		content := fmt.Sprintf("#!/bin/sh\nexec \"%s\" --proxy \"$@\"\n", self)
		os.WriteFile(fakeBash, []byte(content), 0755)
	}

	// Find claude CLI.
	claudePath, err := exec.LookPath("claude")
	if err != nil {
		fmt.Fprintln(os.Stderr, "Error: 'claude' not found in PATH.  Install Claude Code first.")
		os.Exit(1)
	}

	fmt.Printf("\nRemote shell active: %s\n", remoteURL)
	fmt.Println("Launching Claude Code — all commands will run on the remote machine.")
	fmt.Println("Type 'exit' in Claude Code or press Ctrl+C here to end the session.\n")

	// Build environment: override SHELL + PATH so Claude Code routes commands
	// through the proxy regardless of how it invokes the shell binary.
	currentPath := os.Getenv("PATH")
	var newPath string
	if runtime.GOOS == "windows" {
		newPath = tmpBin + ";" + currentPath
	} else {
		newPath = tmpBin + ":" + currentPath
	}

	env := append(os.Environ(),
		"SHELL="+fakeBash,
		"PATH="+newPath,
		"D613_REMOTE_URL="+remoteURL,
		"D613_REMOTE_TOKEN="+token,
	)

	cmd := exec.Command(claudePath)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Env = env

	if err := cmd.Run(); err != nil {
		// Claude Code exiting with non-zero is normal (e.g. user Ctrl+C).
		if exitErr, ok := err.(*exec.ExitError); ok {
			os.Exit(exitErr.ExitCode())
		}
	}
}

// ── Shared helpers ────────────────────────────────────────────────────────────

type execResult struct {
	Stdout     string `json:"stdout"`
	ExitCode   int    `json:"exit_code"`
	DurationMs int64  `json:"duration_ms"`
}

func execRemote(remoteURL, token, command string, timeoutSec int) execResult {
	payload := map[string]interface{}{
		"command": command,
		"timeout": timeoutSec,
	}
	body, _ := json.Marshal(payload)

	client := &http.Client{Timeout: time.Duration(timeoutSec+15) * time.Second}
	req, _ := http.NewRequest(http.MethodPost, remoteURL+"/exec", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return execResult{Stdout: "d613-connect: remote exec failed: " + err.Error(), ExitCode: 1}
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusUnauthorized {
		return execResult{Stdout: "d613-connect: invalid token", ExitCode: 1}
	}

	var result execResult
	raw, _ := io.ReadAll(resp.Body)
	if err := json.Unmarshal(raw, &result); err != nil {
		return execResult{Stdout: string(raw), ExitCode: 1}
	}
	return result
}

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
