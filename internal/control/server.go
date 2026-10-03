// Package control exposes the authenticated, loopback-only daemon HTTP API.
// The API intentionally has no browser or cross-origin mode. Callers must use
// the bearer token in the Authorization header, never in a URL.
package control

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"mime"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ictusidera/mediatrix/internal/model"
)

// Backend implements the node operations needed by language-independent clients.
// Upload must consume its reader to EOF before committing a file. OpenFile must
// verify local content before returning a stream; the handler closes that stream.
// Context-aware operations must stop work when their context is canceled.
type Backend interface {
	Info() model.NodeInfo
	Services() []model.Service
	RegisterService(context.Context, model.Service) error
	DeleteService(context.Context, string) error
	Call(context.Context, model.CallRequest) (model.CallResponse, error)
	Upload(context.Context, io.Reader) (model.FileInfo, error)
	SetFileAccess(context.Context, string, []string) error
	Fetch(context.Context, model.FetchRequest) (model.FileInfo, error)
	OpenFile(string) (io.ReadCloser, model.FileInfo, error)
}

// Server owns HTTP deadlines, authentication, limits, and a node backend.
type Server struct {
	http            *http.Server
	backend         Backend
	tokenHash       [sha256.Size]byte
	maxJSON         int64
	maxFile         int64
	timeout         time.Duration
	concurrent      chan struct{}
	connectionLimit int
	ctx             context.Context
	cancel          context.CancelFunc
}

// New constructs a server but does not bind a socket. addr must name a numeric
// IPv4 or IPv6 loopback address, with a numeric TCP port (zero is allowed).
func New(addr, token string, backend Backend, maxJSON, maxFile int64, maxConcurrent int, timeout time.Duration) (*Server, error) {
	if err := validateAddress(addr); err != nil {
		return nil, err
	}
	if len(token) < 32 || len(token) > 4096 || strings.IndexFunc(token, func(r rune) bool { return r < 33 || r > 126 }) >= 0 {
		return nil, errors.New("API token must contain 32 to 4096 printable ASCII characters without spaces")
	}
	if backend == nil {
		return nil, errors.New("API backend is required")
	}
	if maxJSON <= 0 || maxFile <= 0 || maxConcurrent <= 0 || maxConcurrent > (int(^uint(0)>>1)-16)/2 || timeout <= 0 {
		return nil, errors.New("API body limits, concurrency, and timeout must be positive")
	}
	ctx, cancel := context.WithCancel(context.Background())
	s := &Server{
		ctx: ctx, cancel: cancel, connectionLimit: 2*maxConcurrent + 16,
		backend: backend, tokenHash: sha256.Sum256([]byte(token)), maxJSON: maxJSON,
		maxFile: maxFile, timeout: timeout, concurrent: make(chan struct{}, maxConcurrent),
	}
	s.http = &http.Server{
		Addr: addr, Handler: s.Handler(),
		ReadHeaderTimeout: min(timeout, 5*time.Second),
		ReadTimeout:       timeout, WriteTimeout: timeout, IdleTimeout: timeout,
		MaxHeaderBytes: 16 << 10,
		// Request data and credentials must not appear in server diagnostics.
		ErrorLog: log.New(io.Discard, "", 0),
	}
	return s, nil
}

func validateAddress(addr string) error {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return errors.New("API address must be a numeric loopback IP and TCP port")
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return errors.New("API address must use a numeric loopback IP")
	}
	if port == "" || strings.IndexFunc(port, func(r rune) bool { return r < '0' || r > '9' }) >= 0 {
		return errors.New("API address must use a numeric TCP port")
	}
	if _, err := strconv.ParseUint(port, 10, 16); err != nil {
		return errors.New("API TCP port is outside the range 0 to 65535")
	}
	return nil
}

