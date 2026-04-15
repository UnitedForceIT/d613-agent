// Package bore downloads and manages a bore TCP tunnel.
// bore (https://github.com/ekzhang/bore) provides free, anonymous TCP tunnels
// via bore.pub with no account or configuration required.
package bore

import (
	"archive/tar"
	"archive/zip"
	"bufio"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"
)

// Tunnel represents a live bore TCP tunnel.
type Tunnel struct {
	// Host is always "bore.pub"
	Host string
	// Port is the remote port assigned by bore.pub
	Port int
	cmd  *exec.Cmd
}

// Stop terminates the bore process.
func (t *Tunnel) Stop() {
	if t.cmd != nil && t.cmd.Process != nil {
		t.cmd.Process.Kill()
		t.cmd.Wait()
	}
}

// Start downloads bore if needed, then opens a TCP tunnel to localPort.
func Start(localPort int) (*Tunnel, error) {
	borePath, err := ensureBore()
	if err != nil {
		return nil, err
	}

	cmd := exec.Command(borePath, "local", fmt.Sprintf("%d", localPort), "--to", "bore.pub")

	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start bore: %w", err)
	}

	// bore prints: "listening at bore.pub:PORT"
	portRe := regexp.MustCompile(`bore\.pub:(\d+)`)
	portCh := make(chan int, 1)

	go func() {
		scanner := bufio.NewScanner(stderr)
		for scanner.Scan() {
			line := scanner.Text()
			if m := portRe.FindStringSubmatch(line); m != nil {
				var port int
				fmt.Sscanf(m[1], "%d", &port)
				portCh <- port
				for scanner.Scan() {}
				return
			}
		}
		close(portCh)
	}()

	select {
	case port, ok := <-portCh:
		if !ok {
			cmd.Process.Kill()
			return nil, fmt.Errorf("bore exited before providing a port")
		}
		return &Tunnel{Host: "bore.pub", Port: port, cmd: cmd}, nil
	case <-time.After(20 * time.Second):
		cmd.Process.Kill()
		return nil, fmt.Errorf("timed out waiting for bore tunnel")
	}
}

func latestBoreVersion() (string, error) {
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Get("https://api.github.com/repos/ekzhang/bore/releases/latest")
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var rel struct {
		TagName string `json:"tag_name"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&rel); err != nil {
		return "", err
	}
	return strings.TrimPrefix(rel.TagName, "v"), nil
}

func boreAssetName(version string) string {
	arch := "x86_64"
	if runtime.GOARCH == "arm64" {
		arch = "aarch64"
	}
	switch runtime.GOOS {
	case "windows":
		return fmt.Sprintf("bore-v%s-x86_64-pc-windows-msvc.zip", version)
	case "darwin":
		return fmt.Sprintf("bore-v%s-%s-apple-darwin.tar.gz", version, arch)
	default:
		return fmt.Sprintf("bore-v%s-%s-unknown-linux-musl.tar.gz", version, arch)
	}
}

func ensureBore() (string, error) {
	name := "bore"
	if runtime.GOOS == "windows" {
		name = "bore.exe"
	}
	dest := filepath.Join(os.TempDir(), "d613-"+name)

	if info, err := os.Stat(dest); err == nil && info.Size() > 0 {
		return dest, nil
	}

	version, err := latestBoreVersion()
	if err != nil {
		version = "0.5.0" // fallback
	}

	asset := boreAssetName(version)
	fmt.Printf("  Downloading bore (%s)...\n", asset)
	url := fmt.Sprintf("https://github.com/ekzhang/bore/releases/download/v%s/%s", version, asset)

	resp, err := http.Get(url) //nolint:gosec
	if err != nil {
		return "", fmt.Errorf("download bore: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("bore download returned HTTP %d", resp.StatusCode)
	}

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}

	if strings.HasSuffix(asset, ".zip") {
		if err := extractFromZip(data, dest); err != nil {
			return "", err
		}
	} else {
		if err := extractFromTGZ(bytes.NewReader(data), dest); err != nil {
			return "", err
		}
	}

	return dest, nil
}

func extractFromTGZ(r io.Reader, dest string) error {
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
		if hdr.Typeflag == tar.TypeReg && strings.HasSuffix(hdr.Name, "bore") {
			f, err := os.OpenFile(dest, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0755)
			if err != nil {
				return err
			}
			_, err = io.Copy(f, tr)
			f.Close()
			return err
		}
	}
	return fmt.Errorf("bore binary not found in archive")
}

func extractFromZip(data []byte, dest string) error {
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return err
	}
	for _, f := range zr.File {
		if strings.HasSuffix(f.Name, "bore.exe") || strings.HasSuffix(f.Name, "bore") {
			rc, err := f.Open()
			if err != nil {
				return err
			}
			out, err := os.OpenFile(dest, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0755)
			if err != nil {
				rc.Close()
				return err
			}
			_, err = io.Copy(out, rc)
			rc.Close()
			out.Close()
			return err
		}
	}
	return fmt.Errorf("bore.exe not found in zip")
}
