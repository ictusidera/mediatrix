package p2p

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	libp2p "github.com/libp2p/go-libp2p"
	kaddht "github.com/libp2p/go-libp2p-kad-dht"
	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/libp2p/go-libp2p/core/protocol"
	"github.com/libp2p/go-libp2p/p2p/host/peerstore/pstoremem"
	ma "github.com/multiformats/go-multiaddr"

	"github.com/s-yamamoto/mediatrix/internal/config"
	"github.com/s-yamamoto/mediatrix/internal/identity"
	"github.com/s-yamamoto/mediatrix/internal/store"
	"github.com/s-yamamoto/mediatrix/internal/wire"
)

type Node struct {
	cfg       config.Config
	host      host.Host
	dht       *kaddht.IpfsDHT
	store     *store.Store
	services  map[string]config.ServiceConfig
	files     map[string]store.FileRecord
	namespace string
	mu        sync.RWMutex
}

type Options struct {
	Role string
}

func New(ctx context.Context, cfg config.Config, opts Options) (*Node, error) {
	if err := os.MkdirAll(filepath.Dir(cfg.Node.DatastorePath), 0o755); err != nil {
		return nil, err
	}
	st, err := store.Open(cfg.Node.DatastorePath)
	if err != nil {
		return nil, err
	}

	key, err := identity.LoadOrCreate(cfg.Node.IdentityPath)
	if err != nil {
		_ = st.Close()
		return nil, err
	}

	ps, err := pstoremem.NewPeerstore()
	if err != nil {
		_ = st.Close()
		return nil, err
	}

	libp2pOpts := []libp2p.Option{
		libp2p.Identity(key),
		libp2p.Peerstore(ps),
		libp2p.ListenAddrStrings(cfg.Node.Listen...),
		libp2p.EnableRelay(),
		libp2p.UserAgent("mediatrix/0.1"),
	}
	if cfg.Network.NAT.PortMap {
		libp2pOpts = append(libp2pOpts, libp2p.NATPortMap())
	}
	if cfg.Network.NAT.AutoNAT {
		libp2pOpts = append(libp2pOpts, libp2p.EnableAutoNATv2())
	}
	if cfg.Network.NAT.HolePunch {
		libp2pOpts = append(libp2pOpts, libp2p.EnableHolePunching())
	}
	if cfg.Network.Relay.EnableService || opts.Role == "relay" {
		libp2pOpts = append(libp2pOpts, libp2p.EnableRelayService())
	}
	if len(cfg.Network.Relay.StaticRelays) > 0 {
		relays, err := parseAddrInfos(cfg.Network.Relay.StaticRelays)
		if err != nil {
			_ = st.Close()
			return nil, err
		}
		libp2pOpts = append(libp2pOpts, libp2p.EnableAutoRelayWithStaticRelays(relays))
	}

	h, err := libp2p.New(libp2pOpts...)
	if err != nil {
		_ = st.Close()
		return nil, err
	}

	bootstraps, err := parseAddrInfos(cfg.Network.BootstrapPeers)
	if err != nil {
		_ = h.Close()
		_ = st.Close()
		return nil, err
	}

	dhtOpts := []kaddht.Option{
		kaddht.Mode(dhtMode(cfg.Network.DHT.Mode)),
	}
	if cfg.Network.DHT.Namespace != "" {
		dhtOpts = append(dhtOpts, kaddht.ProtocolPrefix(protocol.ID("/"+cfg.Network.DHT.Namespace)))
	}
	if len(bootstraps) > 0 {
		dhtOpts = append(dhtOpts, kaddht.BootstrapPeers(bootstraps...))
	}
	kdht, err := kaddht.New(ctx, h, dhtOpts...)
	if err != nil {
		_ = h.Close()
		_ = st.Close()
		return nil, err
	}

	n := &Node{
		cfg:       cfg,
		host:      h,
		dht:       kdht,
		store:     st,
		services:  map[string]config.ServiceConfig{},
		files:     map[string]store.FileRecord{},
		namespace: cfg.Network.DHT.Namespace,
	}
	if n.namespace == "" {
		n.namespace = "mediatrix"
	}
	n.host.SetStreamHandler(protocol.ID(wire.RPCProtocol), n.handleRPC)
	n.host.SetStreamHandler(protocol.ID(wire.FileProtocol), n.handleFile)

	for _, ai := range bootstraps {
		if ai.ID == h.ID() {
			continue
		}
		connectCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
		err := h.Connect(connectCtx, ai)
		cancel()
		if err != nil {
			log.Printf("bootstrap connect failed: peer=%s err=%v", ai.ID, err)
			continue
		}
		n.observePeer(ai)
	}
	if err := kdht.Bootstrap(ctx); err != nil {
		log.Printf("dht bootstrap failed: %v", err)
	}
	return n, nil
}

