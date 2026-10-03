// mediatrix is a bounded loopback-only client for an already-running daemon.
package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/ictusidera/mediatrix/internal/config"
	"github.com/ictusidera/mediatrix/internal/model"
)

const maxJSON = 16 << 20
const maxFile = 1 << 30

func main() {
	if err := run(os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "mediatrix:", err)
		os.Exit(1)
	}
}

type apiClient struct {
	base   string
	token  string
	client *http.Client
}
type stringsFlag []string

func (v *stringsFlag) String() string     { return strings.Join(*v, ",") }
func (v *stringsFlag) Set(s string) error { *v = append(*v, s); return nil }

func run(args []string, out, errOut io.Writer) error {
	global := flag.NewFlagSet("mediatrix", flag.ContinueOnError)
	global.SetOutput(errOut)
	api := global.String("api", "http://127.0.0.1:47832", "daemon's numeric-loopback HTTP URL")
	tokenFile := global.String("token-file", "", "private file containing the API token (otherwise MEDIATRIX_API_TOKEN)")
	timeout := global.Duration("timeout", 45*time.Second, "HTTP request timeout, up to 310s")
	global.Usage = func() {
		fmt.Fprintln(errOut, "Usage: mediatrix [--api URL] [--token-file FILE] [--timeout 45s] COMMAND [FLAGS]\nCommands: node, services, register-service, remove-service, call, upload, grant-file, fetch\nGlobal flags precede COMMAND. Every command talks to an existing daemon.")
		global.PrintDefaults()
	}
	if err := global.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	rest := global.Args()
	if len(rest) == 0 {
		global.Usage()
		return errors.New("a command is required")
	}
	if rest[0] == "help" {
		global.Usage()
		return nil
	}
	if *timeout < time.Second || *timeout > 310*time.Second {
		return errors.New("--timeout must be between 1s and 310s")
	}
	base, err := validateAPI(*api)
	if err != nil {
		return err
	}
	// Parse a command completely before loading credentials or contacting the API.
	fs := flag.NewFlagSet(rest[0], flag.ContinueOnError)
	fs.SetOutput(errOut)
	var name, handler, service, method, params, requestID, file, key, output string
	var timeoutMS int64
	var allowed stringsFlag
	switch rest[0] {
	case "node", "services":
	case "register-service":
		fs.StringVar(&name, "name", "", "service:<name>")
		fs.StringVar(&handler, "url", "", "local HTTP handler URL")
		fs.Var(&allowed, "allow-peer", "authorized caller peer ID; repeat for each peer")
	case "remove-service":
		fs.StringVar(&name, "name", "", "service:<name>")
	case "call":
		fs.StringVar(&service, "service", "", "service:<name>")
		fs.StringVar(&method, "method", "", "handler method")
		fs.StringVar(&params, "params", "null", "JSON parameters")
		fs.Int64Var(&timeoutMS, "timeout-ms", 0, "remote execution timeout in milliseconds (0 uses daemon default)")
		fs.StringVar(&requestID, "request-id", "", "optional correlation ID (does not deduplicate calls)")
	case "upload":
		fs.StringVar(&file, "file", "", "regular file to upload")
	case "grant-file":
		fs.StringVar(&key, "key", "", "file:sha256:<digest>")
		fs.Var(&allowed, "allow-peer", "authorized reader peer ID; repeat; omit all to revoke access")
	case "fetch":
		fs.StringVar(&key, "key", "", "file:sha256:<digest>")
		fs.StringVar(&output, "out", "", "new output filename (existing files are never replaced)")
		fs.Int64Var(&timeoutMS, "timeout-ms", 0, "peer fetch timeout in milliseconds (0 uses daemon default)")
	default:
		return fmt.Errorf("unknown command %q", rest[0])
	}
	if err := fs.Parse(rest[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("unexpected positional arguments")
	}
	token, err := config.LoadToken(*tokenFile)
	if err != nil {
		return err
	}
	transport := &http.Transport{Proxy: nil, MaxIdleConns: 2, MaxIdleConnsPerHost: 2, ResponseHeaderTimeout: *timeout}
	defer transport.CloseIdleConnections()
	c := apiClient{base: base, token: token, client: &http.Client{Transport: transport, Timeout: *timeout, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
	ctx := context.Background()
	switch rest[0] {
	case "node":
		return c.printJSON(ctx, http.MethodGet, "/v1/node", nil, out)
	case "services":
		return c.printJSON(ctx, http.MethodGet, "/v1/services", nil, out)
	case "register-service":
		if err := model.ValidateService(name); err != nil {
			return err
		}
		if handler == "" {
			return errors.New("--url is required")
		}
		if allowed == nil {
			allowed = stringsFlag{}
		}
		return c.printJSON(ctx, http.MethodPut, "/v1/services", model.Service{Name: name, URL: handler, AllowedPeers: allowed}, out)
	case "remove-service":
		if err := model.ValidateService(name); err != nil {
			return err
		}
		return c.printJSON(ctx, http.MethodDelete, "/v1/services?name="+url.QueryEscape(name), nil, out)
	case "call":
		if err := model.ValidateService(service); err != nil {
			return err
		}
		if method == "" {
			return errors.New("--method is required")
		}
		if len(params) > maxJSON || !json.Valid([]byte(params)) {
			return errors.New("--params must be valid JSON, at most 16 MiB")
		}
		if timeoutMS < 0 || timeoutMS > 300000 {
			return errors.New("--timeout-ms must be between 0 and 300000")
		}
		data, err := c.jsonRequest(ctx, http.MethodPost, "/v1/call", model.CallRequest{Service: service, Method: method, Params: json.RawMessage(params), RequestID: requestID, TimeoutMS: timeoutMS})
		if err != nil {
			return err
		}
		var result model.CallResponse
		if err := json.Unmarshal(data, &result); err != nil {
			return callOutcomeUnknown(err)
		}
		if result.RequestID == "" || (requestID != "" && result.RequestID != requestID) || (len(result.Result) == 0) == (result.Error == nil) {
			return callOutcomeUnknown(errors.New("daemon returned an invalid or mismatched call response"))
		}
		if err := writePrettyJSON(out, data); err != nil {
			return err
		}
		if result.Error != nil {
			return describeAPIError(result.Error)
		}
		return nil
	case "upload":
		return c.upload(ctx, file, out)
	case "grant-file":
		if _, err := model.FileDigest(key); err != nil {
			return err
		}
		if allowed == nil {
			allowed = stringsFlag{}
		}
		return c.printJSON(ctx, http.MethodPut, "/v1/files/access", model.AccessRequest{Key: key, AllowedPeers: allowed}, out)
	case "fetch":
		if timeoutMS < 0 || timeoutMS > 300000 {
			return errors.New("--timeout-ms must be between 0 and 300000")
		}
		return c.fetch(ctx, key, output, timeoutMS, out)
	}
	return nil
}

func validateAPI(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", errors.New("invalid --api URL")
	}
	if u.Scheme != "http" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Opaque != "" || (u.Path != "" && u.Path != "/") || u.RawPath != "" {
		return "", errors.New("--api must be an http:// numeric-loopback IP:port URL with no credentials, path, query or fragment")
	}
	if err := config.ValidateAPIAddr(u.Host); err != nil {
		return "", err
	}
	if u.Port() == "0" {
		return "", errors.New("--api port must not be zero")
	}
	return "http://" + u.Host, nil
}

func (c *apiClient) request(ctx context.Context, method, path string, body io.Reader, contentType string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/json")
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := c.client.Do(req)
	if err != nil {
		if method == http.MethodPost && path == "/v1/call" {
			return nil, callOutcomeUnknown(err)
		}
		return nil, fmt.Errorf("daemon request failed: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		defer resp.Body.Close()
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		// API errors may be bare model.Error or wrapped under error.
		var wrapped struct {
			Error *model.Error `json:"error"`
		}
		var direct model.Error
		if json.Unmarshal(data, &wrapped) == nil && wrapped.Error != nil && wrapped.Error.Code != "" {
			return nil, describeAPIError(wrapped.Error)
		}
		if json.Unmarshal(data, &direct) == nil && direct.Code != "" {
			return nil, describeAPIError(&direct)
		}
		return nil, fmt.Errorf("daemon returned HTTP %d", resp.StatusCode)
	}
	return resp, nil
}

func (c *apiClient) jsonRequest(ctx context.Context, method, path string, payload any) ([]byte, error) {
	var body io.Reader
	if payload != nil {
		data, err := json.Marshal(payload)
		if err != nil {
			return nil, err
		}
		if len(data) > maxJSON {
			return nil, errors.New("request JSON exceeds 16 MiB")
		}
		body = bytes.NewReader(data)
	}
	resp, err := c.request(ctx, method, path, body, "application/json")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, err := readBounded(resp.Body, maxJSON)
	if err == nil && (len(data) > 0 || path == "/v1/call") && !json.Valid(data) {
		err = errors.New("daemon returned invalid JSON")
	}
	if err != nil {
		if method == http.MethodPost && path == "/v1/call" {
			return nil, callOutcomeUnknown(err)
		}
		return nil, err
	}
	return data, nil
}

func (c *apiClient) printJSON(ctx context.Context, method, path string, payload any, out io.Writer) error {
	data, err := c.jsonRequest(ctx, method, path, payload)
	if err != nil {
		return err
	}
	return writePrettyJSON(out, data)
}
func writePrettyJSON(out io.Writer, data []byte) error {
	if len(data) == 0 {
		return nil
	}
	var pretty bytes.Buffer
	if err := json.Indent(&pretty, data, "", "  "); err != nil {
		return err
	}
	pretty.WriteByte('\n')
	_, err := out.Write(pretty.Bytes())
	return err
}
func readBounded(r io.Reader, limit int64) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, errors.New("daemon response exceeds size limit")
	}
	return data, nil
}

func (c *apiClient) upload(ctx context.Context, path string, out io.Writer) error {
	if path == "" {
		return errors.New("--file is required")
	}
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	st, err := file.Stat()
	if err != nil {
		return err
	}
	if !st.Mode().IsRegular() {
		return errors.New("upload source must be a regular file")
	}
	if st.Size() > maxFile {
		return errors.New("file exceeds the client's 1 GiB limit")
	}
	resp, err := c.request(ctx, http.MethodPost, "/v1/files", io.LimitReader(file, maxFile+1), "application/octet-stream")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, err := readBounded(resp.Body, maxJSON)
	if err != nil {
		return err
	}
	return writePrettyJSON(out, data)
}

func (c *apiClient) fetch(ctx context.Context, key, output string, timeoutMS int64, out io.Writer) error {
	digest, err := model.FileDigest(key)
	if err != nil {
		return err
	}
	if output == "" {
		return errors.New("--out is required")
	}
	if _, err := os.Lstat(output); err == nil {
		return errors.New("output already exists; choose a new --out path")
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	data, err := c.jsonRequest(ctx, http.MethodPost, "/v1/fetch", model.FetchRequest{Key: key, TimeoutMS: timeoutMS})
	if err != nil {
		return err
	}
	var info model.FileInfo
	if err := json.Unmarshal(data, &info); err != nil {
		return err
	}
	if info.Key != key || info.Size < 0 || info.Size > maxFile {
		return errors.New("daemon returned inconsistent or oversized file metadata")
	}
	resp, err := c.request(ctx, http.MethodGet, "/v1/files/"+digest, nil, "")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.ContentLength >= 0 && resp.ContentLength != info.Size {
		return errors.New("download length does not match file metadata")
	}
	temp, err := os.CreateTemp(filepath.Dir(output), ".mediatrix-download-*")
	if err != nil {
		return err
	}
	tempPath := temp.Name()
	defer os.Remove(tempPath)
	defer temp.Close()
	hash := sha256.New()
	n, err := io.Copy(io.MultiWriter(temp, hash), io.LimitReader(resp.Body, info.Size+1))
	if err != nil {
		return fmt.Errorf("download interrupted: %w", err)
	}
	if n != info.Size {
		return errors.New("download size mismatch")
	}
	if hex.EncodeToString(hash.Sum(nil)) != digest {
		return errors.New("download SHA-256 mismatch")
	}
	if err := temp.Sync(); err != nil {
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	// A hardlink atomically publishes a fully verified file without overwriting
	// an output created after Lstat. Rename alone is unsafe on Unix for this case.
	if err := os.Link(tempPath, output); err != nil {
		return fmt.Errorf("publish download without overwriting: %w", err)
	}
	return writePrettyJSON(out, data)
}

func describeAPIError(err *model.Error) error {
	if err.OutcomeUnknown {
		return fmt.Errorf("%w (remote outcome unknown; do not assume the handler failed)", err)
	}
	return err
}

func callOutcomeUnknown(err error) error {
	return fmt.Errorf("call outcome unknown; do not assume the remote handler failed: %w", err)
}
