// Package config loads the daemon's explicit, operator-managed network settings.
package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"unicode"

	"github.com/libp2p/go-libp2p/core/peer"
	ma "github.com/multiformats/go-multiaddr"
)

const (
	MiB            int64 = 1 << 20
	MaxPeers             = 128
	maxConfigBytes       = 1 << 20
)

// Config contains no public discovery defaults. An empty AllowedPeers list is
// local-only: the node must reject all inbound and outbound remote peers.
type Config struct {
	DataDir      string   `json:"data_dir"`
	APIAddr      string   `json:"api_addr"`
	Listen       []string `json:"listen"`
	Network      string   `json:"network"`
	AllowedPeers []string `json:"allowed_peers"`
	Bootstrap    []string `json:"bootstrap"`
	Relays       []string `json:"relays"`
	RelayService bool     `json:"relay_service"`
	DHTServer    bool     `json:"dht_server"`
	Limits       Limits   `json:"limits"`
}

type Limits struct {
	MaxFileBytes   int64 `json:"max_file_bytes"`
	MaxStoreBytes  int64 `json:"max_store_bytes"`
	MaxJSONBytes   int64 `json:"max_json_bytes"`
	MaxConcurrent  int   `json:"max_concurrent"`
	TimeoutSeconds int   `json:"timeout_seconds"`
	MaxConnections int   `json:"max_connections"`
}

func Default() Config {
	return Config{
		APIAddr:      "127.0.0.1:47832",
		Listen:       []string{"/ip4/0.0.0.0/tcp/0", "/ip4/0.0.0.0/udp/0/quic-v1"},
		AllowedPeers: []string{}, Bootstrap: []string{}, Relays: []string{},
		Limits: Limits{MaxFileBytes: 64 * MiB, MaxStoreBytes: 1024 * MiB, MaxJSONBytes: MiB, MaxConcurrent: 16, TimeoutSeconds: 30, MaxConnections: 64},
	}
}

