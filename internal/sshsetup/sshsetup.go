//go:build !windows

// Package sshsetup is a no-op on non-Windows platforms.
package sshsetup

import "fmt"

type Session struct{}

func Enable() (*Session, error) {
	return nil, fmt.Errorf("sshsetup only supported on Windows")
}

func (s *Session) SSHPort() int      { return 0 }
func (s *Session) PrivateKeyPath() string { return "" }
func (s *Session) Username() string       { return "" }
func (s *Session) Disable()               {}