func (n *Node) Close() error {
	var errs []error
	if n.dht != nil {
		errs = append(errs, n.dht.Close())
	}
	if n.host != nil {
		errs = append(errs, n.host.Close())
	}
	if n.store != nil {
		errs = append(errs, n.store.Close())
	}
	return errors.Join(errs...)
}

func (n *Node) PeerID() peer.ID {
	return n.host.ID()
}

func (n *Node) ListenAddrs() []string {
	out := make([]string, 0, len(n.host.Addrs()))
	for _, addr := range n.host.Addrs() {
		out = append(out, addr.Encapsulate(ma.StringCast("/p2p/"+n.host.ID().String())).String())
	}
	return out
}

func (n *Node) Bootstrap(ctx context.Context) error {
	return n.dht.Bootstrap(ctx)
}

func (n *Node) RegisterConfigured(ctx context.Context) error {
	for _, svc := range n.cfg.Services {
		if len(svc.Command) == 0 {
			return fmt.Errorf("service %s has no command", svc.Name)
		}
		if err := n.RegisterService(ctx, svc); err != nil {
			return err
		}
	}
	for _, share := range n.cfg.Files.Shares {
		if _, err := n.RegisterFile(ctx, share.Path, share.Name); err != nil {
			return err
		}
	}
	for _, dir := range n.cfg.Files.ShareDirs {
		if err := n.RegisterDir(ctx, dir); err != nil {
			return err
		}
	}
	return nil
}

func (n *Node) RegisterService(ctx context.Context, svc config.ServiceConfig) error {
	name := serviceKey(svc.Name)
	n.mu.Lock()
	svc.Name = name
	n.services[name] = svc
	n.mu.Unlock()
	if err := n.store.RecordService(name, strings.Join(svc.Command, "\x00")); err != nil {
		return err
	}
	return n.provide(ctx, name)
}

func (n *Node) RegisterFile(ctx context.Context, path, name string) (string, error) {
	rec, err := fileRecord(path, name)
	if err != nil {
		return "", err
	}
	n.mu.Lock()
	n.files[rec.Key] = rec
	n.mu.Unlock()
	if err := n.store.UpsertFile(rec); err != nil {
		return "", err
	}
	log.Printf("file advertised: key=%s path=%s size=%d", rec.Key, rec.Path, rec.Size)
	return rec.Key, n.provide(ctx, rec.Key)
}

func (n *Node) RegisterDir(ctx context.Context, dir string) error {
	return filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		_, err = n.RegisterFile(ctx, path, "")
		return err
	})
}

func (n *Node) provide(ctx context.Context, key string) error {
	c, err := cidForKey(n.namespace, key)
	if err != nil {
		return err
	}
	return n.dht.Provide(ctx, c, true)
}

func (n *Node) AdvertiseLoop(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			n.mu.RLock()
			keys := make([]string, 0, len(n.services)+len(n.files))
			for k := range n.services {
				keys = append(keys, k)
			}
			for k := range n.files {
				keys = append(keys, k)
			}
			n.mu.RUnlock()
			for _, key := range keys {
				if err := n.provide(ctx, key); err != nil {
					log.Printf("advertise failed: key=%s err=%v", key, err)
				}
			}
		}
	}
}

