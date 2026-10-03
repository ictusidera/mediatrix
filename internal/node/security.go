package node

import (
	"crypto/rand"
	"fmt"
	"os"
	"path/filepath"

	"github.com/libp2p/go-libp2p/core/control"
	"github.com/libp2p/go-libp2p/core/crypto"
	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
	ma "github.com/multiformats/go-multiaddr"
)

type gate struct{ allowed map[peer.ID]bool }

func (g *gate) InterceptPeerDial(p peer.ID) bool                 { return g.allowed[p] }
func (g *gate) InterceptAddrDial(p peer.ID, _ ma.Multiaddr) bool { return g.allowed[p] }
func (g *gate) InterceptAccept(network.ConnMultiaddrs) bool      { return len(g.allowed) > 0 }
func (g *gate) InterceptSecured(_ network.Direction, p peer.ID, _ network.ConnMultiaddrs) bool {
	return g.allowed[p]
}
func (g *gate) InterceptUpgraded(c network.Conn) (bool, control.DisconnectReason) {
	return g.allowed[c.RemotePeer()], 0
}

// loadIdentity is called only after the store has exclusively locked the data directory.
func loadIdentity(dir string) (crypto.PrivKey, error) {
	path := filepath.Join(dir, "identity.key")
	info, err := os.Lstat(path)
	if err == nil {
		if !info.Mode().IsRegular() {
			return nil, fmt.Errorf("identity must be a regular file")
		}
		b, e := os.ReadFile(path)
		if e != nil {
			return nil, e
		}
		return crypto.UnmarshalPrivateKey(b)
	}
	if !os.IsNotExist(err) {
		return nil, err
	}
	key, _, err := crypto.GenerateEd25519Key(rand.Reader)
	if err != nil {
		return nil, err
	}
	b, err := crypto.MarshalPrivateKey(key)
	if err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return nil, err
	}
	_, err = f.Write(b)
	if err == nil {
		err = f.Sync()
	}
	ce := f.Close()
	if err == nil {
		err = ce
	}
	if err != nil {
		return nil, err
	}
	return key, nil
}
