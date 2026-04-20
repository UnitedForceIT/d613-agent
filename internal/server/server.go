// Package server exposes the agent's HTTP API.
//
// Endpoints:
//   GET  /ping       — unauthenticated health check
//   POST /exec       — run a shell command (auth required)
//   POST /auth       — receive Claude OAuth token (auth required)
//   GET  /ssh-key    — retrieve temporary SSH private key (auth required)
//   GET  /info       — session metadata (auth required)
package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/UnitedForceIT/d613-agent/internal/executor"
)

// Server owns the HTTP listener and the persistent shell session.
type Server struct {
	token      string
	Port       int
	session    *executor.Session
	ln         net.Listener
	mu         sync.RWMutex
	oauthToken string
	sshKeyPath string // path to private key file on disk (optional)
	sshUser    string
	borePath   string // "bore.pub:PORT" or "" if unavailable
}

// Config holds optional extra metadata the server can expose.
type Config struct {
	SSHKeyPath string // path to temp SSH private key (may be empty)
	SSHUser    string // SSH username on this machine
	BorePath   string // "bore.pub:PORT" or ""
}

// New creates a Session and binds a random localhost port.
func New(token string, cfg Config) (*Server, error) {
	session, err := executor.NewSession()
	if err != nil {
		return nil, fmt.Errorf("create shell session: %w", err)
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("bind listener: %w", err)
	}

	return &Server{
		token:      token,
		Port:       ln.Addr().(*net.TCPAddr).Port,
		session:    session,
		ln:         ln,
		sshKeyPath: cfg.SSHKeyPath,
		sshUser:    cfg.SSHUser,
		borePath:   cfg.BorePath,
	}, nil
}

// requireAuth is a middleware that validates the Bearer token.
func (s *Server) requireAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		tok := strings.TrimPrefix(auth, "Bearer ")
		if tok != s.token {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			fmt.Fprint(w, `{"error":"unauthorized"}`)
			return
		}
		next(w, r)
	}
}

// execRequest is the JSON body for POST /exec.
type execRequest struct {
	Command string `json:"command"`
	Timeout int    `json:"timeout"` // seconds; 0 → default (60 s)
}

func (s *Server) handleExec(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req execRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Command == "" {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, `{"error":"command field required"}`)
		return
	}

	result := s.session.Execute(req.Command, req.Timeout)

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(result)
}

func (s *Server) handlePing(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{
		"status": "ok",
		"time":   time.Now().UTC().Format(time.RFC3339),
	})
}

func (s *Server) handleInfo(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{
		"status":      "ok",
		"version":     "0.2.0",
		"ssh_user":    s.sshUser,
		"bore_path":   s.borePath,
		"has_ssh_key": fmt.Sprintf("%v", s.sshKeyPath != ""),
		"has_auth":    fmt.Sprintf("%v", s.oauthToken != ""),
	})
}

// authRequest is the JSON body for POST /auth.
type authRequest struct {
	OAuthToken string `json:"oauth_token"`
}

func (s *Server) handleAuth(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req authRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.OAuthToken == "" {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, `{"error":"oauth_token field required"}`)
		return
	}
	s.mu.Lock()
	s.oauthToken = req.OAuthToken
	s.mu.Unlock()
	log.Printf("[agent] Claude OAuth token received and stored")
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"status":   "ok",
		"ssh_user": s.sshUser,
		"bore":     s.borePath,
	})
}

func (s *Server) handleSSHKey(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	keyPath := s.sshKeyPath
	s.mu.RUnlock()
	if keyPath == "" {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprint(w, `{"error":"no SSH key available"}`)
		return
	}
	data, err := os.ReadFile(keyPath)
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprintf(w, `{"error":"%v"}`, err)
		return
	}
	w.Header().Set("Content-Type", "text/plain")
	w.Write(data)
}

// OAuthToken returns the stored Claude OAuth token (empty string if not yet set).
func (s *Server) OAuthToken() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.oauthToken
}

// handleSSHTunnel hijacks the HTTP connection and proxies raw TCP bytes
// between the client and the local SSH daemon on port 22. This lets
// d613-connect reach the remote SSH daemon over the Cloudflare HTTPS
// tunnel even when bore/alternative tunnels are unavailable.
func (s *Server) handleSSHTunnel(w http.ResponseWriter, r *http.Request) {
	// The client is expected to have sent the Authorization header and we've
	// already validated it via requireAuth. Now we hijack.
	hijacker, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "hijacking not supported", http.StatusInternalServerError)
		return
	}

	sshTarget := "127.0.0.1:22"
	if v := r.URL.Query().Get("port"); v != "" {
		sshTarget = "127.0.0.1:" + v
	}

	// Connect to the local SSH daemon first, so we can fail cleanly if it's down.
	backend, err := net.DialTimeout("tcp", sshTarget, 5*time.Second)
	if err != nil {
		http.Error(w, fmt.Sprintf("connect to sshd: %v", err), http.StatusBadGateway)
		return
	}

	// Write 200 Connection Established before hijacking.
	w.WriteHeader(http.StatusOK)
	// Flush if possible (prevents hijacked conn from missing our response).
	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}

	clientConn, bufrw, err := hijacker.Hijack()
	if err != nil {
		backend.Close()
		return
	}
	defer clientConn.Close()
	defer backend.Close()

	// Copy any buffered data first (HTTP body already read into buffer).
	if bufrw.Reader.Buffered() > 0 {
		buf := make([]byte, bufrw.Reader.Buffered())
		bufrw.Read(buf)
		backend.Write(buf)
	}

	// Bidirectional copy. Closes when either side closes.
	done := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(backend, clientConn); done <- struct{}{} }()
	go func() { _, _ = io.Copy(clientConn, backend); done <- struct{}{} }()
	<-done
}

// Start registers routes and begins serving.  It blocks until ctx is cancelled.
func (s *Server) Start(ctx context.Context) error {
	mux := http.NewServeMux()
	mux.HandleFunc("/ping", s.handlePing)
	mux.HandleFunc("/info", s.requireAuth(s.handleInfo))
	mux.HandleFunc("/exec", s.requireAuth(s.handleExec))
	mux.HandleFunc("/auth", s.requireAuth(s.handleAuth))
	mux.HandleFunc("/ssh-key", s.requireAuth(s.handleSSHKey))
	mux.HandleFunc("/ssh-tunnel", s.requireAuth(s.handleSSHTunnel))

	srv := &http.Server{
		Handler:      mux,
		ReadTimeout:  10 * time.Minute,
		WriteTimeout: 10 * time.Minute,
		IdleTimeout:  5 * time.Minute,
	}

	go func() {
		<-ctx.Done()
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(shutCtx)
		s.session.Close()
	}()

	log.Printf("[agent] listening on 127.0.0.1:%d", s.Port)
	return srv.Serve(s.ln)
}
