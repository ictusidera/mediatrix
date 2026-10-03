package control

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ictusidera/mediatrix/internal/model"
)

const testToken = "0123456789abcdef0123456789abcdef"

var testKey = model.FileKey(strings.Repeat("a", 64))

type mockBackend struct {
	calls    atomic.Int64
	register func(context.Context, model.Service) error
	delete   func(context.Context, string) error
	call     func(context.Context, model.CallRequest) (model.CallResponse, error)
	upload   func(context.Context, io.Reader) (model.FileInfo, error)
	access   func(context.Context, string, []string) error
	fetch    func(context.Context, model.FetchRequest) (model.FileInfo, error)
	open     func(string) (io.ReadCloser, model.FileInfo, error)
}

func (m *mockBackend) Info() model.NodeInfo {
	m.calls.Add(1)
	return model.NodeInfo{Version: model.Version, PeerID: "test-peer"}
}
func (m *mockBackend) Services() []model.Service {
	m.calls.Add(1)
	return nil
}
func (m *mockBackend) RegisterService(ctx context.Context, service model.Service) error {
	m.calls.Add(1)
	if m.register != nil {
		return m.register(ctx, service)
	}
	return nil
}
func (m *mockBackend) DeleteService(ctx context.Context, name string) error {
	m.calls.Add(1)
	if m.delete != nil {
		return m.delete(ctx, name)
	}
	return nil
}
func (m *mockBackend) Call(ctx context.Context, req model.CallRequest) (model.CallResponse, error) {
	m.calls.Add(1)
	if m.call != nil {
		return m.call(ctx, req)
	}
	return model.CallResponse{RequestID: req.RequestID, Result: json.RawMessage(`{"ok":true}`)}, nil
}
func (m *mockBackend) Upload(ctx context.Context, reader io.Reader) (model.FileInfo, error) {
	m.calls.Add(1)
	if m.upload != nil {
		return m.upload(ctx, reader)
	}
	data, err := io.ReadAll(reader)
	if err != nil {
		return model.FileInfo{}, err
	}
	digest := sha256.Sum256(data)
	return model.FileInfo{Key: model.FileKey(hex.EncodeToString(digest[:])), Size: int64(len(data)), AllowedPeers: []string{}}, nil
}
func (m *mockBackend) SetFileAccess(ctx context.Context, key string, peers []string) error {
	m.calls.Add(1)
	if m.access != nil {
		return m.access(ctx, key, peers)
	}
	return nil
}
func (m *mockBackend) Fetch(ctx context.Context, req model.FetchRequest) (model.FileInfo, error) {
	m.calls.Add(1)
	if m.fetch != nil {
		return m.fetch(ctx, req)
	}
	return model.FileInfo{Key: req.Key, Size: 4}, nil
}
func (m *mockBackend) OpenFile(key string) (io.ReadCloser, model.FileInfo, error) {
	m.calls.Add(1)
	if m.open != nil {
		return m.open(key)
	}
	return io.NopCloser(strings.NewReader("file")), model.FileInfo{Key: key, Size: 4}, nil
}