// Serve accepts only an actual loopback TCP listener, even if New was passed a
// different address. At most 2*maxConcurrent+16 TCP connections are accepted,
// counting idle and unauthenticated sockets. Excess sockets wait in the OS
// backlog. It returns http.ErrServerClosed following Shutdown.
func (s *Server) Serve(listener net.Listener) error {
	if listener == nil {
		return errors.New("API listener is required")
	}
	addr, ok := listener.Addr().(*net.TCPAddr)
	if !ok || addr == nil || addr.Zone != "" || !addr.IP.IsLoopback() || addr.Port < 0 || addr.Port > 65535 {
		return errors.New("API listener must be bound to a numeric loopback TCP address")
	}
	return s.http.Serve(newBoundedListener(listener, s.connectionLimit))
}

// Handler returns the protected handler, useful for embedding and testing.
// An embedding server remains responsible for its own loopback binding and
// transport deadlines; normal daemon entry points should use Serve instead.
func (s *Server) Handler() http.Handler { return http.HandlerFunc(s.serveHTTP) }

// Shutdown drains accepted work. If its deadline expires, active operations
// are canceled and transport connections closed before returning the error.
func (s *Server) Shutdown(ctx context.Context) error {
	err := s.http.Shutdown(ctx)
	s.cancel()
	if err != nil {
		_ = s.http.Close()
	}
	return err
}

// Close immediately cancels active operations and closes their connections.
func (s *Server) Close() error {
	s.cancel()
	return s.http.Close()
}

