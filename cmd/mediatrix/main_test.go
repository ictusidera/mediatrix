package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/ictusidera/mediatrix/internal/model"
)

func TestValidateAPI(t *testing.T) {
	for _, good := range []string{"http://127.0.0.1:47832", "http://[::1]:1234/", "http://127.0.0.2:4567"} {
		if _, err := validateAPI(good); err != nil {
			t.Fatalf("%s: %v", good, err)
		}
	}
	for _, bad := range []string{"https://127.0.0.1:1234", "http://localhost:1234", "http://example.com:1234", "http://0.0.0.0:1234", "http://127.0.0.1:0", "http://127.0.0.1", "http://a:b@127.0.0.1:1234", "http://127.0.0.1:1234/path", "http://127.0.0.1:1234?foo=bar", "http://127.0.0.1:1234#x"} {
		if _, err := validateAPI(bad); err == nil {
			t.Fatalf("unsafe API accepted: %s", bad)
		}
	}
}
func TestCommandFailures(t *testing.T) {
	t.Setenv("MEDIATRIX_API_TOKEN", strings.Repeat("x", 32))
	for _, args := range [][]string{{}, {"unknown"}, {"node", "junk"}, {"call", "--service", "oops"}, {"upload"}, {"fetch", "--key", "invalid"}, {"--timeout", "0s", "node"}} {
		if err := run(args, io.Discard, io.Discard); err == nil {
			t.Fatalf("invalid args accepted: %v", args)
		}
	}
	if err := run([]string{"--help"}, io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
}
func TestNodeCommand(t *testing.T) {
	token := strings.Repeat("t", 32)
	t.Setenv("MEDIATRIX_API_TOKEN", token)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/node" || r.Method != http.MethodGet {
			t.Error("unexpected route")
		}
		if r.Header.Get("Authorization") != "Bearer "+token {
			t.Error("missing auth")
		}
		json.NewEncoder(w).Encode(model.NodeInfo{PeerID: "test"})
	}))
	defer server.Close()
	var out bytes.Buffer
	if err := run([]string{"--api", server.URL, "node"}, &out, io.Discard); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), `"peer_id": "test"`) {
		t.Fatalf("unexpected output: %s", out.String())
	}
}
func TestRedirectRefused(t *testing.T) {
	leaked := false
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { leaked = true }))
	defer target.Close()
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, target.URL, 302) }))
	defer source.Close()
	t.Setenv("MEDIATRIX_API_TOKEN", strings.Repeat("x", 32))
	if err := run([]string{"--api", source.URL, "node"}, io.Discard, io.Discard); err == nil {
		t.Fatal("redirect accepted")
	}
	if leaked {
		t.Fatal("redirect followed")
	}
}
func TestReadBounded(t *testing.T) {
	if _, err := readBounded(strings.NewReader("abcd"), 3); err == nil {
		t.Fatal("oversized response accepted")
	}
	if got, err := readBounded(strings.NewReader("abc"), 3); err != nil || string(got) != "abc" {
		t.Fatal("exact bound failed")
	}
}
func TestFetchVerifiedAndNoOverwrite(t *testing.T) {
	body := []byte("verified file\n")
	sum := sha256.Sum256(body)
	digest := hex.EncodeToString(sum[:])
	key := model.FileKey(digest)
	var output string
	var corrupt, race bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/fetch":
			json.NewEncoder(w).Encode(model.FileInfo{Key: key, Size: int64(len(body))})
		case "/v1/files/" + digest:
			if race {
				if err := os.WriteFile(output, []byte("keep me"), 0600); err != nil {
					t.Error(err)
				}
			}
			if corrupt {
				w.Write(bytes.Repeat([]byte("!"), len(body)))
			} else {
				w.Write(body)
			}
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	c := apiClient{base: server.URL, token: strings.Repeat("x", 32), client: server.Client()}
	dir := t.TempDir()
	output = filepath.Join(dir, "result")
	if err := c.fetch(context.Background(), key, output, 0, io.Discard); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(output)
	if !bytes.Equal(got, body) {
		t.Fatal("wrong file")
	}
	if st, err := os.Stat(output); err != nil || (runtime.GOOS != "windows" && st.Mode().Perm() != 0600) {
		t.Fatal("output must be private")
	}
	if err := c.fetch(context.Background(), key, output, 0, io.Discard); err == nil {
		t.Fatal("existing output overwritten")
	}
	output = filepath.Join(dir, "corrupt")
	corrupt = true
	if err := c.fetch(context.Background(), key, output, 0, io.Discard); err == nil {
		t.Fatal("corrupt download accepted")
	}
	if _, err := os.Lstat(output); !os.IsNotExist(err) {
		t.Fatal("corrupt output published")
	}
	output = filepath.Join(dir, "race")
	corrupt = false
	race = true
	if err := c.fetch(context.Background(), key, output, 0, io.Discard); err == nil {
		t.Fatal("race output overwritten")
	}
	got, _ = os.ReadFile(output)
	if string(got) != "keep me" {
		t.Fatal("race content changed")
	}
	matches, _ := filepath.Glob(filepath.Join(dir, ".mediatrix-download-*"))
	if len(matches) > 0 {
		t.Fatal("temporary downloads leaked")
	}
}
func TestCallErrorFails(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(model.CallResponse{RequestID: "abc", Error: model.Err("not_found", "service unavailable")})
	}))
	defer server.Close()
	t.Setenv("MEDIATRIX_API_TOKEN", strings.Repeat("x", 32))
	var out bytes.Buffer
	err := run([]string{"--api", server.URL, "call", "--service", "service:test", "--method", "ping"}, &out, io.Discard)
	if err == nil {
		t.Fatal("call error returned success")
	}
	if !strings.Contains(out.String(), "not_found") {
		t.Fatal("call error omitted")
	}
}