func newTestServer(t *testing.T, backend *mockBackend) *Server {
	t.Helper()
	s, err := New("127.0.0.1:0", testToken, backend, 256, 4, 2, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	return s
}
func request(method, path, body string) *http.Request {
	r := httptest.NewRequest(method, "http://127.0.0.1:9000"+path, strings.NewReader(body))
	r.Header.Set("Authorization", "Bearer "+testToken)
	r.Header.Set("Content-Type", "application/json")
	return r
}
func execute(s *Server, r *http.Request) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	return w
}
func assertError(t *testing.T, w *httptest.ResponseRecorder, status int, code string) model.Error {
	t.Helper()
	if w.Code != status {
		t.Fatalf("status = %d, want %d: %s", w.Code, status, w.Body.String())
	}
	var envelope struct {
		Error model.Error `json:"error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("invalid error envelope: %v: %s", err, w.Body.String())
	}
	if envelope.Error.Code != code || envelope.Error.Message == "" {
		t.Fatalf("error = %+v, want code %q and a message", envelope.Error, code)
	}
	if w.Header().Get("Cache-Control") != "no-store" || w.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Fatal("security response headers missing")
	}
	if w.Header().Get("Access-Control-Allow-Origin") != "" || w.Header().Get("Content-Type") != "application/json" {
		t.Fatal("error response must be JSON without CORS")
	}
	return envelope.Error
}

func TestNewValidation(t *testing.T) {
	for _, addr := range []string{"127.0.0.1:0", "127.2.3.4:8080", "[::1]:8080", "[::ffff:127.0.0.1]:8080"} {
		if _, err := New(addr, testToken, &mockBackend{}, 1, 1, 1, time.Second); err != nil {
			t.Errorf("valid address %q: %v", addr, err)
		}
	}
	for _, addr := range []string{"", ":8080", "0.0.0.0:8080", "[::]:8080", "localhost:8080", "192.168.1.2:8080", "example.com:8080", "127.0.0.1", "127.0.0.1:http", "127.0.0.1:-1", "127.0.0.1:+80", "127.0.0.1:65536", "[::1%lo]:8080"} {
		if _, err := New(addr, testToken, &mockBackend{}, 1, 1, 1, time.Second); err == nil {
			t.Errorf("unsafe address %q accepted", addr)
		}
	}
	for _, token := range []string{"", testToken[:31], testToken + "\n", testToken + " ", strings.Repeat("a", 4097)} {
		if _, err := New("127.0.0.1:0", token, &mockBackend{}, 1, 1, 1, time.Second); err == nil {
			t.Errorf("invalid token accepted: length %d", len(token))
		}
	}
	for _, limits := range [][4]int64{{0, 1, 1, 1}, {1, 0, 1, 1}, {1, 1, 0, 1}, {1, 1, 1, 0}, {-1, 1, 1, 1}} {
		if _, err := New("127.0.0.1:0", testToken, &mockBackend{}, limits[0], limits[1], int(limits[2]), time.Duration(limits[3])); err == nil {
			t.Errorf("invalid limits accepted: %v", limits)
		}
	}
	if _, err := New("127.0.0.1:0", testToken, nil, 1, 1, 1, time.Second); err == nil {
		t.Fatal("nil backend accepted")
	}
}

func TestEveryEndpointAuthenticatesBeforeRouting(t *testing.T) {
	backend := &mockBackend{}
	s := newTestServer(t, backend)
	for _, path := range []string{"/v1/health", "/v1/node", "/v1/services", "/v1/call", "/v1/files", "/v1/files/access", "/v1/fetch", "/v1/files/" + strings.Repeat("a", 64), "/unknown"} {
		for _, auth := range []string{"", "Bearer wrong", "Bearer " + testToken + " ", "Basic " + testToken, "Bearer  " + testToken} {
			r := request(http.MethodGet, path, "")
			r.Header.Set("Authorization", auth)
			w := execute(s, r)
			assertError(t, w, 401, "unauthorized")
			if w.Header().Get("WWW-Authenticate") == "" {
				t.Fatal("missing bearer challenge")
			}
		}
	}
	r := request(http.MethodGet, "/v1/node", "")
	r.Header.Add("Authorization", "Bearer "+testToken)
	assertError(t, execute(s, r), 401, "unauthorized")
	if backend.calls.Load() != 0 {
		t.Fatal("unauthenticated request reached backend")
	}
	r = request(http.MethodGet, "/v1/node", "")
	r.Header.Set("Authorization", "bEaReR "+testToken)
	if w := execute(s, r); w.Code != 200 {
		t.Fatalf("case-insensitive bearer scheme rejected: %s", w.Body.String())
	}
}

func TestOriginAndHostProtection(t *testing.T) {
	backend := &mockBackend{}
	s := newTestServer(t, backend)
	for _, origin := range []string{"https://attacker.example", "http://127.0.0.1:9000", "null", ""} {
		r := request(http.MethodGet, "/v1/health", "")
		r.Header.Set("Origin", origin)
		assertError(t, execute(s, r), 403, "forbidden")
	}
	for _, host := range []string{"", "localhost:9000", "attacker.example:9000", "0.0.0.0:9000", "[::]:9000", "192.168.1.2:9000", "127.0.0.1:65536", "127.0.0.1:http", "127.0.0.1:80@attacker.example"} {
		r := request(http.MethodGet, "/v1/node", "")
		r.Host = host
		assertError(t, execute(s, r), 403, "forbidden")
	}
	if backend.calls.Load() != 0 {
		t.Fatal("browser or non-loopback host reached backend")
	}
	for _, host := range []string{"127.0.0.1", "127.0.0.1:9000", "[::1]", "[::1]:9000"} {
		r := request(http.MethodGet, "/v1/health", "")
		r.Host = host
		if w := execute(s, r); w.Code != 200 {
			t.Fatalf("numeric loopback Host %q rejected: %s", host, w.Body.String())
		}
	}
}

func TestRoutesAndResponseContracts(t *testing.T) {
	backend := &mockBackend{
		register: func(ctx context.Context, service model.Service) error {
			if service.Name != "service:example" || service.URL != "http://127.0.0.1:8000" || len(service.AllowedPeers) != 1 {
				t.Errorf("wrong registration: %+v", service)
			}
			return nil
		},
		delete: func(ctx context.Context, name string) error {
			if name != "service:example" {
				t.Errorf("wrong deletion name %q", name)
			}
			return nil
		},
		access: func(ctx context.Context, key string, peers []string) error {
			if key != testKey || len(peers) != 1 || peers[0] != "peer" {
				t.Errorf("wrong access arguments: %q, %v", key, peers)
			}
			return nil
		},
	}
	s := newTestServer(t, backend)
	tests := []struct {
		method, path, body string
		status             int
		contains           string
	}{
		{"GET", "/v1/health", "", 200, `"status":"ok"`},
		{"GET", "/v1/node", "", 200, `"peer_id":"test-peer"`},
		{"GET", "/v1/services", "", 200, `[]`},
		{"PUT", "/v1/services", `{"name":"service:example","url":"http://127.0.0.1:8000","allowed_peers":["peer"]}`, 204, ""},
		{"DELETE", "/v1/services?name=service%3Aexample", "", 204, ""},
		{"POST", "/v1/call", `{"service":"service:example","method":"ping","params":{},"request_id":"request-1"}`, 200, `"request_id":"request-1","result":{"ok":true}`},
		{"PUT", "/v1/files/access", `{"key":"` + testKey + `","allowed_peers":["peer"]}`, 204, ""},
		{"POST", "/v1/fetch", `{"key":"` + testKey + `","timeout_ms":123}`, 200, `"size":4`},
		{"GET", "/v1/files/" + strings.Repeat("a", 64), "", 200, "file"},
	}
	for _, tt := range tests {
		t.Run(tt.method+tt.path, func(t *testing.T) {
			w := execute(s, request(tt.method, tt.path, tt.body))
			if w.Code != tt.status || !strings.Contains(w.Body.String(), tt.contains) {
				t.Fatalf("got %d %s, want %d containing %q", w.Code, w.Body.String(), tt.status, tt.contains)
			}
			if tt.status == 204 && w.Body.Len() != 0 {
				t.Fatal("204 response has a body")
			}
		})
	}
	r := request(http.MethodPost, "/v1/files", "file")
	r.Header.Set("Content-Type", "application/octet-stream")
	w := execute(s, r)
	if w.Code != 201 {
		t.Fatalf("upload: %d %s", w.Code, w.Body.String())
	}
	var info model.FileInfo
	if err := json.Unmarshal(w.Body.Bytes(), &info); err != nil || info.Size != 4 || len(info.AllowedPeers) != 0 {
		t.Fatalf("bad upload response %s", w.Body.String())
	}
}

func TestMethodsAndQueryValidation(t *testing.T) {
	s := newTestServer(t, &mockBackend{})
	for _, path := range []string{"/v1/health", "/v1/node", "/v1/services", "/v1/call", "/v1/files", "/v1/files/access", "/v1/fetch", "/v1/files/" + strings.Repeat("a", 64)} {
		for _, method := range []string{http.MethodOptions, http.MethodHead, http.MethodPatch} {
			w := execute(s, request(method, path, ""))
			assertError(t, w, 405, "invalid")
			if w.Header().Get("Allow") == "" {
				t.Fatal("method rejection must specify Allow")
			}
		}
	}
	for _, path := range []string{"/v1/services", "/v1/services?name=", "/v1/services?name=bad", "/v1/services?name=service:a&name=service:b", "/v1/services?name=service:a&token=secret", "/v1/services?name=service:a;other=b", "/v1/services?name=%zz"} {
		assertError(t, execute(s, request("DELETE", path, "")), 400, "invalid")
	}
	for _, path := range []string{"/v1/health?token=secret", "/v1/services?name=service:a", "/v1/node?unexpected=yes", "/v1/files/" + strings.Repeat("a", 63), "/v1/files/" + strings.Repeat("A", 64)} {
		assertError(t, execute(s, request("GET", path, "")), 400, "invalid")
	}
	assertError(t, execute(s, request("GET", "/missing", "")), 404, "not_found")
	assertError(t, execute(s, request("GET", "/v1/node", "body")), 400, "invalid")
}

func TestStrictJSON(t *testing.T) {
	backend := &mockBackend{}
	s := newTestServer(t, backend)
	for _, body := range []string{"", "null", "[]", `"text"`, `{`, `{"service":"service:a","surprise":true}`, `{"service":"service:a"} {}`, `{"service":"service:a"} garbage`, `{"timeout_ms":"not-a-number"}`} {
		assertError(t, execute(s, request("POST", "/v1/call", body)), 400, "invalid")
	}
	for _, contentType := range []string{"", "text/plain", "application/x-www-form-urlencoded", "application/json;broken"} {
		r := request("POST", "/v1/call", `{}`)
		r.Header.Set("Content-Type", contentType)
		assertError(t, execute(s, r), 415, "invalid")
	}
	r := request("POST", "/v1/call", `{}`)
	r.Header.Set("Content-Encoding", "gzip")
	assertError(t, execute(s, r), 415, "invalid")
	for _, path := range []string{"/v1/fetch", "/v1/files/access"} {
		method := "POST"
		if strings.Contains(path, "access") {
			method = "PUT"
		}
		assertError(t, execute(s, request(method, path, `{"key":"bad"}`)), 400, "invalid")
	}
	assertError(t, execute(s, request("PUT", "/v1/services", `{"name":"bad"}`)), 400, "invalid")
	if backend.calls.Load() != 0 {
		t.Fatal("invalid JSON or metadata reached backend")
	}
	r = request("POST", "/v1/call", `{"params":{"arbitrary":"application data"}} `)
	r.Header.Set("Content-Type", "application/json; charset=utf-8")
	if w := execute(s, r); w.Code != 200 {
		t.Fatalf("valid JSON charset or application params rejected: %s", w.Body.String())
	}
}

func TestBodyLimitsIncludingChunkedAndTrailingWhitespace(t *testing.T) {
	for _, unknownLength := range []bool{false, true} {
		t.Run(fmt.Sprintf("unknown-length-%v", unknownLength), func(t *testing.T) {
			backend := &mockBackend{}
			s := newTestServer(t, backend)
			r := request("POST", "/v1/call", `{}`+strings.Repeat(" ", 255))
			if unknownLength {
				r.ContentLength = -1
			}
			assertError(t, execute(s, r), 413, "too_large")
			if backend.calls.Load() != 0 {
				t.Fatal("oversized JSON reached backend")
			}
			r = request("POST", "/v1/files", "12345")
			r.Header.Set("Content-Type", "application/octet-stream")
			if unknownLength {
				r.ContentLength = -1
			}
			assertError(t, execute(s, r), 413, "too_large")
			if !unknownLength && backend.calls.Load() != 0 {
				t.Fatal("known oversized upload reached backend")
			}
		})
	}
	s := newTestServer(t, &mockBackend{})
	for _, body := range []string{"", "1234"} {
		r := request("POST", "/v1/files", body)
		r.Header.Del("Content-Type")
		r.ContentLength = -1
		if w := execute(s, r); w.Code != 201 {
			t.Fatalf("at-limit upload rejected: %s", w.Body.String())
		}
	}
	r := request("POST", "/v1/call", `{}`+strings.Repeat(" ", 254))
	r.ContentLength = -1
	if w := execute(s, r); w.Code != 200 {
		t.Fatalf("at-limit JSON rejected: %s", w.Body.String())
	}
	for _, readBody := range []bool{false, true} {
		backend := &mockBackend{upload: func(ctx context.Context, r io.Reader) (model.FileInfo, error) {
			if readBody {
				_, _ = io.ReadAll(r)
			}
			return model.FileInfo{}, nil
		}}
		s := newTestServer(t, backend)
		r := request("POST", "/v1/files", "12345")
		r.Header.Del("Content-Type")
		r.ContentLength = -1
		assertError(t, execute(s, r), 413, "too_large")
	}
}

func TestErrorMappingAndCallApplicationErrors(t *testing.T) {
	for _, tt := range []struct {
		code   string
		status int
	}{{"invalid", 400}, {"forbidden", 403}, {"not_found", 404}, {"conflict", 409}, {"too_large", 413}, {"busy", 429}, {"unavailable", 502}, {"unknown", 502}, {"timeout", 504}} {
		t.Run(tt.code, func(t *testing.T) {
			backend := &mockBackend{call: func(context.Context, model.CallRequest) (model.CallResponse, error) {
				return model.CallResponse{}, fmt.Errorf("wrapped: %w", &model.Error{Code: tt.code, Message: "useful message", OutcomeUnknown: true})
			}}
			apiErr := assertError(t, execute(newTestServer(t, backend), request("POST", "/v1/call", `{}`)), tt.status, tt.code)
			if !apiErr.OutcomeUnknown {
				t.Fatal("outcome_unknown was lost")
			}
		})
	}
	backend := &mockBackend{call: func(context.Context, model.CallRequest) (model.CallResponse, error) {
		return model.CallResponse{}, errors.New("secret payload and filesystem details")
	}}
	w := execute(newTestServer(t, backend), request("POST", "/v1/call", `{}`))
	assertError(t, w, 502, "unavailable")
	if strings.Contains(w.Body.String(), "secret") {
		t.Fatal("untyped internal error leaked")
	}
	backend.call = func(context.Context, model.CallRequest) (model.CallResponse, error) {
		return model.CallResponse{RequestID: "id", Error: model.Err("application_error", "expected application failure")}, nil
	}
	w = execute(newTestServer(t, backend), request("POST", "/v1/call", `{}`))
	if w.Code != 200 || !strings.Contains(w.Body.String(), "application_error") {
		t.Fatalf("application response changed: %s", w.Body.String())
	}
	backend.call = func(context.Context, model.CallRequest) (model.CallResponse, error) {
		return model.CallResponse{Result: json.RawMessage(`broken`)}, nil
	}
	assertError(t, execute(newTestServer(t, backend), request("POST", "/v1/call", `{}`)), 502, "unavailable")
}

func TestCancellationPropagatesToBackend(t *testing.T) {
	started := make(chan struct{})
	backend := &mockBackend{call: func(ctx context.Context, _ model.CallRequest) (model.CallResponse, error) {
		close(started)
		<-ctx.Done()
		return model.CallResponse{}, ctx.Err()
	}}
	s := newTestServer(t, backend)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r := request("POST", "/v1/call", `{}`).WithContext(ctx)
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- execute(s, r) }()
	<-started
	cancel()
	select {
	case w := <-done:
		apiErr := assertError(t, w, 504, "timeout")
		if !apiErr.OutcomeUnknown {
			t.Fatal("canceled call must preserve uncertainty")
		}
	case <-time.After(time.Second):
		t.Fatal("request cancellation did not reach backend")
	}
	if len(s.concurrent) != 0 {
		t.Fatal("canceled request leaked a concurrency slot")
	}
}

func TestDeadlineAndAlreadyCanceledRequest(t *testing.T) {
	backend := &mockBackend{fetch: func(ctx context.Context, _ model.FetchRequest) (model.FileInfo, error) {
		if _, ok := ctx.Deadline(); !ok {
			t.Error("no backend deadline")
		}
		<-ctx.Done()
		return model.FileInfo{}, ctx.Err()
	}}
	s, err := New("127.0.0.1:0", testToken, backend, 256, 4, 1, 15*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	assertError(t, execute(s, request("POST", "/v1/fetch", `{"key":"`+testKey+`"}`)), 504, "timeout")
	calls := backend.calls.Load()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	assertError(t, execute(s, request("GET", "/v1/node", "").WithContext(ctx)), 504, "timeout")
	if backend.calls.Load() != calls {
		t.Fatal("already canceled request reached backend")
	}
	if s.http.ReadHeaderTimeout <= 0 || s.http.ReadTimeout <= 0 || s.http.WriteTimeout <= 0 || s.http.IdleTimeout <= 0 || s.http.MaxHeaderBytes <= 0 {
		t.Fatal("server transport limits are missing")
	}
}

func TestUploadOccupiesConcurrencySlotUntilBodyFinishes(t *testing.T) {
	started := make(chan struct{})
	backend := &mockBackend{upload: func(ctx context.Context, reader io.Reader) (model.FileInfo, error) {
		close(started)
		data, err := io.ReadAll(reader)
		return model.FileInfo{Key: testKey, Size: int64(len(data))}, err
	}}
	s, err := New("127.0.0.1:0", testToken, backend, 256, 4, 1, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	reader, writer := io.Pipe()
	defer reader.Close()
	defer writer.Close()
	r := httptest.NewRequest("POST", "http://127.0.0.1/v1/files", reader)
	r.Header.Set("Authorization", "Bearer "+testToken)
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- execute(s, r) }()
	<-started
	w := execute(s, request("GET", "/v1/node", ""))
	assertError(t, w, 429, "busy")
	if w.Header().Get("Retry-After") != "1" {
		t.Fatal("missing retry hint")
	}
	if _, err := writer.Write([]byte("file")); err != nil {
		t.Fatal(err)
	}
	writer.Close()
	select {
	case w := <-done:
		if w.Code != 201 {
			t.Fatalf("upload failed: %s", w.Body.String())
		}
	case <-time.After(time.Second):
		t.Fatal("streamed upload failed to finish")
	}
	if w := execute(s, request("GET", "/v1/node", "")); w.Code != 200 {
		t.Fatal("upload leaked concurrency slot")
	}
}

type observedStream struct {
	reader io.Reader
	closed atomic.Int64
}

func (s *observedStream) Read(p []byte) (int, error) { return s.reader.Read(p) }
func (s *observedStream) Close() error               { s.closed.Add(1); return nil }

type brokenWriter struct{ header http.Header }

func (w *brokenWriter) Header() http.Header       { return w.header }
func (w *brokenWriter) WriteHeader(int)           {}
func (w *brokenWriter) Write([]byte) (int, error) { return 0, errors.New("client left") }

func TestDownloadClosesStreamsOnAllExitPaths(t *testing.T) {
	for _, tt := range []struct {
		name   string
		size   int64
		key    string
		err    error
		status int
	}{
		{"success", 4, testKey, nil, 200}, {"zero-length", 0, testKey, nil, 200},
		{"oversized", 5, testKey, nil, 413}, {"bad-size", -1, testKey, nil, 502},
		{"wrong-key", 4, "wrong", nil, 502}, {"open-error", 4, testKey, model.Err("not_found", "missing"), 404},
	} {
		t.Run(tt.name, func(t *testing.T) {
			stream := &observedStream{reader: strings.NewReader("file and extra data")}
			backend := &mockBackend{open: func(string) (io.ReadCloser, model.FileInfo, error) {
				return stream, model.FileInfo{Key: tt.key, Size: tt.size}, tt.err
			}}
			w := execute(newTestServer(t, backend), request("GET", "/v1/files/"+strings.Repeat("a", 64), ""))
			if w.Code != tt.status {
				t.Fatalf("status %d: %s", w.Code, w.Body.String())
			}
			if stream.closed.Load() != 1 {
				t.Fatalf("stream closed %d times", stream.closed.Load())
			}
			if tt.status == 200 && int64(w.Body.Len()) != tt.size {
				t.Fatal("download exposed bytes beyond verified size")
			}
		})
	}
	stream := &observedStream{reader: strings.NewReader("file")}
	backend := &mockBackend{open: func(string) (io.ReadCloser, model.FileInfo, error) {
		return stream, model.FileInfo{Key: testKey, Size: 4}, nil
	}}
	s := newTestServer(t, backend)
	s.Handler().ServeHTTP(&brokenWriter{header: http.Header{}}, request("GET", "/v1/files/"+strings.Repeat("a", 64), ""))
	if stream.closed.Load() != 1 {
		t.Fatal("write failure leaked stream")
	}
	backend.open = func(string) (io.ReadCloser, model.FileInfo, error) { return nil, model.FileInfo{}, nil }
	assertError(t, execute(s, request("GET", "/v1/files/"+strings.Repeat("a", 64), "")), 502, "unavailable")
}

type blockingStream struct {
	started    chan struct{}
	closed     chan struct{}
	once       sync.Once
	readOnce   sync.Once
	closeCalls atomic.Int64
}

func (b *blockingStream) Read([]byte) (int, error) {
	b.readOnce.Do(func() { close(b.started) })
	<-b.closed
	return 0, errors.New("closed")
}
func (b *blockingStream) Close() error {
	b.closeCalls.Add(1)
	b.once.Do(func() { close(b.closed) })
	return nil
}
func TestCanceledDownloadClosesBlockedStreamAndReleasesSlot(t *testing.T) {
	stream := &blockingStream{started: make(chan struct{}), closed: make(chan struct{})}
	backend := &mockBackend{open: func(string) (io.ReadCloser, model.FileInfo, error) {
		return stream, model.FileInfo{Key: testKey, Size: 4}, nil
	}}
	s, err := New("127.0.0.1:0", testToken, backend, 256, 4, 1, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r := request("GET", "/v1/files/"+strings.Repeat("a", 64), "").WithContext(ctx)
	done := make(chan struct{})
	go func() { execute(s, r); close(done) }()
	<-stream.started
	assertError(t, execute(s, request("GET", "/v1/node", "")), 429, "busy")
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("cancellation did not unblock download")
	}
	if stream.closeCalls.Load() != 1 {
		t.Fatalf("stream closed %d times", stream.closeCalls.Load())
	}
	if w := execute(s, request("GET", "/v1/node", "")); w.Code != 200 {
		t.Fatal("download leaked slot")
	}
}

type unusedListener struct {
	addr    net.Addr
	accepts atomic.Int64
}

func (l *unusedListener) Accept() (net.Conn, error) {
	l.accepts.Add(1)
	return nil, errors.New("unexpected accept")
}
func (l *unusedListener) Close() error   { return nil }
func (l *unusedListener) Addr() net.Addr { return l.addr }

func TestServeRejectsNonLoopbackListener(t *testing.T) {
	s := newTestServer(t, &mockBackend{})
	if err := s.Serve(nil); err == nil {
		t.Fatal("nil listener accepted")
	}
	for _, addr := range []net.Addr{&net.TCPAddr{IP: net.IPv4zero, Port: 9000}, &net.TCPAddr{IP: net.ParseIP("192.168.1.2"), Port: 9000}, &net.TCPAddr{IP: net.IPv6zero, Port: 9000}, &net.UnixAddr{Name: "/tmp/socket", Net: "unix"}} {
		listener := &unusedListener{addr: addr}
		if err := s.Serve(listener); err == nil {
			t.Errorf("non-loopback listener %s accepted", addr)
		}
		if listener.accepts.Load() != 0 {
			t.Fatal("unsafe listener reached HTTP serve")
		}
	}
}

func TestServeAndShutdownOverLoopback(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	s := newTestServer(t, &mockBackend{})
	done := make(chan error, 1)
	go func() { done <- s.Serve(listener) }()
	defer s.Shutdown(context.Background())
	r, err := http.NewRequest("GET", "http://"+listener.Addr().String()+"/v1/health", nil)
	if err != nil {
		t.Fatal(err)
	}
	r.Header.Set("Authorization", "Bearer "+testToken)
	client := &http.Client{Timeout: time.Second}
	resp, err := client.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil || resp.StatusCode != 200 || !bytes.Contains(body, []byte(`"status":"ok"`)) {
		t.Fatalf("health response %d %s, error %v", resp.StatusCode, body, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := s.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-done; !errors.Is(err, http.ErrServerClosed) {
		t.Fatalf("Serve result %v", err)
	}
}

func startTCPServer(t *testing.T, s *Server) net.Listener {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- s.Serve(listener) }()
	t.Cleanup(func() {
		_ = s.Close()
		select {
		case err := <-done:
			if !errors.Is(err, http.ErrServerClosed) {
				t.Errorf("Serve returned %v", err)
			}
		case <-time.After(time.Second):
			t.Error("Serve did not stop")
		}
	})
	return listener
}

func TestTCPConnectionCapIncludesUnauthenticatedAndIdleConnections(t *testing.T) {
	for _, idle := range []bool{false, true} {
		t.Run(fmt.Sprintf("idle-%v", idle), func(t *testing.T) {
			s, err := New("127.0.0.1:0", testToken, &mockBackend{}, 256, 4, 1, 5*time.Second)
			if err != nil {
				t.Fatal(err)
			}
			accepted := make(chan struct{}, s.connectionLimit+2)
			s.http.ConnState = func(_ net.Conn, state http.ConnState) {
				if state == http.StateNew {
					accepted <- struct{}{}
				}
			}
			listener := startTCPServer(t, s)
			var clients []net.Conn
			defer func() {
				for _, c := range clients {
					c.Close()
				}
			}()
			wireRequest := "GET /v1/health HTTP/1.1\r\nHost: " + listener.Addr().String() + "\r\nAuthorization: Bearer " + testToken + "\r\n\r\n"
			for i := 0; i < s.connectionLimit; i++ {
				conn, err := net.DialTimeout("tcp", listener.Addr().String(), time.Second)
				if err != nil {
					t.Fatal(err)
				}
				clients = append(clients, conn)
				select {
				case <-accepted:
				case <-time.After(time.Second):
					t.Fatalf("connection %d was not accepted", i)
				}
				if idle {
					conn.SetDeadline(time.Now().Add(time.Second))
					if _, err := io.WriteString(conn, wireRequest); err != nil {
						t.Fatal(err)
					}
					resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
					if err != nil {
						t.Fatal(err)
					}
					_, _ = io.Copy(io.Discard, resp.Body)
					resp.Body.Close()
					if resp.StatusCode != 200 {
						t.Fatalf("health status %d", resp.StatusCode)
					}
					conn.SetDeadline(time.Time{})
				}
			}
			excess, err := net.DialTimeout("tcp", listener.Addr().String(), time.Second)
			if err != nil {
				t.Fatal(err)
			}
			defer excess.Close()
			excess.SetDeadline(time.Now().Add(40 * time.Millisecond))
			if _, err := io.WriteString(excess, wireRequest); err != nil {
				t.Fatal(err)
			}
			var b [1]byte
			if _, err := excess.Read(b[:]); err == nil {
				t.Fatal("connection above cap was serviced")
			} else if ne, ok := err.(net.Error); !ok || !ne.Timeout() {
				t.Fatalf("expected waiting socket, got %v", err)
			}
			select {
			case <-accepted:
				t.Fatal("accepted socket above connection cap")
			default:
			}
			clients[0].Close()
			select {
			case <-accepted:
			case <-time.After(time.Second):
				t.Fatal("closing a socket did not release a slot")
			}
			excess.SetDeadline(time.Now().Add(time.Second))
			resp, err := http.ReadResponse(bufio.NewReader(excess), nil)
			if err != nil {
				t.Fatal(err)
			}
			_, _ = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			if resp.StatusCode != 200 {
				t.Fatalf("released connection status %d", resp.StatusCode)
			}
		})
	}
}

func TestBodyCancellationUnblocksEmbeddedUpload(t *testing.T) {
	started := make(chan struct{})
	backend := &mockBackend{upload: func(ctx context.Context, reader io.Reader) (model.FileInfo, error) {
		close(started)
		_, err := io.ReadAll(reader)
		return model.FileInfo{}, err
	}}
	s := newTestServer(t, backend)
	reader, writer := io.Pipe()
	defer reader.Close()
	defer writer.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r := httptest.NewRequest("POST", "http://127.0.0.1/v1/files", reader).WithContext(ctx)
	r.Header.Set("Authorization", "Bearer "+testToken)
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- execute(s, r) }()
	<-started
	cancel()
	select {
	case w := <-done:
		assertError(t, w, 504, "timeout")
	case <-time.After(time.Second):
		t.Fatal("cancellation did not close a blocked body")
	}
	if len(s.concurrent) != 0 {
		t.Fatal("canceled upload retained concurrency slot")
	}
}

func TestStalledTCPUploadStopsOnDeadlineCloseAndShutdown(t *testing.T) {
	for _, mode := range []string{"deadline", "close", "shutdown"} {
		t.Run(mode, func(t *testing.T) {
			started := make(chan struct{})
			finished := make(chan struct{})
			backend := &mockBackend{upload: func(ctx context.Context, reader io.Reader) (model.FileInfo, error) {
				close(started)
				_, err := io.ReadAll(reader)
				close(finished)
				return model.FileInfo{}, err
			}}
			timeout := 5 * time.Second
			if mode == "deadline" {
				timeout = 60 * time.Millisecond
			}
			s, err := New("127.0.0.1:0", testToken, backend, 256, 4, 1, timeout)
			if err != nil {
				t.Fatal(err)
			}
			listener := startTCPServer(t, s)
			conn, err := net.DialTimeout("tcp", listener.Addr().String(), time.Second)
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			wire := "POST /v1/files HTTP/1.1\r\nHost: " + listener.Addr().String() + "\r\nAuthorization: Bearer " + testToken + "\r\nContent-Type: application/octet-stream\r\nTransfer-Encoding: chunked\r\n\r\n"
			if _, err := io.WriteString(conn, wire); err != nil {
				t.Fatal(err)
			}
			select {
			case <-started:
			case <-time.After(time.Second):
				t.Fatal("upload did not begin")
			}
			switch mode {
			case "close":
				if err := s.Close(); err != nil {
					t.Fatal(err)
				}
			case "shutdown":
				ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
				defer cancel()
				if err := s.Shutdown(ctx); !errors.Is(err, context.DeadlineExceeded) {
					t.Fatalf("grace timeout result %v", err)
				}
			}
			select {
			case <-finished:
			case <-time.After(time.Second):
				t.Fatal("stalled upload did not release backend")
			}
			// Verify the handler released its slot, not only the mock's Read.
			limit := time.Now().Add(time.Second)
			for len(s.concurrent) != 0 && time.Now().Before(limit) {
				time.Sleep(time.Millisecond)
			}
			if len(s.concurrent) != 0 {
				t.Fatal("stalled upload retained a request slot")
			}
		})
	}
}

func TestBoundedListenerCloseUnblocksFullAccept(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	bounded := newBoundedListener(listener, 1)
	defer bounded.Close()
	client, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	accepted, err := bounded.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer accepted.Close()
	done := make(chan error, 1)
	go func() { _, err := bounded.Accept(); done <- err }()
	bounded.Close()
	select {
	case err := <-done:
		if !errors.Is(err, net.ErrClosed) {
			t.Fatalf("Accept result %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("full listener did not close")
	}
	accepted.Close()
	accepted.Close()
	if len(bounded.slots) != 0 {
		t.Fatal("connection slot was not released exactly once")
	}
}
