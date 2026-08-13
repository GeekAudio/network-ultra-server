package config

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"strconv"
	"strings"

	"github.com/BurntSushi/toml"

	"github.com/GeekASMR/network-ultra-server/internal/proto"
)

type Config struct {
	Server    ServerCfg    `toml:"server"`
	TLS       TLSCfg       `toml:"tls"`
	Log       LogCfg       `toml:"log"`
	RateLimit RateLimitCfg `toml:"ratelimit"`
}

type ServerCfg struct {
	Listen           string `toml:"listen"`
	HealthListen     string `toml:"health_listen"`
	UdpListen        string `toml:"udp_listen"`         // empty = UDP disabled
	UdpAdvertiseHost string `toml:"udp_advertise_host"` // empty = derive from Listen
	MaxRooms         int    `toml:"max_rooms"`
	MaxPeersPerRoom  int    `toml:"max_peers_per_room"`
	MaxConnections   int    `toml:"max_connections"`
	AdminToken       string `toml:"admin_token"`
	// TrustedProxies is the explicit set of reverse-proxy source addresses or
	// CIDRs allowed to supply Forwarded/X-Forwarded-For client identity. Empty
	// means all forwarding headers are ignored.
	TrustedProxies []string `toml:"trusted_proxies"`

	// These switches are deliberately separate: TLS protects WebSocket only;
	// the UDP audio plane remains plaintext and must be opted into explicitly.
	AllowInsecurePublic bool `toml:"allow_insecure_public"`
	AllowInsecureUDP    bool `toml:"allow_insecure_udp"`

	// Server-level connection password (v1.3+).
	// Empty = no server-level gating (anyone who knows the address can connect).
	// Non-empty = clients must include serverPassword in their hello message;
	// otherwise server replies with SERVER_PASSWORD_REQUIRED / BAD_SERVER_PASSWORD.
	// Stored as plaintext at rest (config.toml is root-only on systemd hosts);
	// hashed in memory on load and only the bcrypt hash is compared per-connection.
	Password string `toml:"password"`
}

type TLSCfg struct {
	Enabled         bool   `toml:"enabled"`
	CertFile        string `toml:"cert_file"`
	KeyFile         string `toml:"key_file"`
	AutoLetsEncrypt bool   `toml:"auto_letsencrypt"`
	Domain          string `toml:"domain"`
	Email           string `toml:"email"`
}

type LogCfg struct {
	Level  string `toml:"level"`
	Format string `toml:"format"`
	Path   string `toml:"path"`
}

type RateLimitCfg struct {
	HelloPerIPPerMinute        int `toml:"hello_per_ip_per_minute"`
	RoomCreatePerPeerPerMinute int `toml:"room_create_per_peer_per_minute"`
	RoomJoinPerPeerPerMinute   int `toml:"room_join_per_peer_per_minute"`
	RoomListPerPeerPerMinute   int `toml:"room_list_per_peer_per_minute"`
	ControlPerPeerPerMinute    int `toml:"control_per_peer_per_minute"`
	AudioFramesPerPeerPerSec   int `toml:"audio_frames_per_peer_per_second"`
	PasswordChecksConcurrent   int `toml:"password_checks_concurrent"`
}

func Default() Config {
	return Config{
		Server: ServerCfg{
			Listen:           "127.0.0.1:18900",
			HealthListen:     "127.0.0.1:18901",
			UdpListen:        "",
			UdpAdvertiseHost: "", // empty = use the host the client connected via
			MaxRooms:         50,
			MaxPeersPerRoom:  8,
			MaxConnections:   200,
			AdminToken:       "",
		},
		TLS: TLSCfg{
			Enabled: false,
		},
		Log: LogCfg{
			Level:  "info",
			Format: "json",
			Path:   "",
		},
		RateLimit: RateLimitCfg{
			HelloPerIPPerMinute:        10,
			RoomCreatePerPeerPerMinute: 5,
			RoomJoinPerPeerPerMinute:   30,
			RoomListPerPeerPerMinute:   60,
			ControlPerPeerPerMinute:    120,
			AudioFramesPerPeerPerSec:   200,
			PasswordChecksConcurrent:   4,
		},
	}
}

// Load reads a TOML file into a Config, applying defaults for missing fields.
// Returns Default() if path is empty or missing.
func Load(path string) (Config, error) {
	cfg := Default()

	if path == "" {
		return cfg, nil
	}

	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		return cfg, nil
	}

	if _, err := toml.DecodeFile(path, &cfg); err != nil {
		return cfg, fmt.Errorf("load config %s: %w", path, err)
	}

	if err := validate(&cfg); err != nil {
		return cfg, err
	}
	return cfg, nil
}

