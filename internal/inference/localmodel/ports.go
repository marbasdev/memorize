package localmodel

import (
	"fmt"
	"path/filepath"
	"strings"

	"memorize/internal/domain"
	"memorize/internal/infra/config"
)

// SetPreferredPortFunc registers a dynamic callback returning the preferred GGUF TCP port.
func (m *Manager) SetPreferredPortFunc(fn func() int) {
	m.mu.Lock()
	m.preferredPortFn = fn
	m.mu.Unlock()
}

// SetExcludePortFunc registers a callback returning a TCP port (such as the daemon port) to skip during GGUF port fallback.
func (m *Manager) SetExcludePortFunc(fn func() int) {
	m.mu.Lock()
	m.excludePortFn = fn
	m.mu.Unlock()
}

// SetDashboardURLFunc registers a dynamic callback returning the active Web Console URL.
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
	return domain.DefaultGGUFPort
}

func (m *Manager) readPortConfigGGUF() int {
	if m.dir == "" {
		return 0
	}
	return config.LoadPortConfigDir(filepath.Dir(m.dir)).GGUFPort
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
		if st, ok := config.ReadDaemonStateDir(dataDir); ok {
			return "http://" + st.ListenAddr
		}
		cfg := config.LoadPortConfigDir(dataDir)
		return fmt.Sprintf("http://%s:%d", domain.DefaultDaemonHost, cfg.DaemonPort)
	}
	return "http://" + domain.DefaultDaemonAddr
}

func pickAvailableGGUFPort(preferred, exclude int) (chosen int, fallback bool, err error) {
	if preferred < 1 || preferred > 65535 {
		preferred = domain.DefaultGGUFPort
	}
	return config.PickAvailableLoopbackPort(domain.DefaultDaemonHost, preferred, exclude)
}