// Load overlays JSON onto defaults. Relative data directories are resolved
// against the configuration file's directory, never the caller's working dir.
func Load(path string) (Config, error) {
	cfg := Default()
	f, err := os.Open(path)
	if err != nil {
		return Config{}, fmt.Errorf("open config: %w", err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return Config{}, err
	}
	if !info.Mode().IsRegular() {
		return Config{}, errors.New("config must be a regular file")
	}
	if info.Size() > maxConfigBytes {
		return Config{}, errors.New("config exceeds 1 MiB")
	}
	data, err := io.ReadAll(io.LimitReader(f, maxConfigBytes+1))
	if err != nil {
		return Config{}, fmt.Errorf("read config: %w", err)
	}
	if len(data) > maxConfigBytes {
		return Config{}, errors.New("config exceeds 1 MiB")
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&cfg); err != nil {
		return Config{}, fmt.Errorf("decode config: %w", err)
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		return Config{}, errors.New("config must contain exactly one JSON object")
	}
	if cfg.DataDir != "" {
		if !filepath.IsAbs(cfg.DataDir) {
			cfg.DataDir = filepath.Join(filepath.Dir(path), cfg.DataDir)
		}
		cfg.DataDir, err = filepath.Abs(cfg.DataDir)
		if err != nil {
			return Config{}, fmt.Errorf("resolve data_dir: %w", err)
		}
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

var networkPattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,63}$`)

func (c Config) Validate() error {
	if c.RelayService && len(c.Relays) > 0 {
		return errors.New("relay_service and relay client relays must use separate nodes")
	}
	if strings.TrimSpace(c.DataDir) == "" || strings.ContainsRune(c.DataDir, 0) {
		return errors.New("data_dir is required and must be a valid path")
	}
	if !networkPattern.MatchString(c.Network) {
		return errors.New("network is required: use 1-64 letters, numbers, dots, underscores or hyphens, starting with a letter or number")
	}
	if err := ValidateAPIAddr(c.APIAddr); err != nil {
		return err
	}
	if len(c.Listen) > 16 {
		return errors.New("at most 16 listen addresses are allowed")
	}
	for _, raw := range c.Listen {
		if len(raw) > 4096 {
			return errors.New("listen address is too long")
		}
		addr, err := ma.NewMultiaddr(raw)
		if err != nil {
			return fmt.Errorf("invalid listen address %q: %w", raw, err)
		}
		if _, err := addr.ValueForProtocol(ma.P_P2P); err == nil {
			return errors.New("listen address must not include a peer ID")
		}
		if _, err := addr.ValueForProtocol(ma.P_CIRCUIT); err == nil {
			return errors.New("listen address must not include a relay circuit")
		}
	}
	if len(c.AllowedPeers) > MaxPeers {
		return fmt.Errorf("at most %d allowed_peers are supported", MaxPeers)
	}
	allowed := make(map[peer.ID]bool, len(c.AllowedPeers))
	for _, raw := range c.AllowedPeers {
		id, err := peer.Decode(raw)
		if err != nil {
			return fmt.Errorf("invalid allowed peer ID %q: %w", raw, err)
		}
		if raw != id.String() {
			return fmt.Errorf("allowed peer ID must use canonical form %q", id.String())
		}
		if allowed[id] {
			return fmt.Errorf("duplicate allowed peer ID %q", raw)
		}
		allowed[id] = true
	}
	for kind, addrs := range map[string][]string{"bootstrap": c.Bootstrap, "relays": c.Relays} {
		if len(addrs) > MaxPeers {
			return fmt.Errorf("at most %d %s addresses are supported", MaxPeers, kind)
		}
		for _, raw := range addrs {
			if len(raw) > 4096 {
				return fmt.Errorf("%s address is too long", kind)
			}
			addr, err := ma.NewMultiaddr(raw)
			if err != nil {
				return fmt.Errorf("invalid %s multiaddr: %w", kind, err)
			}
			info, err := peer.AddrInfoFromP2pAddr(addr)
			if err != nil || len(info.Addrs) == 0 {
				return fmt.Errorf("%s address must include a transport address and /p2p/<peer-id>", kind)
			}
			if !allowed[info.ID] {
				return fmt.Errorf("%s peer %s is not in allowed_peers", kind, info.ID)
			}
			// A relayed destination also contacts the intermediary. Every peer ID
			// appearing in an operator address must be explicitly allowed.
			var denied bool
			ma.ForEach(addr, func(component ma.Component) bool {
				if component.Protocol().Code == ma.P_P2P {
					id, e := peer.Decode(component.Value())
					if e != nil || !allowed[id] {
						denied = true
						return false
					}
				}
				return true
			})
			if denied {
				return fmt.Errorf("%s address contains a peer outside allowed_peers", kind)
			}
		}
	}
	l := c.Limits
	if l.MaxFileBytes < 1 || l.MaxFileBytes > 1024*MiB {
		return errors.New("limits.max_file_bytes must be between 1 and 1073741824")
	}
	if l.MaxStoreBytes < l.MaxFileBytes || l.MaxStoreBytes > 1<<40 {
		return errors.New("limits.max_store_bytes must be at least max_file_bytes and at most 1 TiB")
	}
	if l.MaxJSONBytes < 1 || l.MaxJSONBytes > 16*MiB {
		return errors.New("limits.max_json_bytes must be between 1 and 16777216")
	}
	if l.MaxConcurrent < 1 || l.MaxConcurrent > 128 {
		return errors.New("limits.max_concurrent must be between 1 and 128")
	}
	if l.TimeoutSeconds < 1 || l.TimeoutSeconds > 300 {
		return errors.New("limits.timeout_seconds must be between 1 and 300")
	}
	if l.MaxConnections < 1 || l.MaxConnections > 4096 {
		return errors.New("limits.max_connections must be between 1 and 4096")
	}
	return nil
}

// ValidateAPIAddr requires a numeric loopback address. It intentionally avoids
// DNS resolution so rebinding and proxy settings cannot expose the bearer token.
func ValidateAPIAddr(addr string) error {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return errors.New("api_addr must be a numeric loopback IP and port")
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return errors.New("api_addr must use a numeric loopback IP")
	}
	p, err := strconv.Atoi(port)
	if err != nil || p < 0 || p > 65535 || port == "" || strings.IndexFunc(port, func(r rune) bool { return r < '0' || r > '9' }) >= 0 {
		return errors.New("api_addr port must be between 0 and 65535")
	}
	return nil
}

// LoadToken takes an explicit private token file, or MEDIATRIX_API_TOKEN when no
// file was selected. Tokens are deliberately never accepted as command args.
// Unix permission bits are enforced. Windows callers must protect the token file
// with an owner-only NTFS ACL; Go file modes do not expose the Windows DACL.
func LoadToken(path string) (string, error) {
	var token string
	if path == "" {
		token = os.Getenv("MEDIATRIX_API_TOKEN")
	} else {
		before, err := os.Lstat(path)
		if err != nil {
			return "", fmt.Errorf("token file: %w", err)
		}
		if !privateTokenFile(before) {
			return "", errors.New("token file must be a regular, private file (mode 0600 or 0400)")
		}
		f, err := os.Open(path)
		if err != nil {
			return "", fmt.Errorf("token file: %w", err)
		}
		defer f.Close()
		info, err := f.Stat()
		if err != nil {
			return "", err
		}
		if !os.SameFile(before, info) || !privateTokenFile(info) {
			return "", errors.New("token file changed or is not private")
		}
		data, err := io.ReadAll(io.LimitReader(f, 4097))
		if err != nil {
			return "", err
		}
		if len(data) > 4096 {
			return "", errors.New("token file is too large")
		}
		token = strings.TrimSpace(string(data))
	}
	if len(token) < 32 || len(token) > 4096 {
		return "", errors.New("API token must be 32-4096 characters; set MEDIATRIX_API_TOKEN or use --token-file")
	}
	for _, r := range token {
		if r > 126 || r < 33 || unicode.IsSpace(r) {
			return "", errors.New("API token must contain only non-whitespace printable ASCII characters")
		}
	}
	return token, nil
}

func privateTokenFile(info os.FileInfo) bool {
	return info.Mode().IsRegular() && (runtime.GOOS == "windows" || info.Mode().Perm()&0077 == 0)
}
