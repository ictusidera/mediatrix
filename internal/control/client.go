package control

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

type Client struct {
	baseURL string
	token   string
	http    *http.Client
}

func NewClient(addr, token string) *Client {
	baseURL := addr
	if !strings.HasPrefix(baseURL, "http://") && !strings.HasPrefix(baseURL, "https://") {
		baseURL = "http://" + baseURL
	}
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		token:   token,
		http: &http.Client{
			Timeout: 30 * time.Second,
		},
	}
}

type NodeInfo struct {
	PeerID string   `json:"peer_id"`
	Addrs  []string `json:"addrs"`
}

func (c *Client) Node(ctx context.Context) (NodeInfo, error) {
	var out NodeInfo
	err := c.do(ctx, http.MethodGet, "/v1/node", nil, &out)
	return out, err
}

func (c *Client) PublishService(ctx context.Context, name string, command []string) (string, error) {
	req := map[string]any{"name": name, "command": command}
	var out struct {
		Name string `json:"name"`
	}
	err := c.do(ctx, http.MethodPost, "/v1/services", req, &out)
	return out.Name, err
}

func (c *Client) PublishFile(ctx context.Context, path, name string) (string, error) {
	req := map[string]any{"path": path, "name": name}
	var out struct {
		Key string `json:"key"`
	}
	err := c.do(ctx, http.MethodPost, "/v1/files", req, &out)
	return out.Key, err
}

func (c *Client) do(ctx context.Context, method, path string, body any, out any) error {
	var reader *bytes.Reader
	if body == nil {
		reader = bytes.NewReader(nil)
	} else {
		data, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, reader)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("content-type", "application/json")
	}
	if c.token != "" {
		req.Header.Set("authorization", "Bearer "+c.token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var errResp struct {
			Error string `json:"error"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&errResp)
		if errResp.Error == "" {
			errResp.Error = resp.Status
		}
		return fmt.Errorf("control api error: %s", errResp.Error)
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}
