package memory

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
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

func DefaultPortConfig() PortConfig {
	return PortConfig{
		DaemonPort: DefaultDaemonPort,
		GGUFPort:   DefaultGGUFPort,
	}
}

func IsPersistentDBPath(dbPath string) bool {
	p := strings.TrimSpace(dbPath)
	if p == "" || p == ":memory:" || strings.HasPrefix(p, "file:") {
		return false
	}
	return true
}

func DataDirForDBPath(dbPath string) string {
	if !IsPersistentDBPath(dbPath) {
		return ""
	}
	return filepath.Dir(filepath.Clean(strings.TrimSpace(dbPath)))
}

func DefaultDataDir() string {
	if v := strings.TrimSpace(os.Getenv("MEMORY_DATA_DIR")); v != "" {
		return filepath.Clean(v)
	}
	if db := strings.TrimSpace(os.Getenv("MEMORY_DB_PATH")); IsPersistentDBPath(db) {
		return filepath.Dir(filepath.Clean(db))
	}
	home, err := os.UserHomeDir()
	if err != nil || strings.TrimSpace(home) == "" {
		return ""
	}
	return filepath.Join(home, ".memorize")
}

func PortsFilePath(dbPath string) string {
	dir := DataDirForDBPath(dbPath)
	if dir == "" {
		return ""
	}
	return filepath.Join(dir, "ports.json")
}

func DaemonStateFilePath(dbPath string) string {
	dir := DataDirForDBPath(dbPath)
	if dir == "" {
		return ""
	}
	return filepath.Join(dir, "daemon.json")
}

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

// LoadPortConfigDir loads persisted port preferences from a data directory, falling back to defaults.
func LoadPortConfigDir(dataDir string) PortConfig {
	cfg := DefaultPortConfig()
	dir := strings.TrimSpace(dataDir)
	if dir == "" {
		return cfg
	}
	b, err := os.ReadFile(filepath.Join(dir, "ports.json"))
	if err != nil {
		return cfg
	}
	var disk PortConfig
	if err := json.Unmarshal(b, &disk); err != nil {
		return cfg
	}
	if disk.DaemonPort >= 1 && disk.DaemonPort <= 65535 {
		cfg.DaemonPort = disk.DaemonPort
	}
	if disk.GGUFPort >= 1 && disk.GGUFPort <= 65535 {
		cfg.GGUFPort = disk.GGUFPort
	}
	if cfg.DaemonPort == cfg.GGUFPort {
		return DefaultPortConfig()
	}
	return cfg
}

func LoadPortConfig(dbPath string) PortConfig {
	return LoadPortConfigDir(DataDirForDBPath(dbPath))
}

