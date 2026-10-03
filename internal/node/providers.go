package node

import (
	"context"
	"errors"
	"sort"
	"sync"
	"time"

	"github.com/libp2p/go-libp2p/core/peer"
	ma "github.com/multiformats/go-multiaddr"
)

// providerStore is deliberately volatile, bounded routing metadata. DHT records
// are hints, not the source of truth for durable registrations or authorization.
// A full store evicts the oldest key; callers can still probe configured peers.
type providerStore struct {
	mu      sync.Mutex
	entries map[string]*providerEntry
	allowed map[peer.ID]bool
	self    peer.ID
	closed  bool
}
type providerEntry struct {
	updated time.Time
	peers   map[peer.ID]providerRecord
}
type providerRecord struct {
	info    peer.AddrInfo
	expires time.Time
}

const (
	maxProviderKeys    = 1024
	maxProvidersPerKey = 16
	maxProviderAddrs   = 8
	providerTTL        = 5 * time.Minute
)

func newProviderStore(allowed map[peer.ID]bool, self peer.ID) *providerStore {
	return &providerStore{entries: map[string]*providerEntry{}, allowed: allowed, self: self}
}
func (p *providerStore) AddProvider(ctx context.Context, key []byte, info peer.AddrInfo) error {
	if e := ctx.Err(); e != nil {
		return e
	}
	if len(key) == 0 || len(key) > 64 {
		return errors.New("invalid provider key")
	}
	if info.ID != p.self && !p.allowed[info.ID] {
		return errors.New("provider peer not allowed")
	}
	addrs := make([]ma.Multiaddr, 0, maxProviderAddrs)
	for _, a := range info.Addrs {
		if len(a.Bytes()) <= 1024 {
			addrs = append(addrs, a)
		}
		if len(addrs) == maxProviderAddrs {
			break
		}
	}
	info.Addrs = addrs
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return errors.New("provider store closed")
	}
	now := time.Now()
	s := string(key)
	entry := p.entries[s]
	if entry == nil {
		if len(p.entries) >= maxProviderKeys {
			var oldest string
			var when time.Time
			for k, v := range p.entries {
				if oldest == "" || v.updated.Before(when) {
					oldest, when = k, v.updated
				}
			}
			delete(p.entries, oldest)
		}
		entry = &providerEntry{peers: map[peer.ID]providerRecord{}}
		p.entries[s] = entry
	}
	for id, v := range entry.peers {
		if now.After(v.expires) {
			delete(entry.peers, id)
		}
	}
	if _, ok := entry.peers[info.ID]; !ok && len(entry.peers) >= maxProvidersPerKey {
		return errors.New("provider peer limit")
	}
	entry.updated = now
	entry.peers[info.ID] = providerRecord{info: info, expires: now.Add(providerTTL)}
	return nil
}
func (p *providerStore) GetProviders(ctx context.Context, key []byte) ([]peer.AddrInfo, error) {
	if e := ctx.Err(); e != nil {
		return nil, e
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return nil, errors.New("provider store closed")
	}
	out := []peer.AddrInfo{}
	entry := p.entries[string(key)]
	if entry == nil {
		return out, nil
	}
	for id, r := range entry.peers {
		if time.Now().After(r.expires) {
			delete(entry.peers, id)
			continue
		}
		r.info.Addrs = append([]ma.Multiaddr{}, r.info.Addrs...)
		out = append(out, r.info)
	}
	if len(entry.peers) == 0 {
		delete(p.entries, string(key))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID.String() < out[j].ID.String() })
	return out, nil
}
func (p *providerStore) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.closed = true
	p.entries = nil
	return nil
}
