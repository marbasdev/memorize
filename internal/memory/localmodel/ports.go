package localmodel

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

func (m *Manager) SetPreferredPortFunc(fn func() int) {
	m.mu.Lock()
	m.preferredPortFn = fn
	m.mu.Unlock()
}

func (m *Manager) SetExcludePortFunc(fn func() int) {
	m.mu.Lock()
	m.excludePortFn = fn
	m.mu.Unlock()
}

func (m *Manager) SetDashboardURLFunc(fn func() string) {
	m.mu.Lock()
	m.dashboardURLFn = fn
	m.mu.Unlock()
}

func (m *Manager) excludedPort() int {
	m.mu.Lock()
	fn := m.excludePortFn
	m.mu.Unlock()
	if fn != nil {
		return fn()
	}
	return 0
}

func (m *Manager) resolveTargetPort(port int) int {
	if port > 0 && port <= 65535 {
		return port
	}
	m.mu.Lock()
	fn := m.preferredPortFn
	m.mu.Unlock()
	if fn != nil {
		if p := fn(); p > 0 && p <= 65535 {
			return p
		}
	}
	if p := m.readPortConfigGGUF(); p > 0 {
		return p
	}
	return DefaultPort
}

func (m *Manager) readPortConfigGGUF() int {
	if m.dir == "" {
		return 0
	}
	b, err := os.ReadFile(filepath.Join(filepath.Dir(m.dir), "ports.json"))
	if err != nil {
		return 0
	}
	var cfg struct {
		GGUFPort int `json:"gguf_port"`
	}
	if err := json.Unmarshal(b, &cfg); err != nil {
		return 0
	}
	if cfg.GGUFPort >= 1 && cfg.GGUFPort <= 65535 {
		return cfg.GGUFPort
	}
	return 0
}

func (m *Manager) dashboardURL() string {
	m.mu.Lock()
	fn := m.dashboardURLFn
	m.mu.Unlock()
	if fn != nil {
		if u := strings.TrimSpace(fn()); u != "" {
			return strings.TrimRight(u, "/")
		}
	}
	if m.dir != "" {
		dataDir := filepath.Dir(m.dir)
		if b, err := os.ReadFile(filepath.Join(dataDir, "daemon.json")); err == nil {
			var st struct {
				ListenAddr string `json:"listen_addr"`
			}
			if json.Unmarshal(b, &st) == nil && strings.TrimSpace(st.ListenAddr) != "" {
				return "http://" + strings.TrimSpace(st.ListenAddr)
			}
		}
		if b, err := os.ReadFile(filepath.Join(dataDir, "ports.json")); err == nil {
			var cfg struct {
				DaemonPort int `json:"daemon_port"`
			}
			if json.Unmarshal(b, &cfg) == nil && cfg.DaemonPort >= 1 && cfg.DaemonPort <= 65535 {
				return fmt.Sprintf("http://127.0.0.1:%d", cfg.DaemonPort)
			}
		}
	}
	return "http://127.0.0.1:18095"
}

func isLoopbackTCPPortFree(port int) bool {
	if port < 1 || port > 65535 {
		return false
	}
	ln, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		return false
	}
	_ = ln.Close()
	return true
}

func pickAvailableGGUFPort(preferred, exclude int) (chosen int, fallback bool, err error) {
	if preferred < 1 || preferred > 65535 {
		preferred = DefaultPort
	}
	if preferred != exclude && isLoopbackTCPPortFree(preferred) {
		return preferred, false, nil
	}
	for offset := 1; offset <= 32; offset++ {
		cand := preferred + offset
		if cand > 65535 {
			cand = 1024 + (cand - 65535)
		}
		if cand != exclude && isLoopbackTCPPortFree(cand) {
			return cand, true, nil
		}
	}
	for attempt := 0; attempt < 8; attempt++ {
		ln, lerr := net.Listen("tcp", "127.0.0.1:0")
		if lerr != nil {
			continue
		}
		tcpAddr, _ := ln.Addr().(*net.TCPAddr)
		_ = ln.Close()
		if tcpAddr != nil && tcpAddr.Port > 0 && tcpAddr.Port != exclude {
			return tcpAddr.Port, true, nil
		}
	}
	return 0, false, errors.New("no available loopback port found for llama-server")
}
