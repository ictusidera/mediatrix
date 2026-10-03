package node

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ictusidera/mediatrix/internal/config"
	"github.com/ictusidera/mediatrix/internal/model"
	"github.com/libp2p/go-libp2p/core/crypto"
	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
	relayclient "github.com/libp2p/go-libp2p/p2p/protocol/circuitv2/client"
	ma "github.com/multiformats/go-multiaddr"
)

func testConfig(t *testing.T) (config.Config, peer.ID) {
	t.Helper()
	c := config.Default()
	c.DataDir = t.TempDir()
	c.Network = "test-net"
	c.Listen = []string{"/ip4/127.0.0.1/tcp/0"}
	c.Limits.TimeoutSeconds = 3
	c.DHTServer = true
	c.Limits.MaxFileBytes = 1024 * 1024
	c.Limits.MaxStoreBytes = 4 * 1024 * 1024
	k, _, err := crypto.GenerateEd25519Key(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := crypto.MarshalPrivateKey(k)
	if err = os.WriteFile(filepath.Join(c.DataDir, "identity.key"), b, 0600); err != nil {
		t.Fatal(err)
	}
	id, _ := peer.IDFromPrivateKey(k)
	return c, id
}
func startNode(t *testing.T, c config.Config) *Node {
	t.Helper()
	n, err := New(context.Background(), c, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if e := n.Close(); e != nil {
			t.Error(e)
		}
	})
	return n
}
func pair(t *testing.T) (*Node, *Node) {
	t.Helper()
	a, aid := testConfig(t)
	b, bid := testConfig(t)
	a.AllowedPeers = []string{bid.String()}
	b.AllowedPeers = []string{aid.String()}
	an := startNode(t, a)
	b.Bootstrap = an.Info().Addrs
	bn := startNode(t, b)
	return an, bn
}
func register(t *testing.T, n *Node, url string, acl []string) {
	t.Helper()
	if err := n.RegisterService(context.Background(), model.Service{Name: "service:echo", URL: url, AllowedPeers: acl}); err != nil {
		t.Fatal(err)
	}
}
func echoServer(t *testing.T) *httptest.Server {
	t.Helper()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var q model.HandlerRequest
		if err := json.NewDecoder(r.Body).Decode(&q); err != nil {
			t.Error(err)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"result": map[string]any{"params": q.Params, "caller": q.CallerPeer, "request_id": q.RequestID}})
	}))
	t.Cleanup(s.Close)
	return s
}
func call() model.CallRequest {
	return model.CallRequest{Service: "service:echo", Method: "Echo", Params: json.RawMessage(`{"hello":"world"}`), RequestID: "integration-1"}
}

