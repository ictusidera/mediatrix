package control

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/s-yamamoto/mediatrix/internal/config"
	"github.com/s-yamamoto/mediatrix/internal/p2p"
)

type Server struct {
	node  *p2p.Node
	token string
	srv   *http.Server
}

func New(addr, token string, node *p2p.Node) *Server {
	mux := http.NewServeMux()
	s := &Server{
		node:  node,
		token: token,
		srv: &http.Server{
			Addr:              addr,
			ReadHeaderTimeout: 5 * time.Second,
		},
	}
	s.srv.Handler = s.authMiddleware(mux)
	mux.HandleFunc("GET /healthz", s.handleHealth)
	mux.HandleFunc("GET /v1/node", s.handleNode)
	mux.HandleFunc("POST /v1/services", s.handleService)
	mux.HandleFunc("POST /v1/files", s.handleFile)
	return s
}

func (s *Server) ListenAndServe() error {
	return s.srv.ListenAndServe()
}

func (s *Server) Shutdown(ctx context.Context) error {
	return s.srv.Shutdown(ctx)
}

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) authMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			next.ServeHTTP(w, r)
			return
		}
		if s.token == "" {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "control token is not configured"})
			return
		}
		auth := r.Header.Get("Authorization")
		const prefix = "Bearer "
		if !strings.HasPrefix(auth, prefix) {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "bearer token is required"})
			return
		}
		got := strings.TrimPrefix(auth, prefix)
		if subtle.ConstantTimeCompare([]byte(got), []byte(s.token)) != 1 {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "invalid bearer token"})
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) handleNode(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"peer_id": s.node.PeerID().String(),
		"addrs":   s.node.ListenAddrs(),
	})
}

func (s *Server) handleService(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name    string   `json:"name"`
		Command []string `json:"command"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if req.Name == "" || len(req.Command) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "name and command are required"})
		return
	}
	if err := s.node.RegisterService(r.Context(), config.ServiceConfig{Name: req.Name, Command: req.Command}); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{"name": req.Name})
}

func (s *Server) handleFile(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Path string `json:"path"`
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if req.Path == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "path is required"})
		return
	}
	key, err := s.node.RegisterFile(r.Context(), req.Path, req.Name)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{"key": key})
}

func writeError(w http.ResponseWriter, status int, err error) {
	writeJSON(w, status, map[string]string{"error": err.Error()})
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("content-type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
