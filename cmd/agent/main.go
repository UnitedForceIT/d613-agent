// d613-agent — remote shell agent for D613 Labs AI troubleshooting.
//
// Run this binary on any machine (Windows/Mac/Linux) that needs to be
// accessed remotely.  It will:
//
//  1. Start a persistent shell session on this machine.
//  2. Bind a local HTTP server on a random port.
//  3. Download cloudflared (if not already cached) and open a Cloudflare
//     Quick Tunnel — no account or configuration required.
//  4. Print the public HTTPS URL and a one-time auth token.
//
// On your own machine, run:
//
//	d613-connect <URL> <TOKEN>
//
// which launches Claude Code with all shell commands transparently forwarded
// to this machine.
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/UnitedForceIT/d613-agent/internal/server"
	"github.com/UnitedForceIT/d613-agent/internal/tunnel"
)

var version = "dev"

func genToken() string {
	b := make([]byte, 24)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func main() {
	log.SetFlags(0) // no timestamp prefix in log output
	fmt.Printf("D613 Labs Remote Agent  v%s\n", version)
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")

	token := genToken()

	fmt.Println("Starting shell session...")
	srv, err := server.New(token)
	if err != nil {
		log.Fatalf("ERROR: could not start server: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Run HTTP server in background.
	go func() {
		if err := srv.Start(ctx); err != nil && ctx.Err() == nil {
			log.Printf("server error: %v", err)
		}
	}()

	fmt.Println("Opening Cloudflare tunnel (no account required)...")
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
	fmt.Printf( "│  URL:   %-53s│\n", t.URL)
	fmt.Printf( "│  Token: %-53s│\n", token)
	fmt.Println("├─────────────────────────────────────────────────────────────┤")
	fmt.Println("│  Run this on YOUR machine:                                  │")
	fmt.Println("│                                                             │")
	fmt.Printf( "│  d613-connect \"%s\"\n", t.URL)
	fmt.Printf( "│               \"%s\"\n", token)
	fmt.Println("│                                                             │")
	fmt.Println("│  — or use the one-liner install on your machine:            │")
	fmt.Println("│    curl -fsSL <connect-install-url> | bash                  │")
	fmt.Println("└─────────────────────────────────────────────────────────────┘")
	fmt.Println()
	fmt.Println("Session is live.  Press Ctrl+C to end.")

	// ── Wait for shutdown ─────────────────────────────────────────────────
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig

	fmt.Println("\nShutting down session...")
	cancel()
}
