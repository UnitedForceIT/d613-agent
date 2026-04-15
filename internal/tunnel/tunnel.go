// Package tunnel manages a Cloudflare Quick Tunnel session.
// No Cloudflare account is required — Quick Tunnels are completely anonymous
// and provide a random HTTPS URL valid for the lifetime of the process.
package tunnel

import (
	"archive/tar"
	"bufio"
	"compress/gzip"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"time"
)

// assetName returns the GitHub release asset filename for the current platform.
// macOS releases are .tgz archives; Windows is .exe; Linux is a plain binary.
func assetName() string {
	arch := "amd64"
	if runtime.GOARCH == "arm64" {
		arch = "arm64"
	}
	switch runtime.GOOS {
	case "windows":
		return "cloudflared-windows-amd64.exe"
	case "darwin":
		return fmt.Sprintf("cloudflared-darwin-%s.tgz", arch)
	default: // linux
		return fmt.Sprintf("cloudflared-linux-%s", arch)
	}
}

// cachedBinaryPath returns the local path we store the extracted binary at.
func cachedBinaryPath() string {
	name := "d613-cloudflared"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	return filepath.Join(os.TempDir(), name)
}

// ensureCloudflared downloads (and extracts if needed) cloudflared into the OS
// temp directory, returning the path to the executable.
func ensureCloudflared() (string, error) {
	dest := cachedBinaryPath()

	if info, err := os.Stat(dest); err == nil && info.Size() > 0 {
		return dest, nil // already cached
	}

	asset := assetName()
	fmt.Printf("  Downloading cloudflared (%s)...\n", asset)
	url := "https://github.com/cloudflare/cloudflared/releases/latest/download/" + asset

	resp, err := http.Get(url) //nolint:gosec
	if err != nil {
		return "", fmt.Errorf("download cloudflared: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("cloudflared download returned HTTP %d", resp.StatusCode)
	}

	if runtime.GOOS == "darwin" {
		// macOS asset is a .tgz — extract the "cloudflared" binary from it.
		if err := extractTGZ(resp.Body, dest); err != nil {
			os.Remove(dest)
			return "", fmt.Errorf("extract cloudflared: %w", err)
		}
	} else {
		// Windows .exe and Linux plain binary — write directly.
		f, err := os.OpenFile(dest, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0755)
		if err != nil {
			return "", fmt.Errorf("create cloudflared binary: %w", err)
		}
		if _, err := io.Copy(f, resp.Body); err != nil {
			f.Close()
			os.Remove(dest)
			return "", fmt.Errorf("write cloudflared binary: %w", err)
		}
		f.Close()
	}

	return dest, nil
}

// extractTGZ finds the first regular file inside a .tgz stream and writes it
// to dest with executable permissions.
func extractTGZ(r io.Reader, dest string) error {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return err
	}
	defer gz.Close()

	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		if hdr.Typeflag != tar.TypeReg {
			continue
		}
		// Write the first regular file (the cloudflared binary).
		f, err := os.OpenFile(dest, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0755)
		if err != nil {
			return err
		}
		_, err = io.Copy(f, tr)
		f.Close()
		return err
	}
	return fmt.Errorf("no regular file found in archive")
}

// Tunnel represents a live Cloudflare Quick Tunnel.
type Tunnel struct {
	// URL is the public HTTPS address, e.g. https://random-name.trycloudflare.com
	URL string
	cmd *exec.Cmd
}

// Start downloads (if needed) and launches cloudflared, waits for the public
// URL to appear in its output, then returns.
func Start(localPort int) (*Tunnel, error) {
	cfPath, err := ensureCloudflared()
	if err != nil {
		return nil, err
	}

	cmd := exec.Command(cfPath,
		"tunnel",
		"--url", fmt.Sprintf("http://localhost:%d", localPort),
		"--no-autoupdate",
	)

	// cloudflared writes the URL to stderr.
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start cloudflared: %w", err)
	}

	urlRe := regexp.MustCompile(`https://[a-zA-Z0-9\-]+\.trycloudflare\.com`)
	urlCh := make(chan string, 1)

	go func() {
		scanner := bufio.NewScanner(stderr)
		for scanner.Scan() {
			if m := urlRe.FindString(scanner.Text()); m != "" {
				urlCh <- m
				// Drain the rest so cloudflared doesn't block on a full pipe.
				for scanner.Scan() {
				}
				return
			}
		}
		close(urlCh) // process exited without printing a URL
	}()

	select {
	case url, ok := <-urlCh:
		if !ok {
			cmd.Process.Kill()
			return nil, fmt.Errorf("cloudflared exited before providing a URL")
		}
		return &Tunnel{URL: url, cmd: cmd}, nil
	case <-time.After(30 * time.Second):
		cmd.Process.Kill()
		return nil, fmt.Errorf("timed out waiting for tunnel URL (30s)")
	}
}

// Stop terminates the cloudflared process and releases the tunnel.
func (t *Tunnel) Stop() {
	if t.cmd != nil && t.cmd.Process != nil {
		t.cmd.Process.Kill()
		t.cmd.Wait()
	}
}
