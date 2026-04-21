package control

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestClientRequiresBearerToken(t *testing.T) {
	mux := http.NewServeMux()
	s := &Server{token: "secret"}
	wrapped := s.authMiddleware(mux)

	req := httptest.NewRequest(http.MethodGet, "/v1/node", nil)
	rec := httptest.NewRecorder()
	wrapped.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("missing token status: got %d", rec.Code)
	}

	req = httptest.NewRequest(http.MethodGet, "/v1/node", nil)
	req.Header.Set("authorization", "Bearer wrong")
	rec = httptest.NewRecorder()
	wrapped.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("wrong token status: got %d", rec.Code)
	}

	req = httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec = httptest.NewRecorder()
	wrapped.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("healthz should bypass auth and reach mux: got %d", rec.Code)
	}
}

func TestClientAddsBearerToken(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("authorization")
		w.Header().Set("content-type", "application/json")
		_, _ = w.Write([]byte(`{"peer_id":"peer","addrs":[]}`))
	}))
	defer srv.Close()

	client := NewClient(srv.URL, "secret")
	if _, err := client.Node(context.Background()); err != nil {
		t.Fatalf("node request: %v", err)
	}
	if gotAuth != "Bearer secret" {
		t.Fatalf("authorization header mismatch: %q", gotAuth)
	}
}
