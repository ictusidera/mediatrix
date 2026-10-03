// Package node owns peer identity, transport and application protocol authority.
package node

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"sort"
	"sync"
	"time"

	"github.com/ictusidera/mediatrix/internal/config"
	"github.com/ictusidera/mediatrix/internal/model"
	"github.com/ictusidera/mediatrix/internal/store"
	libp2p "github.com/libp2p/go-libp2p"
	dht "github.com/libp2p/go-libp2p-kad-dht"
	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/libp2p/go-libp2p/core/peerstore"
	"github.com/libp2p/go-libp2p/core/protocol"
	"github.com/libp2p/go-libp2p/p2p/host/peerstore/pstoremem"
	rcmgr "github.com/libp2p/go-libp2p/p2p/host/resource-manager"
	relayv2 "github.com/libp2p/go-libp2p/p2p/protocol/circuitv2/relay"
	quic "github.com/libp2p/go-libp2p/p2p/transport/quic"
	"github.com/libp2p/go-libp2p/p2p/transport/tcp"
	ma "github.com/multiformats/go-multiaddr"
)

const (
	rpcProtocol   protocol.ID = "/mediatrix/rpc/2.0.0"
	fileProtocol  protocol.ID = "/mediatrix/file/2.0.0"
	probeProtocol protocol.ID = "/mediatrix/probe/2.0.0"
)

type Node struct {
	cfg           config.Config
	host          host.Host
	dht           *dht.IpfsDHT
	store         *store.Store
	allowed       map[peer.ID]bool
	allowedString map[string]bool
	known         []peer.AddrInfo
	handler       *http.Client
	transport     *http.Transport
	log           *slog.Logger
	ctx           context.Context
	cancel        context.CancelFunc
	slots         chan struct{}
	announce      chan struct{}
	wg            sync.WaitGroup
	closeOnce     sync.Once
	closeErr      error
	active        sync.WaitGroup
	lifecycle     sync.Mutex
	closing       bool
}

