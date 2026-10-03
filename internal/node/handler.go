package node

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/ictusidera/mediatrix/internal/model"
)

func validateService(s model.Service) error {
	if err := model.ValidateService(s.Name); err != nil {
		return err
	}
	u, e := url.Parse(s.URL)
	if e != nil || u.Scheme != "http" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Opaque != "" {
		return fmt.Errorf("handler URL must be loopback HTTP without credentials, query or fragment")
	}
	ip := net.ParseIP(u.Hostname())
	p, e := strconv.Atoi(u.Port())
	if ip == nil || !ip.IsLoopback() || e != nil || p < 1 || p > 65535 {
		return fmt.Errorf("handler URL needs numeric loopback IP and explicit port")
	}
	return nil
}
func (n *Node) validateACL(peers []string) error {
	if len(peers) > 128 {
		return model.Err("invalid", "too many ACL peers")
	}
	seen := map[string]bool{}
	for _, p := range peers {
		if !n.allowedString[p] || seen[p] {
			return model.Err("invalid", "ACL peers must be unique members of allowed_peers")
		}
		seen[p] = true
	}
	return nil
}
func permits(acl []string, p string) bool {
	for _, a := range acl {
		if a == p {
			return true
		}
	}
	return false
}
func (n *Node) invoke(ctx context.Context, caller string, req model.CallRequest) (model.CallResponse, error) {
	response := model.CallResponse{RequestID: req.RequestID}
	svc, ok := n.store.GetService(req.Service)
	if !ok || (caller != n.host.ID().String() && !permits(svc.AllowedPeers, caller)) {
		return response, model.Err("not_found", "service unavailable")
	}
	body, err := json.Marshal(model.HandlerRequest{CallRequest: req, CallerPeer: caller})
	if err != nil {
		return response, err
	}
	if int64(len(body)) > n.cfg.Limits.MaxJSONBytes {
		return response, model.Err("too_large", "handler request too large")
	}
	hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, svc.URL, bytes.NewReader(body))
	if err != nil {
		return response, err
	}
	hreq.Header.Set("Content-Type", "application/json")
	hresp, err := n.handler.Do(hreq)
	if err != nil {
		return response, &model.Error{Code: "handler_failed", Message: "handler request failed or canceled", OutcomeUnknown: true}
	}
	defer hresp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(hresp.Body, n.cfg.Limits.MaxJSONBytes+1))
	if err != nil || int64(len(b)) > n.cfg.Limits.MaxJSONBytes {
		return response, &model.Error{Code: "handler_failed", Message: "invalid or excessive handler response", OutcomeUnknown: true}
	}
	if hresp.StatusCode != http.StatusOK {
		return response, &model.Error{Code: "handler_failed", Message: "handler returned non-200 status", OutcomeUnknown: true}
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	var hr struct {
		Result json.RawMessage `json:"result,omitempty"`
		Error  *model.Error    `json:"error,omitempty"`
	}
	if err := dec.Decode(&hr); err != nil || dec.Decode(new(any)) != io.EOF || (len(hr.Result) == 0) == (hr.Error == nil) {
		return response, &model.Error{Code: "handler_failed", Message: "handler must return exactly one result or error", OutcomeUnknown: true}
	}
	response.Result, response.Error = hr.Result, hr.Error
	if hr.Error != nil { // Error text is application supplied; cap fields within the already bounded frame.
		if len(hr.Error.Code) > 128 || len(hr.Error.Message) > 4096 {
			return response, &model.Error{Code: "handler_failed", Message: "handler error fields too long", OutcomeUnknown: true}
		}
	}
	return response, nil
}
func validateCall(r model.CallRequest) error {
	if e := model.ValidateService(r.Service); e != nil {
		return e
	}
	if len(r.Method) == 0 || len(r.Method) > 128 || strings.ContainsAny(r.Method, "\r\n\x00") {
		return fmt.Errorf("invalid method")
	}
	if len(r.RequestID) > 128 || strings.ContainsAny(r.RequestID, "\r\n\x00") {
		return fmt.Errorf("invalid request_id")
	}
	if r.TimeoutMS < 0 {
		return fmt.Errorf("timeout_ms must be nonnegative")
	}
	if !json.Valid(r.Params) {
		return fmt.Errorf("params must be valid JSON")
	}
	return nil
}
