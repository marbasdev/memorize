package http

import "context"

// DaemonStatus describes the runtime state returned by /api/daemon/status.
type DaemonStatus struct {
	Running              bool   `json:"running"`
	PID                  int    `json:"pid"`
	Version              string `json:"version"`
	StartedAt            string `json:"started_at"`
	UptimeSec            int64  `json:"uptime_sec"`
	ListenAddr           string `json:"listen_addr"`
	DBPath               string `json:"db_path"`
	ModelsDir            string `json:"models_dir"`
	Provider             string `json:"provider"`
	LogPath              string `json:"log_path"`
	BinMtime             int64  `json:"bin_mtime"`
	ConfiguredDaemonPort int    `json:"configured_daemon_port,omitempty"`
	ActiveDaemonPort     int    `json:"active_daemon_port,omitempty"`
	DaemonFallback       bool   `json:"daemon_fallback,omitempty"`
	ConfiguredGGUFPort   int    `json:"configured_gguf_port,omitempty"`
	ActiveGGUFPort       int    `json:"active_gguf_port,omitempty"`
	GGUFEndpoint         string `json:"gguf_endpoint,omitempty"`
	GGUFFallback         bool   `json:"gguf_fallback,omitempty"`
	GGUFRunning          bool   `json:"gguf_running,omitempty"`
}

// DaemonPortState exposes live daemon port status to the dashboard console.
type DaemonPortState struct {
	PID            int    `json:"pid"`
	ListenAddr     string `json:"listen_addr"`
	UptimeSec      int64  `json:"uptime_sec"`
	ConfiguredPort int    `json:"configured_daemon_port"`
	ActivePort     int    `json:"active_daemon_port"`
	Fallback       bool   `json:"daemon_fallback"`
}

// DaemonPortApplyResult summarizes the outcome of a live port reconfiguration request.
type DaemonPortApplyResult struct {
	ListenAddr       string `json:"listen_addr"`
	ActiveDaemonPort int    `json:"active_daemon_port"`
	DaemonFallback   bool   `json:"daemon_fallback"`
	Restarting       bool   `json:"restarting"`
	NewURL           string `json:"new_url"`
	ActiveGGUFPort   int    `json:"active_gguf_port"`
	GGUFEndpoint     string `json:"gguf_endpoint"`
	GGUFFallback     bool   `json:"gguf_fallback"`
	GGUFRunning      bool   `json:"gguf_running"`
}

// PortApplyRequest carries a port change or reset request for /api/daemon/port.
type PortApplyRequest struct {
	DaemonPort int  `json:"daemon_port"`
	GGUFPort   int  `json:"gguf_port"`
	Reset      bool `json:"reset,omitempty"`
}

// PortApplyResponse returns configured and active ports after applying a port update.
type PortApplyResponse struct {
	OK               bool   `json:"ok"`
	ConfiguredDaemon int    `json:"configured_daemon_port"`
	ActiveDaemon     int    `json:"active_daemon_port"`
	ListenAddr       string `json:"listen_addr"`
	DaemonFallback   bool   `json:"daemon_fallback"`
	Restarting       bool   `json:"restarting"`
	NewURL           string `json:"new_url"`
	ConfiguredGGUF   int    `json:"configured_gguf_port"`
	ActiveGGUF       int    `json:"active_gguf_port"`
	GGUFEndpoint     string `json:"gguf_endpoint"`
	GGUFFallback     bool   `json:"gguf_fallback"`
	GGUFRunning      bool   `json:"gguf_running"`
	Error            string `json:"error,omitempty"`
}

// DashboardHooks injects optional daemon lifecycle callbacks into dashboard HTTP handlers.
type DashboardHooks struct {
	YieldFn           func() error
	BinMtimeFn        func() int64
	DaemonStatusFn    func() DaemonPortState
	ApplyDaemonPortFn func(ctx context.Context, daemonPort, ggufPort int) (DaemonPortApplyResult, error)
}