func New(ctx context.Context, cfg config.Config, logger *slog.Logger) (*Node, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if logger == nil {
		logger = slog.Default()
	}
	st, err := store.Open(cfg.DataDir, cfg.Limits.MaxFileBytes, cfg.Limits.MaxStoreBytes)
	if err != nil {
		return nil, err
	}
	success := false
	defer func() {
		if !success {
			_ = st.Close()
		}
	}()
	key, err := loadIdentity(cfg.DataDir)
	if err != nil {
		return nil, err
	}
	allowed := map[peer.ID]bool{}
	allowedS := map[string]bool{}
	for _, s := range cfg.AllowedPeers {
		p, _ := peer.Decode(s)
		allowed[p] = true
		allowedS[s] = true
	}
	for _, svc := range st.Services() {
		if err = validateService(svc); err != nil {
			return nil, fmt.Errorf("persisted service invalid: %w", err)
		}
	}
	lim := rcmgr.PartialLimitConfig{
		System:      rcmgr.ResourceLimits{Conns: rcmgr.LimitVal(cfg.Limits.MaxConnections), ConnsInbound: rcmgr.LimitVal(cfg.Limits.MaxConnections), ConnsOutbound: rcmgr.LimitVal(cfg.Limits.MaxConnections), Streams: rcmgr.LimitVal(cfg.Limits.MaxConcurrent*4 + 64), Memory: rcmgr.LimitVal64(64 << 20)},
		Transient:   rcmgr.ResourceLimits{Conns: 16, Streams: 32, Memory: 8 << 20},
		PeerDefault: rcmgr.ResourceLimits{Conns: 4, Streams: rcmgr.LimitVal(cfg.Limits.MaxConcurrent + 16)},
	}.Build(rcmgr.DefaultLimits.AutoScale())
	rm, err := rcmgr.NewResourceManager(rcmgr.NewFixedLimiter(lim))
	if err != nil {
		return nil, err
	}
	ps, err := pstoremem.NewPeerstore(pstoremem.WithMaxAddresses(2048), pstoremem.WithMaxSignedPeerRecords(256), pstoremem.WithMaxProtocols(32))
	if err != nil {
		_ = rm.Close()
		return nil, err
	}
	opts := []libp2p.Option{libp2p.Peerstore(ps), libp2p.Identity(key), libp2p.UserAgent("mediatrix/" + model.Version), libp2p.ConnectionGater(&gate{allowed: allowed}), libp2p.ResourceManager(rm), libp2p.NoTransports, libp2p.Transport(tcp.NewTCPTransport), libp2p.Transport(quic.NewTransport), libp2p.EnableRelay(), libp2p.DisableMetrics()}
	if len(cfg.Listen) == 0 {
		opts = append(opts, libp2p.NoListenAddrs)
	} else {
		opts = append(opts, libp2p.ListenAddrStrings(cfg.Listen...))
	}
	relays, err := parseAddrs(cfg.Relays)
	if err != nil {
		_ = ps.Close()
		_ = rm.Close()
		return nil, err
	}
	if cfg.RelayService {
		resources := relayv2.DefaultResources()
		resources.MaxReservations = min(len(allowed), 128)
		resources.MaxCircuits = cfg.Limits.MaxConcurrent
		resources.Limit = &relayv2.RelayLimit{Duration: time.Duration(cfg.Limits.TimeoutSeconds+10) * time.Second, Data: cfg.Limits.MaxFileBytes + 2*cfg.Limits.MaxJSONBytes + 1<<20}
		opts = append(opts, libp2p.ForceReachabilityPublic(), libp2p.EnableRelayService(relayv2.WithResources(resources)))
	}
	if len(relays) > 0 {
		opts = append(opts, libp2p.ForceReachabilityPrivate(), libp2p.EnableAutoRelayWithStaticRelays(relays), libp2p.EnableHolePunching())
	}
	h, err := libp2p.New(opts...)
	if err != nil {
		_ = ps.Close()
		_ = rm.Close()
		return nil, err
	}
	nctx, cancel := context.WithCancel(ctx)
	mode := dht.ModeClient
	if cfg.DHTServer {
		mode = dht.ModeServer
	}
	kd, err := dht.New(nctx, h, dht.Mode(mode), dht.ProtocolPrefix(protocol.ID("/mediatrix/"+cfg.Network)), dht.DisableValues(), dht.ProviderStore(newProviderStore(allowed, h.ID())), dht.QueryFilter(func(_ any, ai peer.AddrInfo) bool { return allowed[ai.ID] }), dht.RoutingTableFilter(func(_ any, p peer.ID) bool { return allowed[p] }))
	if err != nil {
		cancel()
		_ = h.Close()
		return nil, err
	}
	known, err := parseAddrs(append(append([]string{}, cfg.Bootstrap...), cfg.Relays...))
	if err != nil {
		cancel()
		_ = kd.Close()
		_ = h.Close()
		return nil, err
	}
	tr := &http.Transport{Proxy: nil, DialContext: (&net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}).DialContext, MaxIdleConns: cfg.Limits.MaxConcurrent, MaxConnsPerHost: cfg.Limits.MaxConcurrent, MaxIdleConnsPerHost: cfg.Limits.MaxConcurrent, IdleConnTimeout: 30 * time.Second, ResponseHeaderTimeout: time.Duration(cfg.Limits.TimeoutSeconds) * time.Second, MaxResponseHeaderBytes: 16 << 10, DisableCompression: true}
	n := &Node{cfg: cfg, host: h, dht: kd, store: st, allowed: allowed, allowedString: allowedS, known: known, log: logger, ctx: nctx, cancel: cancel, transport: tr, handler: &http.Client{Transport: tr, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, slots: make(chan struct{}, cfg.Limits.MaxConcurrent), announce: make(chan struct{}, 1)}
	for _, ai := range known {
		h.Peerstore().AddAddrs(ai.ID, ai.Addrs, peerstore.PermanentAddrTTL)
	}
	h.SetStreamHandler(rpcProtocol, n.handleRPC)
	h.SetStreamHandler(fileProtocol, n.handleFile)
	h.SetStreamHandler(probeProtocol, n.handleProbe)
	n.wg.Add(1)
	go n.discoveryLoop()
	success = true
	return n, nil
}
func parseAddrs(raw []string) ([]peer.AddrInfo, error) {
	out := []peer.AddrInfo{}
	for _, s := range raw {
		a, e := ma.NewMultiaddr(s)
		if e != nil {
			return nil, e
		}
		p, e := peer.AddrInfoFromP2pAddr(a)
		if e != nil {
			return nil, e
		}
		out = append(out, *p)
	}
	return out, nil
}
func (n *Node) Close() error {
	n.closeOnce.Do(func() {
		n.lifecycle.Lock()
		n.closing = true
		n.lifecycle.Unlock()
		n.cancel()
		n.closeErr = errors.Join(n.dht.Close(), n.host.Close())
		n.wg.Wait()
		n.active.Wait()
		n.transport.CloseIdleConnections()
		n.closeErr = errors.Join(n.closeErr, n.store.Close())
	})
	return n.closeErr
}
func (n *Node) Info() model.NodeInfo {
	addrs := []string{}
	for _, a := range n.host.Addrs() {
		addrs = append(addrs, a.String()+"/p2p/"+n.host.ID().String())
	}
	sort.Strings(addrs)
	return model.NodeInfo{Version: model.Version, PeerID: n.host.ID().String(), Addrs: addrs, ConnectedPeers: len(n.host.Network().Peers()), Services: len(n.store.Services()), Files: len(n.store.Files()), Network: n.cfg.Network}
}
func (n *Node) Services() []model.Service { return n.store.Services() }
func (n *Node) RegisterService(ctx context.Context, s model.Service) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := validateService(s); err != nil {
		return model.Err("invalid", err.Error())
	}
	if err := n.validateACL(s.AllowedPeers); err != nil {
		return err
	}
	if err := n.store.PutService(s); err != nil {
		return err
	}
	n.scheduleAnnounce()
	return nil
}
func (n *Node) DeleteService(ctx context.Context, name string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return n.store.DeleteService(name)
}
func (n *Node) scheduleAnnounce() {
	select {
	case n.announce <- struct{}{}:
	default:
	}
}
func (n *Node) enter() bool {
	select {
	case n.slots <- struct{}{}:
		return true
	default:
		return false
	}
}
func (n *Node) leave() { <-n.slots }
func (n *Node) startInbound(s network.Stream) bool {
	n.lifecycle.Lock()
	defer n.lifecycle.Unlock()
	if n.closing {
		_ = s.Reset()
		return false
	}
	if !n.allowed[s.Conn().RemotePeer()] || !n.enter() {
		_ = s.Reset()
		return false
	}
	n.active.Add(1)
	_ = s.SetDeadline(time.Now().Add(time.Duration(n.cfg.Limits.TimeoutSeconds) * time.Second))
	return true
}

func (n *Node) finishInbound() { n.leave(); n.active.Done() }