func TestCommandRoutes(t *testing.T) {
	token := strings.Repeat("t", 32)
	t.Setenv("MEDIATRIX_API_TOKEN", token)
	cases := []struct {
		name, method, path string
		args               []string
		body               string
		status             int
	}{
		{"services", http.MethodGet, "/v1/services", []string{"services"}, "[]", 200},
		{"register", http.MethodPut, "/v1/services", []string{"register-service", "--name", "service:test", "--url", "http://127.0.0.1:9000/rpc"}, "", 204},
		{"remove", http.MethodDelete, "/v1/services?name=service%3Atest", []string{"remove-service", "--name", "service:test"}, "", 204},
		{"grant", http.MethodPut, "/v1/files/access", []string{"grant-file", "--key", "file:sha256:" + strings.Repeat("a", 64)}, "", 204},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != tt.method || r.URL.RequestURI() != tt.path {
					t.Errorf("wrong request: %s %s", r.Method, r.URL.RequestURI())
				}
				if r.Header.Get("Authorization") != "Bearer "+token {
					t.Error("missing auth")
				}
				if r.Method == http.MethodPut {
					var payload map[string]any
					if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
						t.Error(err)
					}
					if allowed, ok := payload["allowed_peers"].([]any); !ok || len(allowed) != 0 {
						t.Error("empty ACL must be an explicit JSON array")
					}
				}
				w.WriteHeader(tt.status)
				io.WriteString(w, tt.body)
			}))
			defer server.Close()
			args := append([]string{"--api", server.URL}, tt.args...)
			if err := run(args, io.Discard, io.Discard); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestUpload(t *testing.T) {
	content := []byte("upload content")
	file := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(file, content, 0600); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/files" || r.Header.Get("Content-Type") != "application/octet-stream" {
			t.Error("wrong upload request")
		}
		got, err := io.ReadAll(r.Body)
		if err != nil || !bytes.Equal(got, content) {
			t.Error("upload content changed")
		}
		json.NewEncoder(w).Encode(model.FileInfo{Key: "file:sha256:" + strings.Repeat("a", 64), Size: int64(len(content))})
	}))
	defer server.Close()
	t.Setenv("MEDIATRIX_API_TOKEN", strings.Repeat("x", 32))
	if err := run([]string{"--api", server.URL, "upload", "--file", file}, io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	if err := run([]string{"--api", server.URL, "upload", "--file", filepath.Dir(file)}, io.Discard, io.Discard); err == nil {
		t.Fatal("directory upload accepted")
	}
}

func TestUnknownOutcomePreserved(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusGatewayTimeout)
		json.NewEncoder(w).Encode(map[string]any{"error": &model.Error{Code: "timeout", Message: "deadline exceeded", OutcomeUnknown: true}})
	}))
	defer server.Close()
	t.Setenv("MEDIATRIX_API_TOKEN", strings.Repeat("x", 32))
	err := run([]string{"--api", server.URL, "call", "--service", "service:test", "--method", "ping"}, io.Discard, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "outcome unknown") {
		t.Fatalf("ambiguous execution outcome hidden: %v", err)
	}
}

func TestMalformedSuccessfulCallOutcomeUnknown(t *testing.T) {
	cases := []struct {
		name    string
		respond func(http.ResponseWriter)
	}{
		{"truncated", func(w http.ResponseWriter) {
			w.Header().Set("Content-Length", "1000")
			io.WriteString(w, `{"request_id":"operation-1","result":42}`)
		}},
		{"oversized", func(w http.ResponseWriter) { io.WriteString(w, strings.Repeat(" ", maxJSON+1)) }},
		{"invalid_json", func(w http.ResponseWriter) { io.WriteString(w, `{invalid json`) }},
		{"empty", func(w http.ResponseWriter) {}},
		{"invalid_shape", func(w http.ResponseWriter) { io.WriteString(w, `{"request_id":"operation-1"}`) }},
		{"mismatched_request", func(w http.ResponseWriter) { io.WriteString(w, `{"request_id":"another-operation","result":42}`) }},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			var requests atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { requests.Add(1); tt.respond(w) }))
			defer server.Close()
			t.Setenv("MEDIATRIX_API_TOKEN", strings.Repeat("x", 32))
			err := run([]string{"--api", server.URL, "call", "--service", "service:test", "--method", "mutate", "--request-id", "operation-1"}, io.Discard, io.Discard)
			if err == nil || !strings.Contains(err.Error(), "outcome unknown") {
				t.Fatalf("ambiguous outcome hidden: %v", err)
			}
			if got := requests.Load(); got != 1 {
				t.Fatalf("call was retried: %d requests", got)
			}
		})
	}
}
