// Package model defines the versioned daemon API and peer protocol values.
package model

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

const Version = "0.2.0-dev"

var servicePattern = regexp.MustCompile(`^service:[a-zA-Z0-9][a-zA-Z0-9._/-]{0,127}$`)
var digestPattern = regexp.MustCompile(`^[a-f0-9]{64}$`)

func ValidateService(name string) error {
	if !servicePattern.MatchString(name) {
		return fmt.Errorf("invalid service name: use service:<name>, at most 128 name characters")
	}
	return nil
}
func FileDigest(key string) (string, error) {
	d := strings.TrimPrefix(key, "file:sha256:")
	if d == key || !digestPattern.MatchString(d) {
		return "", fmt.Errorf("invalid file key")
	}
	return d, nil
}
func FileKey(digest string) string { return "file:sha256:" + digest }

type Service struct {
	Name         string   `json:"name"`
	URL          string   `json:"url"`
	AllowedPeers []string `json:"allowed_peers"`
}
type FileInfo struct {
	Key          string   `json:"key"`
	Size         int64    `json:"size"`
	AllowedPeers []string `json:"allowed_peers"`
}
type CallRequest struct {
	Service   string          `json:"service"`
	Method    string          `json:"method"`
	Params    json.RawMessage `json:"params"`
	RequestID string          `json:"request_id,omitempty"`
	TimeoutMS int64           `json:"timeout_ms,omitempty"`
}
type CallResponse struct {
	RequestID string          `json:"request_id"`
	Result    json.RawMessage `json:"result,omitempty"`
	Error     *Error          `json:"error,omitempty"`
}
type Error struct {
	Code           string `json:"code"`
	Message        string `json:"message"`
	OutcomeUnknown bool   `json:"outcome_unknown,omitempty"`
}

func (e *Error) Error() string        { return e.Code + ": " + e.Message }
func Err(code, message string) *Error { return &Error{Code: code, Message: message} }

type FetchRequest struct {
	Key       string `json:"key"`
	TimeoutMS int64  `json:"timeout_ms,omitempty"`
}
type AccessRequest struct {
	Key          string   `json:"key"`
	AllowedPeers []string `json:"allowed_peers"`
}
type NodeInfo struct {
	Version        string   `json:"version"`
	PeerID         string   `json:"peer_id"`
	Addrs          []string `json:"addrs"`
	ConnectedPeers int      `json:"connected_peers"`
	Services       int      `json:"services"`
	Files          int      `json:"files"`
	Network        string   `json:"network"`
}

// HandlerRequest is sent only to an operator-registered loopback HTTP service.
// CallerPeer is authenticated by the daemon, never taken from caller input.
type HandlerRequest struct {
	CallRequest
	CallerPeer string `json:"caller_peer"`
}
