package config

import (
	"errors"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Node          NodeConfig          `yaml:"node"`
	Network       NetworkConfig       `yaml:"network"`
	Services      []ServiceConfig     `yaml:"services"`
	Files         FileConfig          `yaml:"files"`
	Security      SecurityConfig      `yaml:"security"`
	Limits        LimitsConfig        `yaml:"limits"`
	Control       ControlConfig       `yaml:"control"`
	Observability ObservabilityConfig `yaml:"observability"`
}

type NodeConfig struct {
	IdentityPath  string   `yaml:"identity_path"`
	DatastorePath string   `yaml:"datastore_path"`
	Listen        []string `yaml:"listen"`
	Announce      []string `yaml:"announce"`
}

type NetworkConfig struct {
	BootstrapPeers []string    `yaml:"bootstrap_peers"`
	DHT            DHTConfig   `yaml:"dht"`
	Relay          RelayConfig `yaml:"relay"`
	NAT            NATConfig   `yaml:"nat"`
}

type DHTConfig struct {
	Mode      string `yaml:"mode"`
	Namespace string `yaml:"namespace"`
}

type RelayConfig struct {
	EnableClient  bool     `yaml:"enable_client"`
	EnableService bool     `yaml:"enable_service"`
	StaticRelays  []string `yaml:"static_relays"`
}

type NATConfig struct {
	AutoNAT   bool `yaml:"autonat"`
	HolePunch bool `yaml:"hole_punch"`
	PortMap   bool `yaml:"port_map"`
}

type ServiceConfig struct {
	Name    string   `yaml:"name"`
	Command []string `yaml:"command"`
}

type FileConfig struct {
	Shares    []FileShare `yaml:"shares"`
	ShareDirs []string    `yaml:"share_dirs"`
}

type FileShare struct {
	Path string `yaml:"path"`
	Name string `yaml:"name"`
}

type SecurityConfig struct {
	PrivateNetworkKeyPath string   `yaml:"private_network_key_path"`
	AllowPeers            []string `yaml:"allow_peers"`
	DenyPeers             []string `yaml:"deny_peers"`
}

type LimitsConfig struct {
	MaxConnections  int    `yaml:"max_connections"`
	MaxStreamsPeer  int    `yaml:"max_streams_per_peer"`
	MaxUploadRate   string `yaml:"max_upload_rate"`
	MaxDownloadRate string `yaml:"max_download_rate"`
}

type ControlConfig struct {
	Addr  string `yaml:"addr"`
	Token string `yaml:"token"`
}

type ObservabilityConfig struct {
	MetricsAddr string `yaml:"metrics_addr"`
	LogLevel    string `yaml:"log_level"`
}

func Default() Config {
	return Config{
		Node: NodeConfig{
			IdentityPath:  "./data/identity.key",
			DatastorePath: "./data/state.db",
			Listen: []string{
				"/ip4/0.0.0.0/tcp/4001",
				"/ip4/0.0.0.0/udp/4001/quic-v1",
			},
		},
		Network: NetworkConfig{
			DHT: DHTConfig{
				Mode:      "auto",
				Namespace: "mediatrix",
			},
			Relay: RelayConfig{
				EnableClient: true,
			},
			NAT: NATConfig{
				AutoNAT:   true,
				HolePunch: true,
				PortMap:   true,
			},
		},
		Files: FileConfig{},
		Limits: LimitsConfig{
			MaxConnections: 256,
			MaxStreamsPeer: 32,
		},
		Control: ControlConfig{
			Addr: "127.0.0.1:8080",
		},
		Observability: ObservabilityConfig{
			MetricsAddr: "127.0.0.1:9090",
			LogLevel:    "info",
		},
	}
}

func Load(path string) (Config, error) {
	if path == "" {
		return Config{}, errors.New("config path is required")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, err
	}
	cfg := Default()
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return Config{}, err
	}
	base := filepath.Dir(path)
	cfg.resolvePaths(base)
	return cfg, nil
}

func WriteDefault(path string) error {
	cfg := Default()
	data, err := yaml.Marshal(cfg)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil && filepath.Dir(path) != "." {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}

func (c *Config) resolvePaths(base string) {
	c.Node.IdentityPath = resolvePath(base, c.Node.IdentityPath)
	c.Node.DatastorePath = resolvePath(base, c.Node.DatastorePath)
	c.Security.PrivateNetworkKeyPath = resolvePath(base, c.Security.PrivateNetworkKeyPath)
	for i := range c.Files.Shares {
		c.Files.Shares[i].Path = resolvePath(base, c.Files.Shares[i].Path)
	}
	for i := range c.Files.ShareDirs {
		c.Files.ShareDirs[i] = resolvePath(base, c.Files.ShareDirs[i])
	}
}

func resolvePath(base, path string) string {
	if path == "" || filepath.IsAbs(path) {
		return path
	}
	return filepath.Clean(filepath.Join(base, path))
}