func TestTwoNodeRPCAndFile(t *testing.T) {
	a, b := pair(t)
	srv := echoServer(t)
	register(t, a, srv.URL, []string{b.Info().PeerID})
	r, e := b.Call(context.Background(), call())
	if e != nil || r.Error != nil {
		t.Fatalf("call: %+v %v", r, e)
	}
	var data struct {
		Caller string          `json:"caller"`
		Params json.RawMessage `json:"params"`
	}
	if json.Unmarshal(r.Result, &data) != nil || data.Caller != b.Info().PeerID || !bytes.Contains(data.Params, []byte("world")) {
		t.Fatalf("wrong response: %s", r.Result)
	}
	content := bytes.Repeat([]byte("hello-file\x00"), 1000)
	info, e := a.Upload(context.Background(), bytes.NewReader(content))
	if e != nil {
		t.Fatal(e)
	}
	// An unpublished key does not leak its existence via a probe or file request.
	if a.available(info.Key, b.Info().PeerID) {
		t.Fatal("unshared file was discoverable")
	}
	if e = a.SetFileAccess(context.Background(), info.Key, []string{b.Info().PeerID}); e != nil {
		t.Fatal(e)
	}
	got, e := b.Fetch(context.Background(), model.FetchRequest{Key: info.Key})
	if e != nil {
		t.Fatal(e)
	}
	if got.Size != int64(len(content)) || len(got.AllowedPeers) != 0 {
		t.Fatalf("bad fetched metadata: %+v", got)
	}
	f, _, e := b.OpenFile(info.Key)
	if e != nil {
		t.Fatal(e)
	}
	body, _ := io.ReadAll(f)
	_ = f.Close()
	if !bytes.Equal(body, content) {
		t.Fatal("content mismatch")
	}
}
func TestACLAndUntrustedPeer(t *testing.T) {
	a, b := pair(t)
	srv := echoServer(t)
	register(t, a, srv.URL, nil)
	_, e := b.Call(context.Background(), call())
	var api *model.Error
	if !errors.As(e, &api) || api.Code != "not_found" {
		t.Fatalf("denied call: %v", e)
	}
	rogueCfg, _ := testConfig(t)
	rogueCfg.AllowedPeers = []string{a.Info().PeerID}
	rogue := startNode(t, rogueCfg)
	ai, _ := peer.AddrInfoFromString(a.Info().Addrs[0])
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if e = rogue.host.Connect(ctx, *ai); e == nil { // Some transports report an outbound connection before remote gate closes.
		s, e := rogue.host.NewStream(ctx, ai.ID, rpcProtocol)
		if e == nil {
			_ = s.Reset()
			t.Fatal("untrusted peer opened application stream")
		}
	}
	if e = a.RegisterService(context.Background(), model.Service{Name: "service:bad", URL: "http://example.com:80/", AllowedPeers: nil}); e == nil {
		t.Fatal("SSRF URL accepted")
	}
}
func TestRPCCancelReachesHandlerAndNoRetry(t *testing.T) {
	a, b := pair(t)
	started := make(chan struct{}, 1)
	canceled := make(chan struct{}, 1)
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		calls.Add(1)
		started <- struct{}{}
		<-r.Context().Done()
		canceled <- struct{}{}
	}))
	defer srv.Close()
	register(t, a, srv.URL, []string{b.Info().PeerID})
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() { _, e := b.Call(ctx, call()); result <- e }()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("handler never started")
	}
	cancel()
	select {
	case e := <-result:
		var api *model.Error
		if !errors.As(e, &api) || !api.OutcomeUnknown {
			t.Fatalf("expected unknown outcome, got %v", e)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("caller did not cancel")
	}
	select {
	case <-canceled:
	case <-time.After(2 * time.Second):
		t.Fatal("handler did not receive cancellation")
	}
	if calls.Load() != 1 {
		t.Fatalf("duplicated call: %d", calls.Load())
	}
}
func TestTimeoutAndBusy(t *testing.T) {
	c, _ := testConfig(t)
	c.Limits.MaxConcurrent = 1
	n := startNode(t, c)
	started := make(chan struct{}, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		started <- struct{}{}
		<-r.Context().Done()
	}))
	defer srv.Close()
	register(t, n, srv.URL, nil)
	req := call()
	req.TimeoutMS = 150
	result := make(chan error, 1)
	go func() { _, e := n.Call(context.Background(), req); result <- e }()
	<-started
	_, e := n.Call(context.Background(), call())
	var api *model.Error
	if !errors.As(e, &api) || api.Code != "busy" {
		t.Fatalf("expected busy, got %v", e)
	}
	select {
	case e := <-result:
		if !errors.As(e, &api) || !api.OutcomeUnknown {
			t.Fatalf("timeout error: %v", e)
		}
	case <-time.After(time.Second):
		t.Fatal("timeout ignored")
	}
}
func TestRestartAndCorruptContent(t *testing.T) {
	c, _ := testConfig(t)
	n := startNode(t, c)
	srv := echoServer(t)
	register(t, n, srv.URL, nil)
	info, e := n.Upload(context.Background(), bytes.NewBufferString("persist me"))
	if e != nil {
		t.Fatal(e)
	}
	id := n.Info().PeerID
	if e = n.Close(); e != nil {
		t.Fatal(e)
	}
	restored := startNode(t, c)
	if restored.Info().PeerID != id || len(restored.Services()) != 1 {
		t.Fatal("identity/service missing after restart")
	}
	f, _, e := restored.OpenFile(info.Key)
	if e != nil {
		t.Fatal(e)
	}
	_ = f.Close()
	digest, _ := model.FileDigest(info.Key)
	if e = os.WriteFile(filepath.Join(c.DataDir, "blobs", digest), []byte("corrupt me"), 0600); e != nil {
		t.Fatal(e)
	}
	if _, _, e = restored.OpenFile(info.Key); e == nil {
		t.Fatal("corrupt content served")
	}
}
func TestHandlerRejectsRedirectAndOversize(t *testing.T) {
	c, _ := testConfig(t)
	c.Limits.MaxJSONBytes = 512
	n := startNode(t, c)
	var leaked atomic.Bool
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { leaked.Store(true) }))
	defer target.Close()
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, target.URL, 302) }))
	defer redirect.Close()
	register(t, n, redirect.URL, nil)
	if _, e := n.Call(context.Background(), call()); e == nil {
		t.Fatal("redirect accepted")
	}
	if leaked.Load() {
		t.Fatal("followed redirect")
	}
	big := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write(bytes.Repeat([]byte("x"), 1024)) }))
	defer big.Close()
	register(t, n, big.URL, nil)
	if _, e := n.Call(context.Background(), call()); e == nil {
		t.Fatal("oversize response accepted")
	}
}

