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

// Session holds a long-lived shell process.  All Execute calls are serialised
// through a mutex so that output from concurrent requests cannot interleave.
type Session struct {
	mu     sync.Mutex
	stdin  io.WriteCloser
	reader *bufio.Reader
	cmd    *exec.Cmd
	isWin  bool
}

// NewSession starts the underlying shell and returns a ready Session.
func NewSession() (*Session, error) {
	isWin := runtime.GOOS == "windows"

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

	s := &Session{
		stdin:  stdin,
		reader: bufio.NewReaderSize(pr, 64*1024),
		cmd:    cmd,
		isWin:  isWin,
	}

	// Suppress interactive prompts so they don't pollute command output.
	if !isWin {
		fmt.Fprintln(stdin, "export PS1='' PS2=''")
	}

	return s, nil
}

// marker returns a random hex string used to delimit command output.
func marker() string {
	b := make([]byte, 8)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// Execute runs command in the persistent shell and returns the combined
// stdout/stderr, exit code, and wall-clock duration.  Execution is bounded
// by timeoutSec (default 60 s).
func (s *Session) Execute(command string, timeoutSec int) Result {
	s.mu.Lock()
	defer s.mu.Unlock()

	if timeoutSec <= 0 {
		timeoutSec = 60
	}

	m := marker()
	sentinel := "###D613:" + m + ":"
	start := time.Now()

	// Wrap the user command so we always print a sentinel containing the exit
	// code regardless of whether the command succeeds or fails.
	var script string
	if s.isWin {
		// cmd.exe: 2>&1 redirect, then echo sentinel on its own line.
		script = command + " 2>&1\r\necho " + sentinel + "%ERRORLEVEL%###\r\n"
	} else {
		// bash: run in a subshell, merge stderr, then printf sentinel.
		script = "{ " + command + "; } 2>&1\nprintf '\\n" + sentinel + "%d###\\n' $?\n"
	}
	fmt.Fprint(s.stdin, script)

	type readResult struct {
		out  string
		code int
	}
	ch := make(chan readResult, 1)

	go func() {
		var sb strings.Builder
		for {
			line, err := s.reader.ReadString('\n')
			if idx := strings.Index(line, sentinel); idx >= 0 {
				// Sentinel found — parse exit code from trailing "###".
				rest := line[idx+len(sentinel):]
				rest = strings.TrimRight(rest, "#\r\n ")
				code, _ := strconv.Atoi(rest)
				ch <- readResult{strings.TrimRight(sb.String(), "\n"), code}
				return
			}
			sb.WriteString(line)
			if err != nil {
				// Pipe closed — shell likely exited.
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
		return Result{
			Stdout:     fmt.Sprintf("[command timed out after %ds]", timeoutSec),
			ExitCode:   124,
			DurationMs: time.Since(start).Milliseconds(),
		}
	}
}

// Close terminates the underlying shell process.
func (s *Session) Close() {
	s.stdin.Close()
	if s.cmd.Process != nil {
		s.cmd.Process.Kill()
	}
	s.cmd.Wait()
}
