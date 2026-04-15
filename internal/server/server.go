// Package server exposes the agent's HTTP API.
//
// Endpoints:
//   GET  /ping       — unauthenticated health check
//   POST /exec       — run a shell command (auth required)
//   GET  /info       — session metadata (auth required)
package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/UnitedForceIT/d613-agent/internal/executor"
)

// Server owns the HTTP listener and the persistent shell session.
type Server struct {
	token   string
	Port    int
	session *executor.Session
	ln      net.Listener
}

// New creates a Session and binds a random localhost port.
func New(token string) (*Server, error) {
	session, err := executor.NewSession()
	if err != nil {
		return nil, fmt.Errorf("create shell session: %w", err)
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("bind listener: %w", err)
	}

	return &Server{
		token:   token,
		Port:    ln.Addr().(*net.TCPAddr).Port,
		session: session,
		ln:      ln,
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
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{
		"status":  "ok",
		"version": "0.1.0",
	})
}

// Start registers routes and begins serving.  It blocks until ctx is cancelled.
func (s *Server) Start(ctx context.Context) error {
	mux := http.NewServeMux()
	mux.HandleFunc("/ping", s.handlePing)
	mux.HandleFunc("/info", s.requireAuth(s.handleInfo))
	mux.HandleFunc("/exec", s.requireAuth(s.handleExec))

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