func TestProviderStoreLimits(t *testing.T) {
	_, id := testConfig(t)
	p := newProviderStore(map[peer.ID]bool{id: true}, id)
	defer p.Close()
	for i := 0; i < maxProviderKeys+5; i++ {
		if e := p.AddProvider(context.Background(), []byte(fmt.Sprintf("key%d", i)), peer.AddrInfo{ID: id}); e != nil {
			t.Fatal(e)
		}
	}
	if len(p.entries) != maxProviderKeys {
		t.Fatalf("unbounded store: %d", len(p.entries))
	}
	_, rogue := testConfig(t)
	if e := p.AddProvider(context.Background(), []byte("evil"), peer.AddrInfo{ID: rogue}); e == nil {
		t.Fatal("unknown provider accepted")
	}
	if e := p.AddProvider(context.Background(), make([]byte, 65), peer.AddrInfo{ID: id}); e == nil {
		t.Fatal("oversize key accepted")
	}
	p.entries["expired"] = &providerEntry{peers: map[peer.ID]providerRecord{id: {info: peer.AddrInfo{ID: id}, expires: time.Now().Add(-time.Second)}}}
	if got, e := p.GetProviders(context.Background(), []byte("expired")); e != nil || len(got) != 0 {
		t.Fatal("expired record retained")
	}
}
func TestExactReaderRejectsWrongSize(t *testing.T) {
	for _, v := range []struct {
		body string
		size int64
		ok   bool
	}{{"abc", 3, true}, {"abc", 2, false}, {"abc", 4, false}, {"", 0, true}} {
		r := &exactReader{r: bytes.NewBufferString(v.body), remaining: v.size}
		_, e := io.ReadAll(r)
		if (e == nil) != v.ok {
			t.Fatalf("body %q size %d err=%v", v.body, v.size, e)
		}
	}
}
func TestFrameBoundsAndJSON(t *testing.T) {
	for _, body := range [][]byte{{0, 0, 0, 0}, {255, 255, 255, 255}, {0, 0, 0, 2, '{', '}'}, {0, 0, 0, 4, 'n', 'u', 'l', 'l'}} {
		var v probeRequest
		e := readFrame(bytes.NewReader(body), &v, 3)
		if len(body) != 6 && e == nil {
			t.Fatalf("accepted invalid frame: %v", body)
		}
	}
	var out bytes.Buffer
	if e := writeFrame(&out, probeRequest{Key: "long"}, 2); e == nil || out.Len() != 0 {
		t.Fatal("oversize frame started writing")
	}
}