func (n *Node) Call(ctx context.Context, service, method, params string) (wire.RPCResponse, error) {
	key := serviceKey(service)
	providers, err := n.findProviders(ctx, key, 8)
	if err != nil {
		return wire.RPCResponse{}, err
	}
	var lastErr error
	for _, provider := range providers {
		if provider.ID == n.host.ID() {
			continue
		}
		if err := n.host.Connect(ctx, provider); err != nil {
			lastErr = err
			continue
		}
		n.observePeer(provider)
		stream, err := n.host.NewStream(ctx, provider.ID, protocol.ID(wire.RPCProtocol))
		if err != nil {
			lastErr = err
			continue
		}
		req := wire.RPCRequest{Service: key, Method: method, Params: params}
		if err := writeJSONFrame(stream, req); err != nil {
			_ = stream.Close()
			lastErr = err
			continue
		}
		var resp wire.RPCResponse
		if err := readJSONFrame(stream, &resp); err != nil {
			_ = stream.Close()
			lastErr = err
			continue
		}
		_ = stream.Close()
		return resp, nil
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("no providers found for %s", key)
	}
	return wire.RPCResponse{}, lastErr
}

func (n *Node) FetchFile(ctx context.Context, key, dst string) error {
	if _, err := parseFileKey(key); err != nil {
		return err
	}
	providers, err := n.findProviders(ctx, key, 8)
	if err != nil {
		return err
	}
	var lastErr error
	for _, provider := range providers {
		if provider.ID == n.host.ID() {
			continue
		}
		start := time.Now()
		err := n.fetchFromProvider(ctx, provider, key, dst)
		ended := time.Now()
		status := "ok"
		if err != nil {
			status = "failed"
			lastErr = err
		}
		_ = n.store.RecordTransfer("file", key, provider.ID.String(), status, bytesWritten(dst), start, ended)
		if err == nil {
			return nil
		}
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("no providers found for %s", key)
	}
	return lastErr
}

func (n *Node) fetchFromProvider(ctx context.Context, provider peer.AddrInfo, key, dst string) error {
	if err := n.host.Connect(ctx, provider); err != nil {
		return err
	}
	n.observePeer(provider)
	stream, err := n.host.NewStream(ctx, provider.ID, protocol.ID(wire.FileProtocol))
	if err != nil {
		return err
	}
	defer stream.Close()
	if err := writeJSONFrame(stream, wire.FileRequest{Key: key}); err != nil {
		return err
	}
	var header wire.FileHeader
	if err := readJSONFrame(stream, &header); err != nil {
		return err
	}
	tmp := dst + ".part"
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil && filepath.Dir(dst) != "." {
		return err
	}
	out, err := os.Create(tmp)
	if err != nil {
		return err
	}
	hash := sha256.New()
	w := io.MultiWriter(out, hash)
	_, copyErr := io.CopyN(w, stream, header.Size)
	closeErr := out.Close()
	if copyErr != nil {
		return copyErr
	}
	if closeErr != nil {
		return closeErr
	}
	digest := hex.EncodeToString(hash.Sum(nil))
	if digest != header.SHA256 {
		return fmt.Errorf("download hash mismatch: got=%s want=%s", digest, header.SHA256)
	}
	wantDigest, err := parseFileKey(key)
	if err != nil {
		return err
	}
	if digest != wantDigest {
		return fmt.Errorf("download key mismatch: got=%s want=%s", digest, wantDigest)
	}
	return os.Rename(tmp, dst)
}

func (n *Node) findProviders(ctx context.Context, key string, count int) ([]peer.AddrInfo, error) {
	c, err := cidForKey(n.namespace, key)
	if err != nil {
		return nil, err
	}
	ch := n.dht.FindProvidersAsync(ctx, c, count)
	var providers []peer.AddrInfo
	for ai := range ch {
		if ai.ID == "" {
			continue
		}
		providers = append(providers, ai)
		n.observePeer(ai)
	}
	return providers, nil
}

func (n *Node) handleRPC(stream network.Stream) {
	defer stream.Close()
	var req wire.RPCRequest
	if err := readJSONFrame(stream, &req); err != nil {
		log.Printf("rpc read failed: %v", err)
		return
	}
	n.mu.RLock()
	svc, ok := n.services[req.Service]
	n.mu.RUnlock()
	if !ok {
		_ = writeJSONFrame(stream, wire.RPCResponse{OK: false, Error: "service not found"})
		return
	}
	result, err := runServiceCommand(svc.Command, req)
	if err != nil {
		_ = writeJSONFrame(stream, wire.RPCResponse{OK: false, Error: err.Error()})
		return
	}
	_ = writeJSONFrame(stream, wire.RPCResponse{OK: true, Result: result})
}

