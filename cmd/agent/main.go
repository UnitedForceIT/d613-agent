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
	"github.com/UnitedForceIT/d613-agent/internal/defender"
	"github.com/UnitedForceIT/d613-agent/internal/prereqs"
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

	// ── Windows Defender exclusion for our temp dir (best-effort) ─────────
	// Without this, bore.exe gets quarantined as "potentially unwanted software".
	if runtime.GOOS == "windows" {
		defender.AddTempExclusion()
	}

	// ── Prerequisites (Node.js + Claude Code) ─────────────────────────────
	fmt.Println("Checking prerequisites...")
	prereqState, err := prereqs.Ensure()
	if err != nil {
		fmt.Printf("  [warn] Prerequisites check failed: %v\n", err)
		fmt.Println("  Continuing — admin will need Claude Code installed separately.")
	}

	// ── SSH setup (Windows/macOS/Linux) ───────────────────────────────────
	var sshSession *sshsetup.Session
	var boreTunnel *bore.Tunnel
	srvCfg := server.Config{}

	fmt.Println("Enabling SSH for this session...")
	sshSession, err = sshsetup.Enable()
	if err != nil {
		fmt.Printf("  [warn] SSH setup failed: %v (continuing without it)\n", err)
		if u := os.Getenv("USER"); u != "" {
			srvCfg.SSHUser = u
		}
	} else {
		fmt.Println("  SSH enabled.")
		srvCfg.SSHKeyPath = sshSession.PrivateKeyPath()
		srvCfg.SSHUser = sshSession.Username()

		fmt.Println("  Opening SSH tunnel via bore...")
		boreTunnel, err = bore.Start(sshSession.SSHPort())
		if err != nil {
			fmt.Printf("  [warn] SSH tunnel failed: %v (SSH only reachable on LAN)\n", err)
		} else {
			srvCfg.BorePath = fmt.Sprintf("%s:%d", boreTunnel.Host, boreTunnel.Port)
		}
	}

	// ── HTTP server ───────────────────────────────────────────────────────
	fmt.Println("Starting agent...")
	srv, err := server.New(token, srvCfg)
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
	fmt.Printf( "│  URL:   %-52s│\n", t.URL)
	fmt.Printf( "│  Token: %-52s│\n", token)
	fmt.Println("├─────────────────────────────────────────────────────────────┤")
	fmt.Println("│  Run this on your admin machine:                            │")
	fmt.Printf( "│  d613-connect \"%s\"\n", t.URL)
	fmt.Printf( "│               \"%s\"\n", token)

	if sshSession != nil {
		fmt.Println("├─────────────────────────────────────────────────────────────┤")
		if boreTunnel != nil {
			fmt.Printf("│  SSH (via bore): ssh -p %d %s@bore.pub\n",
				boreTunnel.Port, sshSession.Username())
		} else {
			fmt.Printf("│  SSH (LAN only): ssh %s@<this-machine-ip>\n",
				sshSession.Username())
		}
	}

	fmt.Println("└─────────────────────────────────────────────────────────────┘")
	fmt.Println()
	fmt.Println("Waiting for admin connection. Press Ctrl+C to end session.")

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
		fmt.Println("  Disabling SSH and WinRM...")
		sshSession.Disable()
	}
	if prereqState != nil {
		prereqState.Cleanup()
	}
	fmt.Println("Session ended. All temporary access disabled.")
}