func (s *Server) serveHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if !s.authenticated(r) {
		w.Header().Set("WWW-Authenticate", `Bearer realm="mediatrix"`)
		writeAPIError(w, http.StatusUnauthorized, model.Err("unauthorized", "a valid bearer token is required"))
		return
	}
	for key := range r.Header {
		if strings.EqualFold(key, "Origin") {
			writeAPIError(w, http.StatusForbidden, model.Err("forbidden", "browser Origin headers are not accepted"))
			return
		}
	}
	if !loopbackHost(r.Host) {
		writeError(w, model.Err("forbidden", "Host must be a numeric loopback address"))
		return
	}
	select {
	case s.concurrent <- struct{}{}:
		defer func() { <-s.concurrent }()
	default:
		w.Header().Set("Retry-After", "1")
		writeAPIError(w, http.StatusTooManyRequests, model.Err("busy", "the local API is at its concurrency limit"))
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), s.timeout)
	stopShutdown := context.AfterFunc(s.ctx, cancel)
	defer stopShutdown()
	// net/http Body.Close can wait behind an in-progress Read. Expire the
	// socket's read deadline first, then close the body, so cancellation also
	// interrupts a client that stopped sending a chunked upload. Direct Handler
	// users must provide bodies whose Close unblocks Read (as io.Pipe does).
	bodyStopped := make(chan struct{})
	requestBody := r.Body
	stopBody := context.AfterFunc(ctx, func() {
		_ = http.NewResponseController(w).SetReadDeadline(time.Now())
		_ = requestBody.Close()
		close(bodyStopped)
	})
	defer func() {
		if !stopBody() {
			<-bodyStopped
		}
		cancel()
	}()
	r = r.WithContext(ctx)
	if err := ctx.Err(); err != nil {
		writeError(w, err)
		return
	}
	if r.URL.RawQuery != "" && r.URL.Path != "/v1/services" {
		writeError(w, model.Err("invalid", "this endpoint does not accept query parameters"))
		return
	}
	switch r.URL.Path {
	case "/v1/health":
		if !allowMethod(w, r, http.MethodGet) || !noBody(w, r) {
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	case "/v1/node":
		if !allowMethod(w, r, http.MethodGet) || !noBody(w, r) {
			return
		}
		writeJSON(w, http.StatusOK, s.backend.Info())
	case "/v1/services":
		s.services(w, r)
	case "/v1/call":
		if !allowMethod(w, r, http.MethodPost) {
			return
		}
		var req model.CallRequest
		if !s.readJSON(w, r, &req) {
			return
		}
		result, err := s.backend.Call(ctx, req)
		if err != nil {
			// Cancellation cannot prove that a remote handler did not execute.
			var known *model.Error
			if !errors.As(err, &known) && (errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)) {
				err = &model.Error{Code: "timeout", Message: "call canceled or deadline exceeded", OutcomeUnknown: true}
			}
			writeError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, result)
	case "/v1/files":
		s.upload(w, r)
	case "/v1/files/access":
		if !allowMethod(w, r, http.MethodPut) {
			return
		}
		var req model.AccessRequest
		if !s.readJSON(w, r, &req) || !validKey(w, req.Key) {
			return
		}
		if err := s.backend.SetFileAccess(ctx, req.Key, req.AllowedPeers); err != nil {
			writeError(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	case "/v1/fetch":
		if !allowMethod(w, r, http.MethodPost) {
			return
		}
		var req model.FetchRequest
		if !s.readJSON(w, r, &req) || !validKey(w, req.Key) {
			return
		}
		result, err := s.backend.Fetch(ctx, req)
		if err != nil {
			writeError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, result)
	default:
		if strings.HasPrefix(r.URL.Path, "/v1/files/") {
			s.download(w, r)
			return
		}
		writeError(w, model.Err("not_found", "endpoint not found"))
	}
}

// Checking Host also prevents a browser or proxy from addressing the daemon
// through a DNS name that has been rebound to loopback.
func loopbackHost(host string) bool {
	ipText := host
	if parsedHost, port, err := net.SplitHostPort(host); err == nil {
		if port == "" || strings.IndexFunc(port, func(r rune) bool { return r < '0' || r > '9' }) >= 0 {
			return false
		}
		if _, err := strconv.ParseUint(port, 10, 16); err != nil {
			return false
		}
		ipText = parsedHost
	} else if strings.HasPrefix(host, "[") && strings.HasSuffix(host, "]") {
		ipText = strings.TrimSuffix(strings.TrimPrefix(host, "["), "]")
	}
	ip := net.ParseIP(ipText)
	return ip != nil && ip.IsLoopback()
}

func (s *Server) authenticated(r *http.Request) bool {
	values := r.Header.Values("Authorization")
	var supplied string
	valid := len(values) == 1
	if valid {
		scheme, token, found := strings.Cut(values[0], " ")
		valid = found && strings.EqualFold(scheme, "Bearer") && token != ""
		if valid {
			supplied = token
		}
	}
	digest := sha256.Sum256([]byte(supplied))
	return subtle.ConstantTimeCompare(digest[:], s.tokenHash[:]) == 1 && valid
}

func (s *Server) services(w http.ResponseWriter, r *http.Request) {
	if !allowMethod(w, r, http.MethodGet, http.MethodPut, http.MethodDelete) {
		return
	}
	if r.Method != http.MethodDelete && r.URL.RawQuery != "" {
		writeError(w, model.Err("invalid", "this operation does not accept query parameters"))
		return
	}
	switch r.Method {
	case http.MethodGet:
		if !noBody(w, r) {
			return
		}
		services := s.backend.Services()
		if services == nil {
			services = []model.Service{}
		}
		writeJSON(w, http.StatusOK, services)
	case http.MethodPut:
		var service model.Service
		if !s.readJSON(w, r, &service) {
			return
		}
		if err := model.ValidateService(service.Name); err != nil {
			writeError(w, model.Err("invalid", err.Error()))
			return
		}
		if err := s.backend.RegisterService(r.Context(), service); err != nil {
			writeError(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	case http.MethodDelete:
		if !noBody(w, r) {
			return
		}
		// ParseQuery rejects malformed escapes and semicolon ambiguity.
		query, err := url.ParseQuery(r.URL.RawQuery)
		if err != nil || len(query) != 1 || len(query["name"]) != 1 {
			writeError(w, model.Err("invalid", "exactly one name query parameter is required"))
			return
		}
		name := query.Get("name")
		if err := model.ValidateService(name); err != nil {
			writeError(w, model.Err("invalid", err.Error()))
			return
		}
		if err := s.backend.DeleteService(r.Context(), name); err != nil {
			writeError(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

func (s *Server) readJSON(w http.ResponseWriter, r *http.Request, dst any) bool {
	if r.ContentLength > s.maxJSON {
		writeError(w, model.Err("too_large", "JSON body exceeds the configured limit"))
		return false
	}
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		writeAPIError(w, http.StatusUnsupportedMediaType, model.Err("invalid", "Content-Type must be application/json"))
		return false
	}
	if !unencoded(w, r) {
		return false
	}
	body := http.MaxBytesReader(w, r.Body, s.maxJSON)
	defer body.Close()
	data, err := io.ReadAll(body)
	if err != nil {
		writeBodyError(w, err)
		return false
	}
	data = bytes.TrimSpace(data)
	if len(data) == 0 || data[0] != '{' {
		writeError(w, model.Err("invalid", "request body must be a JSON object"))
		return false
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		writeError(w, model.Err("invalid", "request contains invalid JSON or unknown fields"))
		return false
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		writeError(w, model.Err("invalid", "request must contain exactly one JSON object"))
		return false
	}
	if err := r.Context().Err(); err != nil {
		writeError(w, err)
		return false
	}
	return true
}

func (s *Server) upload(w http.ResponseWriter, r *http.Request) {
	if !allowMethod(w, r, http.MethodPost) {
		return
	}
	if r.ContentLength > s.maxFile {
		writeError(w, model.Err("too_large", "file exceeds the configured limit"))
		return
	}
	if contentType := r.Header.Get("Content-Type"); contentType != "" {
		mediaType, _, err := mime.ParseMediaType(contentType)
		if err != nil || mediaType != "application/octet-stream" {
			writeAPIError(w, http.StatusUnsupportedMediaType, model.Err("invalid", "Content-Type must be application/octet-stream"))
			return
		}
	}
	if !unencoded(w, r) {
		return
	}
	body := http.MaxBytesReader(w, r.Body, s.maxFile)
	defer body.Close()
	reader := &trackedReader{reader: body, ctx: r.Context()}
	info, err := s.backend.Upload(r.Context(), reader)
	if reader.err != nil {
		writeBodyError(w, reader.err)
		return
	}
	if err != nil {
		writeError(w, err)
		return
	}
	// Defend against a backend returning without consuming the complete body.
	if _, err := io.Copy(io.Discard, reader); err != nil {
		writeBodyError(w, err)
		return
	}
	if err := r.Context().Err(); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, info)
}

type trackedReader struct {
	reader io.Reader
	ctx    context.Context
	err    error
}

func (r *trackedReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		r.err = err
		return 0, err
	}
	n, err := r.reader.Read(p)
	if contextErr := r.ctx.Err(); contextErr != nil {
		err = contextErr
	}
	if err != nil && err != io.EOF {
		r.err = err
	}
	return n, err
}

func (s *Server) download(w http.ResponseWriter, r *http.Request) {
	if !allowMethod(w, r, http.MethodGet) || !noBody(w, r) {
		return
	}
	key := model.FileKey(strings.TrimPrefix(r.URL.Path, "/v1/files/"))
	if !validKey(w, key) {
		return
	}
	stream, info, err := s.backend.OpenFile(key)
	if err != nil {
		if stream != nil {
			stream.Close()
		}
		writeError(w, err)
		return
	}
	if stream == nil {
		writeError(w, model.Err("unavailable", "file stream is unavailable"))
		return
	}
	var once sync.Once
	closeStream := func() { once.Do(func() { stream.Close() }) }
	defer closeStream()
	if info.Size < 0 || info.Key != key {
		writeError(w, model.Err("unavailable", "file metadata verification failed"))
		return
	}
	if info.Size > s.maxFile {
		writeError(w, model.Err("too_large", "file exceeds the configured limit"))
		return
	}
	if err := r.Context().Err(); err != nil {
		writeError(w, err)
		return
	}
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-r.Context().Done():
			closeStream()
		case <-done:
		}
	}()
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", strconv.FormatInt(info.Size, 10))
	w.WriteHeader(http.StatusOK)
	// The backend has already verified the file; no bytes beyond its verified
	// size are exposed, and all paths (including broken clients) close it.
	_, _ = io.CopyN(w, stream, info.Size)
}

func allowMethod(w http.ResponseWriter, r *http.Request, methods ...string) bool {
	for _, method := range methods {
		if r.Method == method {
			return true
		}
	}
	w.Header().Set("Allow", strings.Join(methods, ", "))
	writeAPIError(w, http.StatusMethodNotAllowed, model.Err("invalid", "method is not allowed for this endpoint"))
	return false
}

func noBody(w http.ResponseWriter, r *http.Request) bool {
	if r.ContentLength != 0 || len(r.TransferEncoding) != 0 {
		writeError(w, model.Err("invalid", "this endpoint does not accept a request body"))
		return false
	}
	return true
}

func unencoded(w http.ResponseWriter, r *http.Request) bool {
	if r.Header.Get("Content-Encoding") != "" && !strings.EqualFold(r.Header.Get("Content-Encoding"), "identity") {
		writeAPIError(w, http.StatusUnsupportedMediaType, model.Err("invalid", "encoded request bodies are not supported"))
		return false
	}
	return true
}

func validKey(w http.ResponseWriter, key string) bool {
	if _, err := model.FileDigest(key); err != nil {
		writeError(w, model.Err("invalid", "invalid file key"))
		return false
	}
	return true
}

func writeBodyError(w http.ResponseWriter, err error) {
	var limit *http.MaxBytesError
	if errors.As(err, &limit) {
		writeError(w, model.Err("too_large", "request body exceeds the configured limit"))
		return
	}
	var timeout net.Error
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &timeout) && timeout.Timeout()) {
		writeError(w, model.Err("timeout", "request canceled or deadline exceeded"))
		return
	}
	writeError(w, model.Err("invalid", "could not read request body"))
}

