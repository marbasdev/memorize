package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"memorize/internal/domain"
)

// IsPersistentDBPath reports whether dbPath refers to a disk-backed SQLite file rather than in-memory storage.
func IsPersistentDBPath(dbPath string) bool {
	p := strings.TrimSpace(dbPath)
	if p == "" || p == ":memory:" || strings.HasPrefix(p, "file:") {
		return false
	}
	return true
}

// DataDirForDBPath returns the directory containing dbPath when it is disk-backed.
func DataDirForDBPath(dbPath string) string {
	if !IsPersistentDBPath(dbPath) {
		return ""
	}
	return filepath.Dir(filepath.Clean(strings.TrimSpace(dbPath)))
}

// DefaultDataDir resolves the memorize runtime directory from MEMORY_DATA_DIR, MEMORY_DB_PATH, or ~/.memorize.
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

// PortsFilePath returns the ports.json path next to dbPath.
func PortsFilePath(dbPath string) string {
	dir := DataDirForDBPath(dbPath)
	if dir == "" {
		return ""
	}
	return filepath.Join(dir, "ports.json")
}

// DaemonStateFilePath returns the daemon.json runtime descriptor path next to dbPath.
func DaemonStateFilePath(dbPath string) string {
	dir := DataDirForDBPath(dbPath)
	if dir == "" {
		return ""
	}
	return filepath.Join(dir, "daemon.json")
}

// DaemonLogPath returns the daemon log path from MEMORY_DAEMON_LOG, adjacent to dbPath, or OS temp fallback.
func DaemonLogPath(dbPath string) string {
	if p := strings.TrimSpace(os.Getenv("MEMORY_DAEMON_LOG")); p != "" {
		return p
	}
	if IsPersistentDBPath(dbPath) {
		if abs, err := filepath.Abs(dbPath); err == nil {
			return filepath.Join(filepath.Dir(abs), "daemon.log")
		}
	}
	if dir := DefaultDataDir(); dir != "" {
		return filepath.Join(dir, "daemon.log")
	}
	return filepath.Join(os.TempDir(), fmt.Sprintf("memorize-daemon-%d.log", os.Getuid()))
}

// LoadPortConfigDir loads persisted port preferences from dataDir, falling back to defaults.
func LoadPortConfigDir(dataDir string) domain.PortConfig {
	cfg := domain.DefaultPortConfig()
	dir := strings.TrimSpace(dataDir)
	if dir == "" {
		return cfg
	}
	b, err := os.ReadFile(filepath.Join(dir, "ports.json"))
	if err != nil {
		return cfg
	}
	var disk domain.PortConfig
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
		return domain.DefaultPortConfig()
	}
	return cfg
}

// LoadPortConfig loads persisted port preferences for the data directory holding dbPath.
func LoadPortConfig(dbPath string) domain.PortConfig {
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

// RestrictDBPermissions enforces 0600 permissions on SQLite DB and WAL sidecar files.
func RestrictDBPermissions(dbPath string) {
	p := strings.TrimSpace(dbPath)
	if p == "" || p == ":memory:" || strings.HasPrefix(p, "file::memory:") {
		return
	}
	for _, suf := range []string{"", "-wal", "-shm"} {
		_ = os.Chmod(p+suf, 0o600)
	}
}

// EnsurePrivateDBFile pre-creates the SQLite file with 0600 permissions inside a 0700 parent dir.
func EnsurePrivateDBFile(dbPath string) {
	p := strings.TrimSpace(dbPath)
	if p == "" || p == ":memory:" || strings.HasPrefix(p, "file:") {
		return
	}
	if dir := filepath.Dir(p); dir != "" && dir != "." {
		_ = os.MkdirAll(dir, 0o700)
	}
	if f, err := os.OpenFile(p, os.O_CREATE|os.O_RDWR, 0o600); err == nil {
		_ = f.Close()
	}
	RestrictDBPermissions(p)
}

// SavePortConfig persists validated daemon/GGUF port preferences atomically with 0600 permissions.
func SavePortConfig(dbPath string, cfg domain.PortConfig) error {
	if cfg.DaemonPort == 0 {
		cfg.DaemonPort = domain.DefaultDaemonPort
	}
	if cfg.GGUFPort == 0 {
		cfg.GGUFPort = domain.DefaultGGUFPort
	}
	if err := domain.ValidatePortConfig(cfg.DaemonPort, cfg.GGUFPort); err != nil {
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

// ReadDaemonStateDir reads daemon.json from dataDir.
func ReadDaemonStateDir(dataDir string) (domain.DaemonRuntimeState, bool) {
	dir := strings.TrimSpace(dataDir)
	if dir == "" {
		return domain.DaemonRuntimeState{}, false
	}
	b, err := os.ReadFile(filepath.Join(dir, "daemon.json"))
	if err != nil {
		return domain.DaemonRuntimeState{}, false
	}
	var st domain.DaemonRuntimeState
	if err := json.Unmarshal(b, &st); err != nil {
		return domain.DaemonRuntimeState{}, false
	}
	st.ListenAddr = strings.TrimSpace(st.ListenAddr)
	if st.ListenAddr == "" {
		return domain.DaemonRuntimeState{}, false
	}
	return st, true
}

// ReadDaemonState reads daemon.json adjacent to dbPath.
func ReadDaemonState(dbPath string) (domain.DaemonRuntimeState, bool) {
	return ReadDaemonStateDir(DataDirForDBPath(dbPath))
}

// WriteDaemonState writes daemon.json atomically next to dbPath.
func WriteDaemonState(dbPath string, st domain.DaemonRuntimeState) error {
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

// RemoveDaemonStateIfPID removes daemon.json when empty or matching pid.
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
		return fmt.Sprintf("http://%s:%d", domain.DefaultDaemonHost, cfg.DaemonPort)
	}
	return "http://" + domain.DefaultDaemonAddr
}

// SamePathClean canonicalizes two paths including symlinks and checks equality.
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

// IsLoopbackPortFree reports whether host:port can currently be bound via TCP.
func IsLoopbackPortFree(host string, port int) bool {
	if port < 1 || port > 65535 {
		return false
	}
	if strings.TrimSpace(host) == "" {
		host = domain.DefaultDaemonHost
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
		host = domain.DefaultDaemonHost
	}
	excluded := make(map[int]bool, len(excludePorts))
	for _, ex := range excludePorts {
		if ex > 0 {
			excluded[ex] = true
		}
	}
	if desiredPort >= 1 && desiredPort <= 65535 && !excluded[desiredPort] && IsLoopbackPortFree(host, desiredPort) {
		return desiredPort, false, nil
	}
	if desiredPort >= 1 && desiredPort <= 65535 {
		for offset := 1; offset <= 32; offset++ {
			cand := desiredPort + offset
			if cand > 65535 {
				cand = 1024 + (cand - 65535)
			}
			if !excluded[cand] && IsLoopbackPortFree(host, cand) {
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
