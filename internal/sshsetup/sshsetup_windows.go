// Package sshsetup enables OpenSSH Server and PowerShell Remoting on Windows
// for the duration of the agent session and cleans up on exit.
package sshsetup

import (
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"
)

// Session holds the state needed to undo what we enabled.
type Session struct {
	sshdWasRunning  bool
	winrmWasRunning bool
	authKeysPath    string
	pubKeyLine      string
	privateKeyPath  string
	username        string
}

// Enable starts OpenSSH Server and PowerShell Remoting, adds a temporary
// SSH key to authorized_keys, and returns a Session that can be Disable()d.
func Enable() (*Session, error) {
	s := &Session{}

	u, err := user.Current()
	if err != nil {
		return nil, fmt.Errorf("get current user: %w", err)
	}
	s.username = u.Username
	// Strip domain prefix if present (DOMAIN\user → user)
	if idx := strings.LastIndex(s.username, "\\"); idx >= 0 {
		s.username = s.username[idx+1:]
	}

	// ── OpenSSH Server ──────────────────────────────────────────────────────
	s.sshdWasRunning = serviceRunning("sshd")

	// Install OpenSSH Server if not present (Windows 10 optional feature).
	if !serviceExists("sshd") {
		fmt.Println("  [ssh] Installing OpenSSH Server (this may take a minute)...")
		ps(`Add-WindowsCapability -Online -Name OpenSSH.Server~~~~0.0.1.0`)
	}

	if !s.sshdWasRunning {
		fmt.Println("  [ssh] Starting OpenSSH Server...")
		if err := ps(`Start-Service sshd`); err != nil {
			return nil, fmt.Errorf("start sshd: %w", err)
		}
	}

	// ── Temporary SSH key ───────────────────────────────────────────────────
	keyPath := filepath.Join(os.TempDir(), "d613_session_key")
	os.Remove(keyPath)
	os.Remove(keyPath + ".pub")

	if err := run("ssh-keygen", "-t", "ed25519", "-f", keyPath, "-N", "", "-q"); err != nil {
		return nil, fmt.Errorf("ssh-keygen: %w", err)
	}
	s.privateKeyPath = keyPath

	pubKeyBytes, err := os.ReadFile(keyPath + ".pub")
	if err != nil {
		return nil, fmt.Errorf("read public key: %w", err)
	}
	pubKeyLine := strings.TrimSpace(string(pubKeyBytes))
	s.pubKeyLine = pubKeyLine

	// For Administrator the authorised-keys file lives in a special location.
	sshDir := filepath.Join(u.HomeDir, ".ssh")
	if strings.EqualFold(s.username, "administrator") {
		sshDir = `C:\ProgramData\ssh`
	}
	os.MkdirAll(sshDir, 0700)
	s.authKeysPath = filepath.Join(sshDir, "authorized_keys")

	f, err := os.OpenFile(s.authKeysPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		return nil, fmt.Errorf("open authorized_keys: %w", err)
	}
	fmt.Fprintf(f, "\n%s\n", pubKeyLine)
	f.Close()

	// ── PowerShell Remoting (WinRM) ─────────────────────────────────────────
	s.winrmWasRunning = serviceRunning("WinRM")
	fmt.Println("  [winrm] Enabling PowerShell Remoting...")
	// -SkipNetworkProfileCheck avoids failures on public networks.
	ps(`Enable-PSRemoting -Force -SkipNetworkProfileCheck 2>$null`)

	return s, nil
}

// SSHPort returns the standard SSH port.
func (s *Session) SSHPort() int { return 22 }

// PrivateKeyPath returns the path to the temporary private key.
func (s *Session) PrivateKeyPath() string { return s.privateKeyPath }

// Username returns the Windows username for the SSH connection.
func (s *Session) Username() string { return s.username }

// Disable removes the temporary SSH key and restores the previous service state.
func (s *Session) Disable() {
	fmt.Println("\nCleaning up SSH/WinRM session...")

	// Remove temp key from authorized_keys.
	if s.authKeysPath != "" && s.pubKeyLine != "" {
		removeKeyLine(s.authKeysPath, s.pubKeyLine)
	}
	os.Remove(s.privateKeyPath)
	os.Remove(s.privateKeyPath + ".pub")

	// Stop sshd if we started it.
	if !s.sshdWasRunning {
		ps(`Stop-Service sshd -ErrorAction SilentlyContinue`)
	}

	// Disable WinRM if it wasn't running before.
	if !s.winrmWasRunning {
		ps(`Disable-PSRemoting -Force -ErrorAction SilentlyContinue; Stop-Service WinRM -ErrorAction SilentlyContinue`)
	}
}

// ── helpers ──────────────────────────────────────────────────────────────────

func ps(command string) error {
	return run("powershell", "-NoProfile", "-NonInteractive", "-Command", command)
}

func run(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func serviceRunning(name string) bool {
	out, err := exec.Command("powershell", "-NoProfile", "-NonInteractive",
		"-Command", fmt.Sprintf("(Get-Service '%s' -ErrorAction SilentlyContinue).Status", name)).Output()
	if err != nil {
		return false
	}
	return strings.TrimSpace(string(out)) == "Running"
}

func serviceExists(name string) bool {
	out, _ := exec.Command("powershell", "-NoProfile", "-NonInteractive",
		"-Command", fmt.Sprintf("Get-Service '%s' -ErrorAction SilentlyContinue | Measure-Object | Select-Object -ExpandProperty Count", name)).Output()
	return strings.TrimSpace(string(out)) == "1"
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