func (n *Node) handleFile(stream network.Stream) {
	defer stream.Close()
	var req wire.FileRequest
	if err := readJSONFrame(stream, &req); err != nil {
		log.Printf("file request read failed: %v", err)
		return
	}
	n.mu.RLock()
	rec, ok := n.files[req.Key]
	n.mu.RUnlock()
	if !ok {
		log.Printf("file key not found: %s", req.Key)
		return
	}
	f, err := os.Open(rec.Path)
	if err != nil {
		log.Printf("file open failed: %v", err)
		return
	}
	defer f.Close()
	header := wire.FileHeader{Name: rec.Name, Size: rec.Size, SHA256: rec.SHA256}
	if err := writeJSONFrame(stream, header); err != nil {
		log.Printf("file header write failed: %v", err)
		return
	}
	if _, err := io.Copy(stream, f); err != nil {
		log.Printf("file body write failed: %v", err)
	}
}

func runServiceCommand(command []string, req wire.RPCRequest) (string, error) {
	if len(command) == 1 && command[0] == "builtin:echo" {
		return req.Params, nil
	}
	cmd := exec.Command(command[0], command[1:]...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return "", err
	}
	var out strings.Builder
	var stderr strings.Builder
	cmd.Stdout = &out
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return "", err
	}
	if err := writeJSONFrame(stdin, req); err != nil {
		_ = stdin.Close()
		return "", err
	}
	if err := stdin.Close(); err != nil {
		return "", err
	}
	if err := cmd.Wait(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return "", errors.New(msg)
	}
	return strings.TrimSpace(out.String()), nil
}

func fileRecord(path, name string) (store.FileRecord, error) {
	clean, err := filepath.Abs(path)
	if err != nil {
		return store.FileRecord{}, err
	}
	info, err := os.Stat(clean)
	if err != nil {
		return store.FileRecord{}, err
	}
	if info.IsDir() {
		return store.FileRecord{}, fmt.Errorf("file share path is a directory: %s", clean)
	}
	f, err := os.Open(clean)
	if err != nil {
		return store.FileRecord{}, err
	}
	defer f.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, f); err != nil {
		return store.FileRecord{}, err
	}
	digest := hex.EncodeToString(hash.Sum(nil))
	if name == "" {
		name = filepath.Base(clean)
	}
	return store.FileRecord{
		Key:    fileKeyFromDigest(digest),
		Path:   clean,
		Name:   name,
		Size:   info.Size(),
		SHA256: digest,
	}, nil
}

func parseAddrInfos(addrs []string) ([]peer.AddrInfo, error) {
	infos := make([]peer.AddrInfo, 0, len(addrs))
	for _, raw := range addrs {
		if strings.TrimSpace(raw) == "" {
			continue
		}
		addr, err := ma.NewMultiaddr(raw)
		if err != nil {
			return nil, err
		}
		info, err := peer.AddrInfoFromP2pAddr(addr)
		if err != nil {
			return nil, err
		}
		infos = append(infos, *info)
	}
	return infos, nil
}

func dhtMode(mode string) kaddht.ModeOpt {
	switch strings.ToLower(mode) {
	case "client":
		return kaddht.ModeClient
	case "server":
		return kaddht.ModeServer
	default:
		return kaddht.ModeAuto
	}
}

func (n *Node) observePeer(ai peer.AddrInfo) {
	if ai.ID == "" {
		return
	}
	addrs := make([]string, 0, len(ai.Addrs))
	for _, addr := range ai.Addrs {
		addrs = append(addrs, addr.String())
	}
	_ = n.store.UpsertPeer(ai.ID.String(), strings.Join(addrs, "\n"))
}

func bytesWritten(path string) int64 {
	info, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return info.Size()
}

func LocalIPHint() string {
	ifaces, err := net.Interfaces()
	if err != nil {
		return ""
	}
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, addr := range addrs {
			ipnet, ok := addr.(*net.IPNet)
			if !ok || ipnet.IP.To4() == nil {
				continue
			}
			return ipnet.IP.String()
		}
	}
	return ""
}
