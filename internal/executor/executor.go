// Package executor manages a persistent shell session on the remote system.
// All commands from a single agent session run through the same shell process,
// so working directory, environment variables, and shell state persist between
// commands — exactly like an interactive SSH session.
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
	pr     *os.File // read end of the combined stdout+stderr pipe
}

// kill terminates the shell process and closes the pipe.
func (p *proc) kill() {
	p.stdin.Close()
	if p.cmd != nil && p.cmd.Process != nil {
		p.cmd.Process.Kill()
		p.cmd.Wait()
	}
	p.pr.Close()
}

// Session wraps a proc and serialises command execution through a mutex.
// If a command times out, the underlying proc is replaced with a fresh one
// so subsequent commands are never affected by a leaked reader goroutine.
type Session struct {
	mu    sync.Mutex
	isWin bool
	p     *proc
}

// NewSession starts the underlying shell and returns a ready Session.
func NewSession() (*Session, error) {
	isWin := runtime.GOOS == "windows"
	p, err := startProc(isWin)
	if err != nil {
		return nil, err
	}
	return &Session{isWin: isWin, p: p}, nil
}

// startProc launches a new shell process, configures it, and drains its
// startup noise so the first Execute call sees a clean pipe.
func startProc(isWin bool) (*proc, error) {
	var cmd *exec.Cmd
	if isWin {
		cmd = exec.Command("cmd.exe")
	} else {
		cmd = exec.Command("/bin/bash", "--norc", "--noprofile")
	}

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("stdin pipe: %w", err)
	}

	// Combine stdout + stderr into a single reader via an OS pipe.
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
	pw.Close() // close write-end in parent so reads on pr eventually get EOF

	p := &proc{
		cmd:    cmd,
		stdin:  stdin,
		reader: bufio.NewReaderSize(pr, 64*1024),
		pr:     pr,
	}

	// Configure the shell and drain startup noise so Execute sees a clean pipe.
	if isWin {
		// Turn off command echo and simplify the prompt.
		// Then write a known drain sentinel and read until we see its output —
		// this discards the Windows copyright banner and the echoed setup lines.
		drainSentinel := "###D613DRAIN###"
		fmt.Fprint(stdin, "@echo off\r\nprompt $\r\necho "+drainSentinel+"\r\n")
		for {
			line, err := p.reader.ReadString('\n')
			if strings.Contains(line, drainSentinel) {
				break
			}
			if err != nil {
				break
			}
		}
	} else {
		// Suppress the bash prompt so it never appears in command output.
		fmt.Fprintln(stdin, "export PS1='' PS2=''")
	}

	return p, nil
}

// marker returns a random 16-char hex string used as a per-command sentinel.
func marker() string {
	b := make([]byte, 8)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// Execute runs command in the persistent shell and returns combined
// stdout/stderr, exit code, and wall-clock duration.
//
// If the command times out, the underlying shell process is replaced so that
// the leaked reader goroutine cannot corrupt subsequent calls.
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
		// Capture ERRORLEVEL in a variable immediately after the command so
		// that cmd.exe does not expand it at parse-time of the echo line.
		script = command + " 2>&1\r\n" +
			"set D613_EC=%ERRORLEVEL%\r\n" +
			"echo " + sentinel + "%D613_EC%###\r\n"
	} else {
		script = "{ " + command + "; } 2>&1\nprintf '\\n" + sentinel + "%d###\\n' $?\n"
	}
	fmt.Fprint(s.p.stdin, script)

	type readResult struct {
		out  string
		code int
	}
	ch := make(chan readResult, 1)
	// Capture the current proc so the goroutine always refers to the proc we
	// just wrote to, even if s.p gets replaced by a reset.
	currentProc := s.p

	go func() {
		var sb strings.Builder
		for {
			line, err := currentProc.reader.ReadString('\n')
			if idx := strings.Index(line, sentinel); idx >= 0 {
				rest := line[idx+len(sentinel):]
				rest = strings.TrimRight(rest, "#\r\n ")
				code, _ := strconv.Atoi(rest)
				ch <- readResult{strings.TrimRight(sb.String(), "\n"), code}
				return
			}
			sb.WriteString(line)
			if err != nil {
				// Pipe closed — shell likely exited or was killed.
				ch <- readResult{strings.TrimRight(sb.String(), "\n"), 1}
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
		// Kill the current proc — this closes the pipe and unblocks the reader
		// goroutine above (it will get an EOF error and exit cleanly).
		currentProc.kill()
		// Start a fresh shell for the next command.
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

// Close terminates the underlying shell process.
func (s *Session) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.p.kill()
}