func writeFileAtomic0600(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tf, err := os.CreateTemp(dir, filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmp := tf.Name()
	_, werr := tf.Write(data)
	cerr := tf.Close()
	if werr != nil || cerr != nil {
		_ = os.Remove(tmp)
		if werr != nil {
			return werr
		}
		return cerr
	}
	_ = os.Chmod(tmp, 0o600)
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

func SavePortConfig(dbPath string, cfg PortConfig) error {
	if cfg.DaemonPort == 0 {
		cfg.DaemonPort = DefaultDaemonPort
	}
	if cfg.GGUFPort == 0 {
		cfg.GGUFPort = DefaultGGUFPort
	}
	if err := ValidatePortConfig(cfg.DaemonPort, cfg.GGUFPort); err != nil {
		return err
	}
	path := PortsFilePath(dbPath)
	if path == "" {
		return nil
	}
	b, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic0600(path, append(b, '\n'))
}

func ReadDaemonStateDir(dataDir string) (DaemonRuntimeState, bool) {
	dir := strings.TrimSpace(dataDir)
	if dir == "" {
		return DaemonRuntimeState{}, false
	}
	b, err := os.ReadFile(filepath.Join(dir, "daemon.json"))
	if err != nil {
		return DaemonRuntimeState{}, false
	}
	var st DaemonRuntimeState
	if err := json.Unmarshal(b, &st); err != nil {
		return DaemonRuntimeState{}, false
	}
	st.ListenAddr = strings.TrimSpace(st.ListenAddr)
	if st.ListenAddr == "" {
		return DaemonRuntimeState{}, false
	}
	return st, true
}

func ReadDaemonState(dbPath string) (DaemonRuntimeState, bool) {
	return ReadDaemonStateDir(DataDirForDBPath(dbPath))
}

func WriteDaemonState(dbPath string, st DaemonRuntimeState) error {
	path := DaemonStateFilePath(dbPath)
	if path == "" {
		return nil
	}
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic0600(path, append(b, '\n'))
}

func RemoveDaemonStateIfPID(dbPath string, pid int) {
	path := DaemonStateFilePath(dbPath)
	if path == "" {
		return
	}
	if pid > 0 {
		if st, ok := ReadDaemonState(dbPath); ok && st.PID > 0 && st.PID != pid {
			return
		}
	}
	_ = os.Remove(path)
}

// ActiveDaemonURL resolves the best HTTP base URL for reaching the local Web Console.
func ActiveDaemonURL(dbPath string) string {
	dir := DataDirForDBPath(dbPath)
	if dir == "" {
		dir = DefaultDataDir()
	}
	if st, ok := ReadDaemonStateDir(dir); ok && st.ListenAddr != "" {
		return "http://" + st.ListenAddr
	}
	if dir != "" {
		cfg := LoadPortConfigDir(dir)
		return fmt.Sprintf("http://%s:%d", DefaultDaemonHost, cfg.DaemonPort)
	}
	return "http://" + DefaultDaemonAddr
}

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

func SamePathClean(a, b string) bool {
	ca, cb := strings.TrimSpace(a), strings.TrimSpace(b)
	if ca == "" || cb == "" {
		return false
	}
	if aa, err := filepath.Abs(ca); err == nil {
		ca = aa
	}
	if ab, err := filepath.Abs(cb); err == nil {
		cb = ab
	}
	if ra, err := filepath.EvalSymlinks(ca); err == nil {
		ca = ra
	}
	if rb, err := filepath.EvalSymlinks(cb); err == nil {
		cb = rb
	}
	return filepath.Clean(ca) == filepath.Clean(cb)
}

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

func isLoopbackPortFree(host string, port int) bool {
	if port < 1 || port > 65535 {
		return false
	}
	if strings.TrimSpace(host) == "" {
		host = DefaultDaemonHost
	}
	ln, err := net.Listen("tcp", net.JoinHostPort(host, strconv.Itoa(port)))
	if err != nil {
		return false
	}
	_ = ln.Close()
	return true
}

// PickAvailableLoopbackPort returns desiredPort when available, otherwise scans nearby ports
// and finally asks the OS for an ephemeral free port, skipping any port in excludePorts.
func PickAvailableLoopbackPort(host string, desiredPort int, excludePorts ...int) (int, bool, error) {
	if strings.TrimSpace(host) == "" {
		host = DefaultDaemonHost
	}
	excluded := make(map[int]bool, len(excludePorts))
	for _, ex := range excludePorts {
		if ex > 0 {
			excluded[ex] = true
		}
	}
	if desiredPort >= 1 && desiredPort <= 65535 && !excluded[desiredPort] && isLoopbackPortFree(host, desiredPort) {
		return desiredPort, false, nil
	}
	if desiredPort >= 1 && desiredPort <= 65535 {
		for offset := 1; offset <= 32; offset++ {
			cand := desiredPort + offset
			if cand > 65535 {
				cand = 1024 + (cand - 65535)
			}
			if !excluded[cand] && isLoopbackPortFree(host, cand) {
				return cand, true, nil
			}
		}
	}
	for attempt := 0; attempt < 8; attempt++ {
		ln, err := net.Listen("tcp", net.JoinHostPort(host, "0"))
		if err != nil {
			continue
		}
		tcpAddr, _ := ln.Addr().(*net.TCPAddr)
		_ = ln.Close()
		if tcpAddr != nil && tcpAddr.Port > 0 && !excluded[tcpAddr.Port] {
			return tcpAddr.Port, desiredPort > 0 && tcpAddr.Port != desiredPort, nil
		}
	}
	return 0, false, errors.New("no available loopback port found")
}

// SyncManagedGGUFEndpoint updates SQLite gguf_endpoint when it currently points to a managed
// loopback GGUF server (or is unset) and LOCAL_GGUF_URL env override is not set.
func SyncManagedGGUFEndpoint(ctx context.Context, svc *Service, ep string) error {
	if svc == nil || strings.TrimSpace(ep) == "" {
		return nil
	}
	if strings.TrimSpace(os.Getenv("LOCAL_GGUF_URL")) != "" {
		return nil
	}
	raw := svc.GetConfigMap(ctx)
	cur := strings.TrimSpace(raw["gguf_endpoint"])
	if cur != "" && !IsLoopbackEndpoint(cur) {
		return nil
	}
	trimmed := strings.TrimRight(strings.TrimSpace(ep), "/")
	if strings.TrimRight(cur, "/") == trimmed {
		return nil
	}
	return svc.SaveConfig(WithAdminBypass(ctx), map[string]string{"gguf_endpoint": trimmed})
}