func validate(c *Config) error {
	if c.Server.Listen == "" {
		return errors.New("server.listen is empty")
	}
	if _, err := HealthURL(c.Server.HealthListen); err != nil {
		return errors.New("server.health_listen must be an explicit loopback address; publish metrics through an authenticated proxy or tunnel")
	}
	if c.Server.MaxRooms <= 0 {
		c.Server.MaxRooms = 50
	}
	if c.Server.MaxPeersPerRoom < 1 || c.Server.MaxPeersPerRoom > proto.MaxPeersPerRoom {
		return errors.New("server.max_peers_per_room must be between 1 and 8 (protocol v1 client slot limit)")
	}
	if c.Server.MaxConnections <= 0 {
		c.Server.MaxConnections = 200
	}
	for _, raw := range c.Server.TrustedProxies {
		if _, err := ParseTrustedProxy(raw); err != nil {
			return fmt.Errorf("server.trusted_proxies entry %q: %w", raw, err)
		}
	}
	if len([]byte(c.Server.Password)) > proto.MaxPasswordBytes {
		return fmt.Errorf("server.password exceeds bcrypt limit of %d UTF-8 bytes", proto.MaxPasswordBytes)
	}
	if !c.TLS.Enabled && !c.Server.AllowInsecurePublic && !isLoopbackListen(c.Server.Listen) {
		return errors.New("public plaintext WebSocket is disabled: enable TLS, bind server.listen to loopback, or explicitly set server.allow_insecure_public=true")
	}
	if c.Server.UdpListen != "" && !c.Server.AllowInsecureUDP {
		return errors.New("UDP audio is unencrypted: clear server.udp_listen or explicitly set server.allow_insecure_udp=true for a trusted network")
	}
	switch c.Log.Level {
	case "debug", "info", "warn", "error":
	default:
		c.Log.Level = "info"
	}
	if c.TLS.AutoLetsEncrypt {
		return errors.New("tls.auto_letsencrypt is not implemented; use cert_file+key_file or a TLS reverse proxy")
	}
	if c.TLS.Enabled {
		if c.TLS.CertFile == "" || c.TLS.KeyFile == "" {
			return errors.New("tls.enabled requires cert_file+key_file")
		}
	}
	defaults := Default().RateLimit
	if c.RateLimit.HelloPerIPPerMinute <= 0 {
		c.RateLimit.HelloPerIPPerMinute = defaults.HelloPerIPPerMinute
	}
	if c.RateLimit.RoomCreatePerPeerPerMinute <= 0 {
		c.RateLimit.RoomCreatePerPeerPerMinute = defaults.RoomCreatePerPeerPerMinute
	}
	if c.RateLimit.RoomJoinPerPeerPerMinute <= 0 {
		c.RateLimit.RoomJoinPerPeerPerMinute = defaults.RoomJoinPerPeerPerMinute
	}
	if c.RateLimit.RoomListPerPeerPerMinute <= 0 {
		c.RateLimit.RoomListPerPeerPerMinute = defaults.RoomListPerPeerPerMinute
	}
	if c.RateLimit.ControlPerPeerPerMinute <= 0 {
		c.RateLimit.ControlPerPeerPerMinute = defaults.ControlPerPeerPerMinute
	}
	if c.RateLimit.AudioFramesPerPeerPerSec <= 0 {
		c.RateLimit.AudioFramesPerPeerPerSec = defaults.AudioFramesPerPeerPerSec
	}
	if c.RateLimit.PasswordChecksConcurrent <= 0 {
		c.RateLimit.PasswordChecksConcurrent = defaults.PasswordChecksConcurrent
	}
	return nil
}

// ParseTrustedProxy parses one explicit address or CIDR from trusted_proxies.
// Catch-all prefixes and IPv4-mapped IPv6 spellings are rejected to avoid
// accidentally trusting arbitrary direct clients or creating ambiguous keys.
func ParseTrustedProxy(raw string) (netip.Prefix, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return netip.Prefix{}, errors.New("empty address")
	}
	if addr, err := netip.ParseAddr(raw); err == nil {
		if addr.Zone() != "" || addr.Is4In6() || addr.IsUnspecified() || addr.IsMulticast() {
			return netip.Prefix{}, errors.New("address must be an unambiguous unicast IP")
		}
		addr = addr.Unmap()
		return netip.PrefixFrom(addr, addr.BitLen()), nil
	}
	prefix, err := netip.ParsePrefix(raw)
	if err != nil {
		return netip.Prefix{}, errors.New("must be an IP address or CIDR")
	}
	if prefix.Bits() == 0 || prefix.Addr().Is4In6() || prefix.Addr().IsMulticast() {
		return netip.Prefix{}, errors.New("catch-all, multicast, and IPv4-mapped prefixes are forbidden")
	}
	return prefix.Masked(), nil
}

func isLoopbackListen(listen string) bool {
	host, _, err := net.SplitHostPort(listen)
	if err != nil {
		return false
	}
	host = strings.Trim(host, "[]")
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// HealthURL converts a validated health listener into the only URL the updater
// may probe. It accepts explicit loopback IPv4/IPv6 literals,
// requires a concrete non-zero port, and never carries userinfo, paths, query,
// fragments, or shell-interpreted text from configuration.
func HealthURL(listen string) (string, error) {
	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		return "", errors.New("health listener must be host:port")
	}
	host = strings.Trim(host, "[]")
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return "", errors.New("health listener host must be an explicit loopback IP")
	}
	host = ip.String()
	portNumber, err := strconv.Atoi(port)
	if err != nil || portNumber < 1 || portNumber > 65535 {
		return "", errors.New("health listener port must be between 1 and 65535")
	}
	return "http://" + net.JoinHostPort(host, strconv.Itoa(portNumber)) + "/healthz", nil
}
