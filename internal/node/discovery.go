package node

import (
	"context"
	"crypto/sha256"
	"sort"
	"time"

	"github.com/ictusidera/mediatrix/internal/model"
	"github.com/ipfs/go-cid"
	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/multiformats/go-multihash"
)

type probeRequest struct {
	Key string `json:"key"`
}
type probeResponse struct {
	Available bool `json:"available"`
}

func (n *Node) keyCID(key string) cid.Cid {
	h := sha256.Sum256([]byte("mediatrix:v2:" + n.cfg.Network + ":" + key))
	mh, _ := multihash.Encode(h[:], multihash.SHA2_256)
	return cid.NewCidV1(cid.Raw, mh)
}
func (n *Node) available(key, caller string) bool {
	if svc, ok := n.store.GetService(key); ok {
		return caller == n.host.ID().String() || permits(svc.AllowedPeers, caller)
	}
	if f, ok := n.store.GetFile(key); ok {
		return caller == n.host.ID().String() || permits(f.AllowedPeers, caller)
	}
	return false
}
func (n *Node) handleProbe(s network.Stream) {
	if !n.startInbound(s) {
		return
	}
	defer n.finishInbound()
	defer s.Close()
	var r probeRequest
	if readFrame(s, &r, n.cfg.Limits.MaxJSONBytes) != nil {
		return
	}
	if model.ValidateService(r.Key) != nil {
		if _, e := model.FileDigest(r.Key); e != nil {
			return
		}
	}
	_ = writeFrame(s, probeResponse{Available: n.available(r.Key, s.Conn().RemotePeer().String())}, n.cfg.Limits.MaxJSONBytes)
}
func (n *Node) discoveryLoop() {
	defer n.wg.Done()
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	n.scheduleAnnounce()
	for {
		select {
		case <-n.ctx.Done():
			return
		case <-t.C:
		case <-n.announce:
		}
		ctx, cancel := context.WithTimeout(n.ctx, 10*time.Second)
		for _, ai := range n.known {
			if ctx.Err() != nil {
				break
			}
			if n.host.Network().Connectedness(ai.ID) != network.Connected {
				if err := n.host.Connect(ctx, ai); err != nil {
					n.log.Debug("bootstrap unavailable", "peer", ai.ID.String())
				}
			}
		}
		_ = n.dht.Bootstrap(ctx)
		// Provider records reveal key/provider metadata to trusted DHT routers.
		for _, svc := range n.store.Services() {
			if len(svc.AllowedPeers) > 0 && ctx.Err() == nil {
				_ = n.dht.Provide(ctx, n.keyCID(svc.Name), true)
			}
		}
		for _, f := range n.store.Files() {
			if len(f.AllowedPeers) > 0 && ctx.Err() == nil {
				_ = n.dht.Provide(ctx, n.keyCID(f.Key), true)
			}
		}
		cancel()
	}
}

// providers checks trusted direct peers first and uses private DHT discovery for
// peers not already known. A probe never grants authority to perform an RPC.
func (n *Node) providers(ctx context.Context, key string) []peer.AddrInfo {
	found := []peer.AddrInfo{}
	seen := map[peer.ID]bool{}
	try := func(ai peer.AddrInfo) {
		if seen[ai.ID] || !n.allowed[ai.ID] || ai.ID == n.host.ID() {
			return
		}
		seen[ai.ID] = true
		pc, cancel := context.WithTimeout(ctx, 2*time.Second)
		defer cancel()
		if n.host.Connect(pc, ai) != nil {
			return
		}
		s, e := n.host.NewStream(network.WithAllowLimitedConn(pc, "mediatrix"), ai.ID, probeProtocol)
		if e != nil {
			return
		}
		defer s.Close()
		stop := bindStream(pc, s)
		defer stop()
		if writeFrame(s, probeRequest{Key: key}, n.cfg.Limits.MaxJSONBytes) != nil {
			return
		}
		var r probeResponse
		if readFrame(s, &r, n.cfg.Limits.MaxJSONBytes) == nil && r.Available {
			found = append(found, ai)
		}
	}
	peers := append([]peer.AddrInfo{}, n.known...)
	for _, p := range n.host.Network().Peers() {
		peers = append(peers, n.host.Peerstore().PeerInfo(p))
	}
	sort.Slice(peers, func(i, j int) bool { return peers[i].ID.String() < peers[j].ID.String() })
	for _, ai := range peers {
		if ctx.Err() != nil {
			break
		}
		try(ai)
		if len(found) > 0 {
			return found
		}
	}
	dc, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	for ai := range n.dht.FindProvidersAsync(dc, n.keyCID(key), 16) {
		try(ai)
		if len(found) > 0 {
			break
		}
	}
	return found
}
