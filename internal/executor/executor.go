// Package executor manages a persistent shell session on the remote system.
package executor

import (
	"bufio"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Result is the JSON-serialisable output of a single command execution.
type Result struct {
	Stdout     string `json:"stdout"`
	ExitCode   int    `json:"exit_code"`
	DurationMs int64  `json:"duration_ms"`
}

// proc holds a single live shell process and its I/O handles.
type proc struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	reader *bufio.Reader
	pr     *os.File
}

func (p *proc) kill() {
	p.stdin.Close()
	if p.cmd != nil && p.cmd.Process != nil {
		p.cmd.Process.Kill()
		p.cmd.Wait()
	}
	p.pr.Close()
}

// Session wraps a proc and serialises command execution.
// On timeout the proc is replaced so leaked goroutines cannot corrupt future calls.
type Session struct {
	mu    sync.Mutex
	isWin bool
	p     *proc
}

func NewSession() (*Session, error) {
	isWin := runtime.GOOS == "windows"
	p, err := startProc(isWin)
	if err != nil {
		return nil, err
	}
	return &Session{isWin: isWin, p: p}, nil
}

func startProc(isWin bool) (*proc, error) {
	var cmd *exec.Cmd
	if isWin {
		// Use PowerShell: no command echo, proper exit codes, modern Windows API.
		cmd = exec.Command("powershell.exe",
			"-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass")
	} else {
		cmd = exec.Command("/bin/bash", "--norc", "--noprofile")
	}

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("stdin pipe: %w", err)
	}

	pr, pw, err := os.Pipe()
	if err != nil {
		return nil, fmt.Errorf("output pipe: %w", err)
	}
	cmd.Stdout = pw
	cmd.Stderr = pw

	if err := cmd.Start(); err != nil {
		pw.Close()
		pr.Close()
		return nil, fmt.Errorf("start shell: %w", err)
	}
	pw.Close()

	p := &proc{
		cmd:    cmd,
		stdin:  stdin,
		reader: bufio.NewReaderSize(pr, 64*1024),
		pr:     pr,
	}

	// Write setup commands and drain any startup output via a known sentinel.
	drainSentinel := "###D613DRAIN###"
	if isWin {
		// PowerShell: write a sentinel to stdout and drain until we see it.
		// PowerShell -NonInteractive doesn't echo or show prompts, but there
		// can be a short splash on some versions.
		fmt.Fprintf(stdin, "Write-Host \"%s\"\n", drainSentinel)
	} else {
		fmt.Fprintln(stdin, "export PS1='' PS2=''")
		fmt.Fprintf(stdin, "echo '%s'\n", drainSentinel)
	}

	for {
		line, err := p.reader.ReadString('\n')
		if strings.Contains(line, drainSentinel) {
			break
		}
		if err != nil {
			break
		}
	}

	return p, nil
}

func marker() string {
	b := make([]byte, 8)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func (s *Session) Execute(command string, timeoutSec int) Result {
	s.mu.Lock()
	defer s.mu.Unlock()

	if timeoutSec <= 0 {
		timeoutSec = 60
	}

	m := marker()
	sentinel := "###D613:" + m + ":"
	start := time.Now()

	var script string
	if s.isWin {
		// PowerShell: merge all streams (*>&1), capture exit code carefully.
		// $LASTEXITCODE covers native executables; $? covers cmdlets.
		script = "try { " + command + " *>&1 } catch { Write-Output $_.Exception.Message }\n" +
			"$d613ec = if ($null -ne $LASTEXITCODE -and $LASTEXITCODE -ne 0) { $LASTEXITCODE } elseif (-not $?) { 1 } else { 0 }\n" +
			"Write-Host \"" + sentinel + "$d613ec###\"\n"
	} else {
		script = "{ " + command + "; } 2>&1\nprintf '\\n" + sentinel + "%d###\\n' $?\n"
	}
	fmt.Fprint(s.p.stdin, script)

	type readResult struct {
		out  string
		code int
	}
	ch := make(chan readResult, 1)
	currentProc := s.p

	go func() {
		var sb strings.Builder
		for {
			line, err := currentProc.reader.ReadString('\n')
			if idx := strings.Index(line, sentinel); idx >= 0 {
				rest := line[idx+len(sentinel):]
				rest = strings.TrimRight(rest, "#\r\n ")
				code, _ := strconv.Atoi(rest)
				ch <- readResult{strings.TrimRight(sb.String(), "\r\n"), code}
				return
			}
			sb.WriteString(line)
			if err != nil {
				ch <- readResult{strings.TrimRight(sb.String(), "\r\n"), 1}
				return
			}
		}
	}()

	select {
	case r := <-ch:
		return Result{
			Stdout:     r.out,
			ExitCode:   r.code,
			DurationMs: time.Since(start).Milliseconds(),
		}
	case <-time.After(time.Duration(timeoutSec) * time.Second):
		// Kill the proc — unblocks the reader goroutine via pipe EOF.
		currentProc.kill()
		// Start a fresh shell for subsequent commands.
		if fresh, err := startProc(s.isWin); err == nil {
			s.p = fresh
		}
		return Result{
			Stdout:     fmt.Sprintf("[command timed out after %ds — session reset]", timeoutSec),
			ExitCode:   124,
			DurationMs: time.Since(start).Milliseconds(),
		}
	}
}

func (s *Session) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.p.kill()
}