func writeError(w http.ResponseWriter, err error) {
	var apiErr *model.Error
	var bodyLimit *http.MaxBytesError
	var timeout net.Error
	switch {
	case errors.As(err, &bodyLimit):
		apiErr = model.Err("too_large", "request body exceeds the configured limit")
	case errors.As(err, &apiErr):
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		apiErr = model.Err("timeout", "request canceled or deadline exceeded")
	case errors.As(err, &timeout) && timeout.Timeout():
		apiErr = model.Err("timeout", "request deadline exceeded")
	default:
		// Do not expose internal paths, payloads, or credentials via errors.
		apiErr = model.Err("unavailable", "backend operation failed")
	}
	status := http.StatusBadGateway
	switch apiErr.Code {
	case "invalid":
		status = http.StatusBadRequest
	case "forbidden":
		status = http.StatusForbidden
	case "not_found":
		status = http.StatusNotFound
	case "conflict":
		status = http.StatusConflict
	case "too_large":
		status = http.StatusRequestEntityTooLarge
	case "busy":
		status = http.StatusTooManyRequests
	case "timeout":
		status = http.StatusGatewayTimeout
	}
	writeAPIError(w, status, apiErr)
}

func writeAPIError(w http.ResponseWriter, status int, err *model.Error) {
	writeJSON(w, status, struct {
		Error *model.Error `json:"error"`
	}{Error: err})
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	data, err := json.Marshal(value)
	if err != nil {
		status = http.StatusBadGateway
		data = []byte(`{"error":{"code":"unavailable","message":"backend response could not be encoded"}}`)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = fmt.Fprintln(w, string(data))
}