func TestQUICRPC(t *testing.T) {
	a, aid := testConfig(t)
	b, bid := testConfig(t)
	a.Listen = []string{"/ip4/127.0.0.1/udp/0/quic-v1"}
	b.Listen = []string{"/ip4/127.0.0.1/udp/0/quic-v1"}
	a.AllowedPeers = []string{bid.String()}
	b.AllowedPeers = []string{aid.String()}
	an := startNode(t, a)
	b.Bootstrap = an.Info().Addrs
	bn := startNode(t, b)
	srv := echoServer(t)
	register(t, an, srv.URL, []string{bid.String()})
	r, e := bn.Call(context.Background(), call())
	if e != nil || r.Error != nil {
		t.Fatalf("QUIC call: %+v %v", r, e)
	}
	for _, c := range bn.host.Network().ConnsToPeer(aid) {
		if !strings.Contains(c.RemoteMultiaddr().String(), "quic-v1") {
			t.Fatal("not a QUIC connection")
		}
	}
}

func TestDHTDiscoveryThroughManagedRouter(t *testing.T) {
	a, aid := testConfig(t)
	b, bid := testConfig(t)
	r, rid := testConfig(t)
	a.AllowedPeers = []string{bid.String(), rid.String()}
	b.AllowedPeers = []string{aid.String(), rid.String()}
	r.AllowedPeers = []string{aid.String(), bid.String()}
	router := startNode(t, r)
	a.Bootstrap = router.Info().Addrs
	b.Bootstrap = router.Info().Addrs
	an := startNode(t, a)
	bn := startNode(t, b)
	srv := echoServer(t)
	register(t, an, srv.URL, []string{bid.String()})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for an.dht.RoutingTable().Size() == 0 || bn.dht.RoutingTable().Size() == 0 {
		select {
		case <-ctx.Done():
			t.Fatal("DHT routing never ready")
		case <-time.After(10 * time.Millisecond):
		}
	}
	if e := an.dht.Provide(ctx, an.keyCID("service:echo"), true); e != nil {
		t.Fatal(e)
	}
	resp, e := bn.Call(ctx, call())
	if e != nil || resp.Error != nil {
		t.Fatalf("DHT call failed: %+v %v", resp, e)
	}
}

func TestExplicitManagedRelay(t *testing.T) {
	a, aid := testConfig(t)
	b, bid := testConfig(t)
	r, rid := testConfig(t)
	a.Listen = []string{} // no direct listen address: RPC/file must use the reserved circuit
	r.RelayService = true
	r.AllowedPeers = []string{aid.String(), bid.String()}
	a.AllowedPeers = []string{bid.String(), rid.String()}
	b.AllowedPeers = []string{aid.String(), rid.String()}
	router := startNode(t, r)
	an := startNode(t, a)
	ai, _ := peer.AddrInfoFromString(router.Info().Addrs[0])
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, e := relayclient.Reserve(ctx, an.host, *ai); e != nil {
		t.Fatal(e)
	}
	relayAddr := ma.StringCast(router.Info().Addrs[0] + "/p2p-circuit/p2p/" + aid.String())
	b.Bootstrap = []string{relayAddr.String()}
	bn := startNode(t, b)
	srv := echoServer(t)
	register(t, an, srv.URL, []string{bid.String()})
	resp, e := bn.Call(ctx, call())
	if e != nil || resp.Error != nil {
		t.Fatalf("relay call: %+v %v", resp, e)
	}
	conns := bn.host.Network().ConnsToPeer(aid)
	if len(conns) == 0 || !strings.Contains(conns[0].RemoteMultiaddr().String(), "p2p-circuit") {
		t.Fatal("relay was bypassed")
	}
	content := bytes.Repeat([]byte("large relay file"), 16000)
	info, e := an.Upload(ctx, bytes.NewReader(content))
	if e != nil {
		t.Fatal(e)
	}
	if e = an.SetFileAccess(ctx, info.Key, []string{bid.String()}); e != nil {
		t.Fatal(e)
	}
	got, e := bn.Fetch(ctx, model.FetchRequest{Key: info.Key})
	if e != nil || got.Size != int64(len(content)) {
		t.Fatalf("relay file: %+v %v", got, e)
	}
}

