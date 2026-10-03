package config

import (
	"crypto/rand"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/libp2p/go-libp2p/core/crypto"
	"github.com/libp2p/go-libp2p/core/peer"
)

func validConfig() Config { c := Default(); c.DataDir = "./data"; c.Network = "private-test"; return c }
func newPeer(t *testing.T) string {
	t.Helper()
	key, _, err := crypto.GenerateEd25519Key(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	id, err := peer.IDFromPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return id.String()
}
func TestDefaultsLocalOnly(t *testing.T) {
	c := validConfig()
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	if len(c.AllowedPeers) != 0 || len(c.Bootstrap) != 0 || len(c.Relays) != 0 {
		t.Fatal("public connectivity enabled by default")
	}
	if c.Limits.MaxFileBytes != 64*MiB || c.Limits.MaxStoreBytes != 1024*MiB {
		t.Fatal("wrong defaults")
	}
}
func TestValidate(t *testing.T) {
	tests := []struct {
		name   string
		change func(*Config)
	}{
		{"missing data", func(c *Config) { c.DataDir = "" }},
		{"missing network", func(c *Config) { c.Network = "" }},
		{"unsafe network", func(c *Config) { c.Network = "../../public" }},
		{"public API", func(c *Config) { c.APIAddr = "0.0.0.0:1234" }},
		{"DNS API", func(c *Config) { c.APIAddr = "localhost:1234" }},
		{"bad port", func(c *Config) { c.APIAddr = "127.0.0.1:65536" }},
		{"bad listen", func(c *Config) { c.Listen = []string{"invalid"} }},
		{"bad peer", func(c *Config) { c.AllowedPeers = []string{"invalid"} }},
		{"file zero", func(c *Config) { c.Limits.MaxFileBytes = 0 }},
		{"file huge", func(c *Config) { c.Limits.MaxFileBytes = 1 << 31 }},
		{"store small", func(c *Config) { c.Limits.MaxStoreBytes = 1 }},
		{"json huge", func(c *Config) { c.Limits.MaxJSONBytes = 17 * MiB }},
		{"concurrency", func(c *Config) { c.Limits.MaxConcurrent = 129 }},
		{"timeout", func(c *Config) { c.Limits.TimeoutSeconds = 301 }},
		{"connections", func(c *Config) { c.Limits.MaxConnections = 0 }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := validConfig()
			tt.change(&c)
			if c.Validate() == nil {
				t.Fatal("invalid config accepted")
			}
		})
	}
}
func TestPeerAllowlist(t *testing.T) {
	a, b := newPeer(t), newPeer(t)
	c := validConfig()
	c.AllowedPeers = []string{a}
	c.Bootstrap = []string{"/ip4/127.0.0.1/tcp/1234/p2p/" + a}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	c.Relays = []string{"/ip4/127.0.0.1/tcp/1234/p2p/" + b}
	if c.Validate() == nil {
		t.Fatal("unlisted relay accepted")
	}
	c.Relays = nil
	c.Bootstrap = []string{"/ip4/127.0.0.1/tcp/1234/p2p/" + b + "/p2p-circuit/p2p/" + a}
	if c.Validate() == nil {
		t.Fatal("unlisted intermediary accepted")
	}
	c.Bootstrap = nil
	c.AllowedPeers = []string{a, a}
	if c.Validate() == nil {
		t.Fatal("duplicate peer accepted")
	}
}
func TestLoadStrictAndRelative(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	write := func(data string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
	}
	write(`{"data_dir":"./data","network":"private"}`)
	c, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if c.DataDir != filepath.Join(dir, "data") {
		t.Fatalf("relative path not resolved: %s", c.DataDir)
	}
	if c.Limits.MaxConcurrent != 16 {
		t.Fatal("defaults lost")
	}
	for _, raw := range []string{`{"data_dir":"data","network":"private","allow_peers":[]}`, `{"data_dir":"data","network":"private"} {}`, `{"data_dir":"data","network":"private","limits":{"unknown":1}}`, `null`, `[]`} {
		write(raw)
		if _, err := Load(path); err == nil {
			t.Fatalf("invalid JSON config accepted: %s", raw)
		}
	}
	c = validConfig()
	data, _ := json.Marshal(c)
	write(string(data))
	if _, err := Load(path); err != nil {
		t.Fatal(err)
	}
}
func TestToken(t *testing.T) {
	token := strings.Repeat("a", 32)
	t.Setenv("MEDIATRIX_API_TOKEN", token)
	if got, err := LoadToken(""); err != nil || got != token {
		t.Fatalf("env token: %q %v", got, err)
	}
	path := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(path, []byte(token+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if got, err := LoadToken(path); err != nil || got != token {
		t.Fatalf("file token: %q %v", got, err)
	}
	if err := os.Chmod(path, 0644); err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" {
		if _, err := LoadToken(path); err == nil {
			t.Fatal("world-readable token accepted")
		}
	}
	if err := os.Chmod(path, 0600); err != nil {
		t.Fatal(err)
	}
	link := path + "-link"
	if err := os.Symlink(path, link); err == nil {
		if _, err := LoadToken(link); err == nil {
			t.Fatal("symlink token accepted")
		}
	} else if runtime.GOOS != "windows" {
		t.Fatal(err)
	}
	for _, bad := range []string{"short", strings.Repeat("a", 31) + " ", strings.Repeat("a", 32) + "\n", strings.Repeat("a", 4097)} {
		t.Setenv("MEDIATRIX_API_TOKEN", bad)
		if _, err := LoadToken(""); err == nil {
			t.Fatal("invalid token accepted")
		}
	}
}

func TestRejectNonCanonicalPeerID(t *testing.T) {
	c := validConfig()
	id, err := peer.Decode(newPeer(t))
	if err != nil {
		t.Fatal(err)
	}
	c.AllowedPeers = []string{peer.ToCid(id).String()}
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "canonical") {
		t.Fatalf("noncanonical peer accepted: %v", err)
	}
}
