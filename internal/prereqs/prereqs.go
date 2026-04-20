// Package prereqs checks for and installs Node.js and Claude Code CLI.
package prereqs

import (
	"fmt"
	"os/exec"
	"runtime"
	"strings"
)

// State tracks what this package installed so it can be cleaned up.
type State struct {
	InstalledNode bool
}

// Ensure checks for Node.js (required for npx to run Claude Code) and installs
// it if missing. Claude Code itself is NOT pre-installed — it will be invoked
// via `npx -y @anthropic-ai/claude-code` at session time, which is ephemeral,
// requires no cleanup, and always uses the latest version.
func Ensure() (*State, error) {
	s := &State{}

	// ── Node.js + npm ──────────────────────────────────────────────────────
	if !hasCommand("node") || !hasCommand("npx") {
		fmt.Println("  Node.js not found — installing...")
		if err := installNode(); err != nil {
			return nil, fmt.Errorf("install Node.js: %w", err)
		}
		s.InstalledNode = true
		fmt.Println("  Node.js installed.")
	} else {
		fmt.Println("  Node.js found.")
	}

	// Verify npx is available (ships with npm 5+)
	if !hasCommand("npx") {
		return nil, fmt.Errorf("npx not found — Node.js install may be incomplete")
	}
	fmt.Println("  npx available — Claude Code will run via npx (no global install needed).")

	return s, nil
}

// Cleanup removes Node.js if we installed it. Claude Code doesn't need cleanup
// since it was run via npx (ephemeral).
func (s *State) Cleanup() {
	if s == nil {
		return
	}
	if s.InstalledNode {
		fmt.Println("  Removing Node.js...")
		uninstallNode()
	}
}

func hasCommand(name string) bool {
	_, err := exec.LookPath(name)
	return err == nil
}

func run(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s: %w\n%s", name, err, strings.TrimSpace(string(out)))
	}
	return nil
}

func installNode() error {
	switch runtime.GOOS {
	case "windows":
		return installNodeWindows()
	case "darwin":
		return installNodeMac()
	default:
		return installNodeLinux()
	}
}

func uninstallNode() {
	switch runtime.GOOS {
	case "windows":
		// Best-effort: uninstall via winget
		run("winget", "uninstall", "-e", "--id", "OpenJS.NodeJS", "--silent")
	}
	// On Unix we used the system package manager; don't auto-remove.
}

func installNodeWindows() error {
	// Try winget first (available on Windows 10+)
	if hasCommand("winget") {
		if err := run("winget", "install", "-e", "--id", "OpenJS.NodeJS.LTS",
			"--silent", "--accept-source-agreements", "--accept-package-agreements"); err == nil {
			return nil
		}
	}
	// Fallback: download LTS MSI directly
	return fmt.Errorf("winget not available; please install Node.js from https://nodejs.org")
}

func installNodeMac() error {
	if hasCommand("brew") {
		return run("brew", "install", "node")
	}
	return fmt.Errorf("brew not available; please install Node.js from https://nodejs.org")
}

func installNodeLinux() error {
	if hasCommand("apt-get") {
		run("apt-get", "update", "-qq")
		return run("apt-get", "install", "-y", "nodejs", "npm")
	}
	if hasCommand("dnf") {
		return run("dnf", "install", "-y", "nodejs")
	}
	if hasCommand("yum") {
		return run("yum", "install", "-y", "nodejs")
	}
	return fmt.Errorf("no supported package manager found; please install Node.js from https://nodejs.org")
}