func TestLostResponseDoesNotTrySecondProvider(t *testing.T) {
	a, aid := testConfig(t)
	b, bid := testConfig(t)
	c, cid := testConfig(t)
	a.AllowedPeers = []string{cid.String()}
	b.AllowedPeers = []string{cid.String()}
	c.AllowedPeers = []string{aid.String(), bid.String()}
	an, bn := startNode(t, a), startNode(t, b)
	c.Bootstrap = append(an.Info().Addrs, bn.Info().Addrs...)
	cn := startNode(t, c)
	srv := echoServer(t)
	register(t, an, srv.URL, []string{cid.String()})
	register(t, bn, srv.URL, []string{cid.String()})
	var first, second atomic.Int32
	chosen, other := an, bn
	if aid.String() > bid.String() {
		chosen, other = bn, an
	}
	chosen.host.SetStreamHandler(rpcProtocol, func(s network.Stream) {
		defer s.Reset()
		var req model.CallRequest
		if readFrame(s, &req, 1<<20) == nil {
			first.Add(1)
		}
	})
	other.host.SetStreamHandler(rpcProtocol, func(s network.Stream) { second.Add(1); _ = s.Reset() })
	_, e := cn.Call(context.Background(), call())
	var api *model.Error
	if !errors.As(e, &api) || !api.OutcomeUnknown {
		t.Fatalf("expected unknown: %v", e)
	}
	if first.Load() != 1 || second.Load() != 0 {
		t.Fatalf("retry detected first=%d second=%d", first.Load(), second.Load())
	}
}
func TestCorruptRemoteFileNeverCommits(t *testing.T) {
	a, b := pair(t)
	info, e := a.Upload(context.Background(), bytes.NewBufferString("genuine"))
	if e != nil {
		t.Fatal(e)
	}
	if e = a.SetFileAccess(context.Background(), info.Key, []string{b.Info().PeerID}); e != nil {
		t.Fatal(e)
	}
	a.host.SetStreamHandler(fileProtocol, func(s network.Stream) {
		defer s.Close()
		var req fileRequest
		if readFrame(s, &req, 1<<20) != nil {
			return
		}
		_ = writeFrame(s, fileHeader{Key: info.Key, Size: info.Size}, 1<<20)
		_, _ = s.Write([]byte("corrupt"))
		_ = s.CloseWrite()
	})
	_, e = b.Fetch(context.Background(), model.FetchRequest{Key: info.Key})
	var api *model.Error
	if !errors.As(e, &api) || api.Code != "integrity" {
		t.Fatalf("expected integrity error: %v", e)
	}
	if len(b.store.Files()) != 0 {
		t.Fatal("corrupt content committed")
	}
	entries, _ := os.ReadDir(filepath.Join(b.cfg.DataDir, "blobs"))
	if len(entries) != 0 {
		t.Fatal("failed transfer left staging content")
	}
}

func FuzzReadFrame(f *testing.F) {
	f.Add([]byte{0, 0, 0, 2, '{', '}'})
	f.Add([]byte{255, 255, 255, 255})
	f.Fuzz(func(t *testing.T, b []byte) {
		var request model.CallRequest
		_ = readFrame(bytes.NewReader(b), &request, 1024)
	})
}
