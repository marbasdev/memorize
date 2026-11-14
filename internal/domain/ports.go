package domain

import (
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
)

const (
	DefaultDaemonHost = "127.0.0.1"
	DefaultDaemonPort = 18095
	DefaultDaemonAddr = "127.0.0.1:18095"
	DefaultGGUFPort   = 18088
)

// PortConfig holds persistent port preferences for the unified daemon/console and built-in GGUF server.
type PortConfig struct {
	DaemonPort int `json:"daemon_port"`
	GGUFPort   int `json:"gguf_port"`
}

// DaemonRuntimeState publishes the live daemon listener address and metadata for local auto-discovery.
type DaemonRuntimeState struct {
	PID        int    `json:"pid"`
	ListenAddr string `json:"listen_addr"`
	DBPath     string `json:"db_path,omitempty"`
	ModelsDir  string `json:"models_dir,omitempty"`
	GGUFPort   int    `json:"gguf_port,omitempty"`
	GGUFURL    string `json:"gguf_url,omitempty"`
	StartedAt  string `json:"started_at,omitempty"`
}

// DefaultPortConfig returns the default daemon and GGUF ports.
func DefaultPortConfig() PortConfig {
	return PortConfig{
		DaemonPort: DefaultDaemonPort,
		GGUFPort:   DefaultGGUFPort,
	}
}

// ValidatePortConfig verifies valid distinct daemon and GGUF TCP port numbers.
func ValidatePortConfig(daemonPort, ggufPort int) error {
	if daemonPort < 1 || daemonPort > 65535 {
		return fmt.Errorf("invalid daemon port %d (must be 1..65535)", daemonPort)
	}
	if ggufPort < 1 || ggufPort > 65535 {
		return fmt.Errorf("invalid GGUF port %d (must be 1..65535)", ggufPort)
	}
	if daemonPort == ggufPort {
		return fmt.Errorf("daemon port and GGUF port must differ (%d)", daemonPort)
	}
	return nil
}

// ParseHostPort extracts host and port from an address or URL string.
func ParseHostPort(addr string) (string, int, bool) {
	raw := strings.TrimSpace(addr)
	if raw == "" {
		return DefaultDaemonHost, DefaultDaemonPort, false
	}
	if strings.HasPrefix(raw, "http://") || strings.HasPrefix(raw, "https://") {
		if u, err := url.Parse(raw); err == nil && u.Host != "" {
			raw = u.Host
		}
	}
	h, pStr, err := net.SplitHostPort(raw)
	if err != nil {
		return "", 0, false
	}
	p, perr := strconv.Atoi(pStr)
	if perr != nil || p < 0 || p > 65535 {
		return "", 0, false
	}
	h = strings.TrimSpace(h)
	if h == "" {
		h = DefaultDaemonHost
	}
	return h, p, true
}

// IsLoopbackEndpoint reports whether rawURL resolves to 127.0.0.1, localhost, or ::1.
func IsLoopbackEndpoint(rawURL string) bool {
	s := strings.TrimSpace(rawURL)
	if s == "" {
		return false
	}
	if !strings.Contains(s, "://") {
		s = "http://" + s
	}
	u, err := url.Parse(s)
	if err != nil {
		return false
	}
	h := strings.TrimSpace(strings.ToLower(u.Hostname()))
	return h == "127.0.0.1" || h == "localhost" || h == "::1"
}
