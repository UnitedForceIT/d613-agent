//go:build !windows

// Package sshsetup enables OpenSSH Server on macOS and Linux for the duration
// of the agent session and cleans up on exit.
package sshsetup

import (
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"strings"
)

// Session holds the state needed to undo what we enabled.
type Session struct {
	sshdWasEnabled bool
	authKeysPath   string
	pubKeyLine     string
	privateKeyPath string
	username       string
	platform       string // "darwin" | "linux"
}

// Enable starts OpenSSH Server, adds a temporary SSH key to authorized_keys,
// and returns a Session that can be Disable()d.
func Enable() (*Session, error) {
	s := &Session{platform: runtime.GOOS}

	u, err := user.Current()
	if err != nil {
		return nil, fmt.Errorf("get current user: %w", err)
	}
	s.username = u.Username

	// ── Start SSH server ─────────────────────────────────────────────────────
	s.sshdWasEnabled = sshIsRunning()
	if !s.sshdWasEnabled {
		if err := startSSHD(); err != nil {
			return nil, fmt.Errorf("enable sshd: %w (hint: agent may need elevated privileges)", err)
		}
		fmt.Println("  [ssh] SSH server started.")
	} else {
		fmt.Println("  [ssh] SSH server already running.")
	}

	// ── Temporary SSH key ────────────────────────────────────────────────────
	keyPath := filepath.Join(os.TempDir(), "d613_session_key")
	os.Remove(keyPath)
	os.Remove(keyPath + ".pub")

	if err := run("ssh-keygen", "-t", "ed25519", "-f", keyPath, "-N", "", "-q"); err != nil {
		return nil, fmt.Errorf("ssh-keygen: %w", err)
	}
	s.privateKeyPath = keyPath
	os.Chmod(keyPath, 0600)

	pubKeyBytes, err := os.ReadFile(keyPath + ".pub")
	if err != nil {
		return nil, fmt.Errorf("read public key: %w", err)
	}
	pubKeyLine := strings.TrimSpace(string(pubKeyBytes))
	s.pubKeyLine = pubKeyLine

	sshDir := filepath.Join(u.HomeDir, ".ssh")
	if err := os.MkdirAll(sshDir, 0700); err != nil {
		return nil, fmt.Errorf("mkdir ~/.ssh: %w", err)
	}
	s.authKeysPath = filepath.Join(sshDir, "authorized_keys")

	f, err := os.OpenFile(s.authKeysPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		return nil, fmt.Errorf("open authorized_keys: %w", err)
	}
	fmt.Fprintf(f, "\n%s\n", pubKeyLine)
	f.Close()

	// Ensure correct perms (SSH is strict about this)
	os.Chmod(s.authKeysPath, 0600)
	os.Chmod(sshDir, 0700)

	return s, nil
}

// SSHPort returns the standard SSH port.
func (s *Session) SSHPort() int { return 22 }

// PrivateKeyPath returns the path to the temporary private key.
func (s *Session) PrivateKeyPath() string { return s.privateKeyPath }

// Username returns the Unix username for the SSH connection.
func (s *Session) Username() string { return s.username }

// Disable removes the temporary SSH key and restores the previous sshd state.
func (s *Session) Disable() {
	fmt.Println("\nCleaning up SSH session...")

	if s.authKeysPath != "" && s.pubKeyLine != "" {
		removeKeyLine(s.authKeysPath, s.pubKeyLine)
	}
	os.Remove(s.privateKeyPath)
	os.Remove(s.privateKeyPath + ".pub")

	// Only stop sshd if it wasn't running before. (Conservative: many admins
	// leave SSH on intentionally; we don't want to break anything.)
	if !s.sshdWasEnabled {
		stopSSHD()
	}
}

// ── platform-specific SSH daemon control ───────────────────────────────────

func sshIsRunning() bool {
	switch runtime.GOOS {
	case "darwin":
		// On macOS, Remote Login is controlled by systemsetup or by launchctl.
		out, err := exec.Command("launchctl", "list", "com.openssh.sshd").Output()
		if err == nil && len(out) > 0 {
			return true
		}
		// Fallback: check if port 22 is bound by sshd
		return portBound(22)
	case "linux":
		// Try common service names
		for _, name := range []string{"sshd", "ssh"} {
			out, err := exec.Command("systemctl", "is-active", name).Output()
			if err == nil && strings.TrimSpace(string(out)) == "active" {
				return true
			}
		}
		return portBound(22)
	}
	return false
}

func startSSHD() error {
	switch runtime.GOOS {
	case "darwin":
		// systemsetup -setremotelogin on requires root AND Full Disk Access.
		// Fall back to launchctl, which may succeed for user-level sshd.
		if err := run("sudo", "-n", "systemsetup", "-setremotelogin", "on"); err == nil {
			return nil
		}
		// Best-effort — if we're already root, try launchctl load
		if os.Geteuid() == 0 {
			return run("launchctl", "load", "-w", "/System/Library/LaunchDaemons/ssh.plist")
		}
		return fmt.Errorf("SSH not running and cannot enable it without root privileges")
	case "linux":
		for _, name := range []string{"sshd", "ssh"} {
			if err := run("systemctl", "start", name); err == nil {
				return nil
			}
		}
		// Fallback: try service command (older distros)
		for _, name := range []string{"sshd", "ssh"} {
			if err := run("service", name, "start"); err == nil {
				return nil
			}
		}
		return fmt.Errorf("could not start sshd service")
	}
	return fmt.Errorf("unsupported platform: %s", runtime.GOOS)
}

func stopSSHD() {
	switch runtime.GOOS {
	case "darwin":
		run("sudo", "-n", "systemsetup", "-setremotelogin", "off")
	case "linux":
		for _, name := range []string{"sshd", "ssh"} {
			if err := run("systemctl", "stop", name); err == nil {
				return
			}
		}
	}
}

// portBound returns true if something is listening on the given local port.
func portBound(port int) bool {
	out, err := exec.Command("sh", "-c",
		fmt.Sprintf("lsof -iTCP:%d -sTCP:LISTEN -n -P 2>/dev/null | head -2", port)).Output()
	return err == nil && strings.Contains(string(out), "LISTEN")
}

// ── helpers ──────────────────────────────────────────────────────────────────

func run(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func removeKeyLine(path, keyLine string) {
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	var out []string
	for _, line := range strings.Split(string(data), "\n") {
		if strings.TrimSpace(line) != strings.TrimSpace(keyLine) {
			out = append(out, line)
		}
	}
	os.WriteFile(path, []byte(strings.Join(out, "\n")), 0600)
}
