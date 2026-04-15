// d613-agent — remote shell agent for D613 Labs AI troubleshooting.
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log"
	"os"
	"os/signal"
	"runtime"
	"syscall"

	"github.com/UnitedForceIT/d613-agent/internal/bore"
	"github.com/UnitedForceIT/d613-agent/internal/server"
	"github.com/UnitedForceIT/d613-agent/internal/sshsetup"
	"github.com/UnitedForceIT/d613-agent/internal/tunnel"
)

var version = "dev"

func genToken() string {
	b := make([]byte, 24)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func main() {
	log.SetFlags(0)
	fmt.Printf("D613 Labs Remote Agent  v%s\n", version)
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")

	token := genToken()

	// ── SSH + WinRM (Windows only) ────────────────────────────────────────
	var sshSession *sshsetup.Session
	var boreTunnel *bore.Tunnel
	if runtime.GOOS == "windows" {
		fmt.Println("Enabling SSH and PowerShell Remoting for this session...")
		var err error
		sshSession, err = sshsetup.Enable()
		if err != nil {
			fmt.Printf("  [warn] SSH/WinRM setup failed: %v (continuing without it)\n", err)
		} else {
			fmt.Println("  SSH and WinRM enabled.")
			fmt.Println("  Opening SSH tunnel via bore...")
			boreTunnel, err = bore.Start(sshSession.SSHPort())
			if err != nil {
				fmt.Printf("  [warn] SSH tunnel failed: %v (SSH only reachable on LAN)\n", err)
			}
		}
	}

	// ── HTTP exec server ──────────────────────────────────────────────────
	fmt.Println("Starting shell session...")
	srv, err := server.New(token)
	if err != nil {
		log.Fatalf("ERROR: could not start server: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() {
		if err := srv.Start(ctx); err != nil && ctx.Err() == nil {
			log.Printf("server error: %v", err)
		}
	}()

	// ── Cloudflare HTTP tunnel ────────────────────────────────────────────
	fmt.Println("Opening Cloudflare tunnel...")
	t, err := tunnel.Start(srv.Port)
	if err != nil {
		log.Fatalf("ERROR: tunnel failed: %v", err)
	}
	defer t.Stop()

	// ── Print session info ────────────────────────────────────────────────
	fmt.Println()
	fmt.Println("┌─────────────────────────────────────────────────────────────┐")
	fmt.Println("│                     SESSION READY                           │")
	fmt.Println("├─────────────────────────────────────────────────────────────┤")
	fmt.Printf( "│  HTTP URL: %-50s│\n", t.URL)
	fmt.Printf( "│  Token:    %-50s│\n", token)
	fmt.Println("├─────────────────────────────────────────────────────────────┤")
	fmt.Println("│  Connect from your Mac (Claude Code proxy):                 │")
	fmt.Printf( "│  d613-connect \"%s\"\n", t.URL)
	fmt.Printf( "│               \"%s\"\n", token)

	if sshSession != nil {
		fmt.Println("├─────────────────────────────────────────────────────────────┤")
		fmt.Println("│  SSH access (direct shell, for this session only):          │")
		if boreTunnel != nil {
			fmt.Printf("│  Host:  %-53s│\n", fmt.Sprintf("%s:%d", boreTunnel.Host, boreTunnel.Port))
			fmt.Printf("│  User:  %-53s│\n", sshSession.Username())
			fmt.Printf("│  Key:   (printed at agent startup, saved to TEMP)           │")
			fmt.Printf("\n│\n│  ssh -i \"%s\" -p %d %s@bore.pub\n",
				sshSession.PrivateKeyPath(), boreTunnel.Port, sshSession.Username())
		} else {
			fmt.Printf("│  ssh %s@<this-machine-ip>  (LAN only — bore tunnel failed)  │\n",
				sshSession.Username())
		}
		fmt.Println("├─────────────────────────────────────────────────────────────┤")
		fmt.Println("│  PowerShell Remoting (WinRM) also enabled on this session.  │")
		fmt.Printf( "│  Enter-PSSession -ComputerName <IP> -Credential %s\n",
			sshSession.Username())
	}

	fmt.Println("└─────────────────────────────────────────────────────────────┘")
	fmt.Println()
	fmt.Println("Session is live. Press Ctrl+C to end (SSH/WinRM will be disabled).")

	// ── Wait for shutdown ─────────────────────────────────────────────────
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig

	fmt.Println("\nShutting down...")
	cancel()
	if boreTunnel != nil {
		boreTunnel.Stop()
	}
	if sshSession != nil {
		sshSession.Disable()
	}
}
