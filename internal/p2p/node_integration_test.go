package p2p

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/s-yamamoto/mediatrix/internal/config"
	"github.com/s-yamamoto/mediatrix/internal/wire"
)

func TestLocalDHTRPCAndFileTransfer(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()

	root := t.TempDir()
	namespace := "mediatrix-test"

	bootstrap, err := New(ctx, testConfig(root, "bootstrap", namespace, nil), Options{Role: "relay"})
	if err != nil {
		t.Fatalf("start bootstrap: %v", err)
	}
	defer bootstrap.Close()

	bootAddrs := bootstrap.ListenAddrs()
	provider, err := New(ctx, testConfig(root, "provider", namespace, bootAddrs), Options{})
	if err != nil {
		t.Fatalf("start provider: %v", err)
	}
	defer provider.Close()

	client, err := New(ctx, testConfig(root, "client", namespace, bootAddrs), Options{})
	if err != nil {
		t.Fatalf("start client: %v", err)
	}
	defer client.Close()

	if err := provider.RegisterService(ctx, config.ServiceConfig{
		Name:    "service:echo",
		Command: []string{"builtin:echo"},
	}); err != nil {
		t.Fatalf("register service: %v", err)
	}

	resp := retryRPC(t, ctx, client, "service:echo", "Echo", `{"message":"hello"}`)
	if !resp.OK {
		t.Fatalf("rpc returned remote error: %s", resp.Error)
	}
	if resp.Result != `{"message":"hello"}` {
		t.Fatalf("rpc result mismatch: got %s", resp.Result)
	}

	src := filepath.Join(root, "sample.txt")
	body := []byte("mediatrix integration file\n")
	if err := os.WriteFile(src, body, 0o644); err != nil {
		t.Fatalf("write sample file: %v", err)
	}
	if _, err := provider.RegisterFile(ctx, src, "sample.txt"); err != nil {
		t.Fatalf("register file: %v", err)
	}
	sum := sha256.Sum256(body)
	key := "file:sha256:" + hex.EncodeToString(sum[:])
	dst := filepath.Join(root, "download.txt")
	retryFetch(t, ctx, client, key, dst)

	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("read downloaded file: %v", err)
	}
	if string(got) != string(body) {
		t.Fatalf("downloaded body mismatch: got %q", string(got))
	}
}

func testConfig(root, name, namespace string, bootstraps []string) config.Config {
	cfg := config.Default()
	cfg.Node.IdentityPath = filepath.Join(root, name, "identity.key")
	cfg.Node.DatastorePath = filepath.Join(root, name, "state.db")
	cfg.Node.Listen = []string{
		"/ip4/127.0.0.1/tcp/0",
		"/ip4/127.0.0.1/udp/0/quic-v1",
	}
	cfg.Network.BootstrapPeers = bootstraps
	cfg.Network.DHT.Mode = "server"
	cfg.Network.DHT.Namespace = namespace
	cfg.Network.NAT.PortMap = false
	cfg.Network.NAT.AutoNAT = false
	cfg.Network.NAT.HolePunch = false
	return cfg
}

func retryRPC(t *testing.T, ctx context.Context, n *Node, service, method, params string) wire.RPCResponse {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		resp, err := n.Call(ctx, service, method, params)
		if err == nil {
			return resp
		}
		lastErr = err
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("rpc did not succeed: %v", lastErr)
	return wire.RPCResponse{}
}

func retryFetch(t *testing.T, ctx context.Context, n *Node, key, dst string) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		err := n.FetchFile(ctx, key, dst)
		if err == nil {
			return
		}
		lastErr = err
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("fetch did not succeed: %v", lastErr)
}
