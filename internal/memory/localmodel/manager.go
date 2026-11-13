package localmodel

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"
)

// IsSupportedPlatform reports whether built-in llama-server GGUF management is supported on the current OS.
// On Windows, built-in GGUF is intentionally disabled in favor of Custom Local URL (Ollama / LM Studio / vLLM) or System 1 API (Flide / Jev).
func IsSupportedPlatform() bool {
	return runtime.GOOS == "darwin" || runtime.GOOS == "linux"
}

func unsupportedPlatformErr() error {
	return fmt.Errorf("built-in Local GGUF (llama-server) is not supported on %s — use Custom Local URL (Ollama / LM Studio / vLLM), System 1 API (Flide / Jev), or Gemini API", runtime.GOOS)
}

const DefaultPort = 18088

func isAllowedDownloadScheme(u *url.URL) bool {
	if u == nil {
		return false
	}
	if u.Scheme == "https" {
		return true
	}
	host := strings.ToLower(strings.TrimSpace(u.Hostname()))
	return u.Scheme == "http" && (host == "127.0.0.1" || host == "localhost" || host == "::1")
}

func checkSafeRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= 10 {
		return fmt.Errorf("stopped after 10 redirects")
	}
	if !isAllowedDownloadScheme(req.URL) {
		return fmt.Errorf("blocked insecure redirect target %q", req.URL.Scheme)
	}
	if len(via) > 0 && !strings.EqualFold(via[0].URL.Host, req.URL.Host) {
		req.Header.Del("Authorization")
	}
	return nil
}

var (
	safeFileRe     = regexp.MustCompile(`^[A-Za-z0-9._-]+\.gguf$`)
	probeClient    = &http.Client{Timeout: 250 * time.Millisecond}
	apiClient      = &http.Client{Timeout: 12 * time.Second, CheckRedirect: checkSafeRedirect}
	downloadClient = &http.Client{
		Timeout:       0, // Streaming multi-GB GGUF downloads are bounded by ctx cancellation, not fixed wall-clock timeout
		CheckRedirect: checkSafeRedirect,
		Transport: &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			ResponseHeaderTimeout: 15 * time.Second,
			IdleConnTimeout:       90 * time.Second,
		},
	}
)

type InstalledModel struct {
	FileName  string `json:"file_name"`
	SizeBytes int64  `json:"size_bytes"`
}

type DownloadJob struct {
	Active          bool    `json:"active"`
	Kind            string  `json:"kind"` // "runtime" or "model"
	Target          string  `json:"target"`
	Phase           string  `json:"phase"`
	DownloadedBytes int64   `json:"downloaded_bytes"`
	TotalBytes      int64   `json:"total_bytes"`
	Percent         float64 `json:"percent"`
	Error           string  `json:"error,omitempty"`
}

type pidState struct {
	PID         int    `json:"pid"`
	Model       string `json:"model"`
	Port        int    `json:"port"`
	DesiredPort int    `json:"desired_port,omitempty"`
}

type StatusSnapshot struct {
	Supported        bool             `json:"supported"`
	OS               string           `json:"os"`
	ModelsDir        string           `json:"models_dir"`
	Hardware         HardwareSpec     `json:"hardware"`
	RuntimeInstalled bool             `json:"runtime_installed"`
	RuntimePath      string           `json:"runtime_path"`
	ServerRunning    bool             `json:"server_running"`
	ServerPID        int              `json:"server_pid"`
	ActiveModel      string           `json:"active_model"`
	Endpoint         string           `json:"endpoint"`
	Port             int              `json:"port"`
	DesiredPort      int              `json:"desired_port"`
	PortFallback     bool             `json:"port_fallback,omitempty"`
	Installed        []InstalledModel `json:"installed"`
	Presets          []Preset         `json:"presets"`
	Download         DownloadJob      `json:"download"`
}

type Manager struct {
	dir             string
	srvMu           sync.Mutex
	mu              sync.Mutex
	job             DownloadJob
	cancel          context.CancelFunc
	preferredPortFn func() int
	excludePortFn   func() int
	dashboardURLFn  func() string
}

func NewManager(modelsDir string) *Manager {
	d := strings.TrimSpace(modelsDir)
	if d == "" {
		d = strings.TrimSpace(os.Getenv("MEMORY_MODELS_DIR"))
	}
	if d == "" {
		d = "models"
	}
	if abs, err := filepath.Abs(d); err == nil {
		d = abs
	}
	return &Manager{dir: d}
}

func (m *Manager) RuntimeDir() string  { return filepath.Join(m.dir, "runtime") }
func (m *Manager) pidFilePath() string { return filepath.Join(m.RuntimeDir(), "llama-server.json") }
func (m *Manager) logFilePath() string { return filepath.Join(m.RuntimeDir(), "llama-server.log") }

func (m *Manager) RuntimeBinary() string {
	managed := filepath.Join(m.RuntimeDir(), runtimeBinaryName())
	if st, err := os.Stat(managed); err == nil && !st.IsDir() {
		return managed
	}
	if sys, err := exec.LookPath("llama-server"); err == nil {
		return sys
	}
	return ""
}

func (m *Manager) installedModels() []InstalledModel {
	entries, _ := os.ReadDir(m.dir)
	installed := make([]InstalledModel, 0)
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(strings.ToLower(e.Name()), ".gguf") {
			continue
		}
		if info, err := e.Info(); err == nil {
			installed = append(installed, InstalledModel{FileName: e.Name(), SizeBytes: info.Size()})
		}
	}
	sort.Slice(installed, func(i, j int) bool { return installed[i].FileName < installed[j].FileName })
	return installed
}

func (m *Manager) Status(port int) StatusSnapshot {
	targetPort := m.resolveTargetPort(port)
	if !IsSupportedPlatform() {
		return StatusSnapshot{
			Supported:   false,
			OS:          runtime.GOOS,
			ModelsDir:   m.dir,
			Hardware:    DetectHardware(),
			Endpoint:    fmt.Sprintf("http://127.0.0.1:%d", targetPort),
			Port:        targetPort,
			DesiredPort: targetPort,
			Installed:   []InstalledModel{},
			Presets:     []Preset{},
		}
	}
	rt := m.RuntimeBinary()
	installed := m.installedModels()

	ps := m.readPIDState()
	epPort := targetPort
	running := false
	activeModel := ""
	pid := 0
	if ps.Port > 0 && ps.PID > 0 {
		candEP := fmt.Sprintf("http://127.0.0.1:%d", ps.Port)
		if probeHealth(candEP) && isManagedServerPID(ps) {
			running = true
			epPort = ps.Port
			activeModel = ps.Model
			pid = ps.PID
		}
	}
	if !running {
		candEP := fmt.Sprintf("http://127.0.0.1:%d", targetPort)
		if probeHealth(candEP) {
			running = true
			epPort = targetPort
			if ps.Port == targetPort {
				activeModel = ps.Model
				pid = ps.PID
			}
		}
	}
	ep := fmt.Sprintf("http://127.0.0.1:%d", epPort)

	m.mu.Lock()
	job := m.job
	m.mu.Unlock()

	return StatusSnapshot{
		Supported:        true,
		OS:               runtime.GOOS,
		ModelsDir:        m.dir,
		Hardware:         DetectHardware(),
		RuntimeInstalled: rt != "",
		RuntimePath:      rt,
		ServerRunning:    running,
		ServerPID:        pid,
		ActiveModel:      activeModel,
		Endpoint:         ep,
		Port:             epPort,
		DesiredPort:      targetPort,
		PortFallback:     running && epPort != targetPort,
		Installed:        installed,
		Presets:          Presets(),
		Download:         job,
	}
}

func probeHealth(endpoint string) bool {
	resp, err := probeClient.Get(strings.TrimRight(endpoint, "/") + "/health")
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

func (m *Manager) readPIDState() pidState {
	var ps pidState
	if b, err := os.ReadFile(m.pidFilePath()); err == nil {
		_ = json.Unmarshal(b, &ps)
	}
	return ps
}

func (m *Manager) StartServer(ctx context.Context, modelFile string, port int) (string, error) {
	if !IsSupportedPlatform() {
		return "", unsupportedPlatformErr()
	}
	m.srvMu.Lock()
	defer m.srvMu.Unlock()
	return m.startServerLocked(ctx, modelFile, port)
}

func (m *Manager) ensureRuntimeInstalled(ctx context.Context) (string, error) {
	if bin := m.RuntimeBinary(); bin != "" {
		return bin, nil
	}
	m.mu.Lock()
	if m.job.Active {
		m.mu.Unlock()
		return "", fmt.Errorf("download in progress; please wait until finished")
	}
	m.job = DownloadJob{Active: true, Kind: "runtime", Target: "llama-server", Phase: "auto-installing runtime"}
	m.mu.Unlock()

	err := m.InstallRuntimeSync(ctx)
	m.finishJob(err)
	if err != nil {
		return "", fmt.Errorf("runtime auto-install failed: %w", err)
	}
	bin := m.RuntimeBinary()
	if bin == "" {
		return "", fmt.Errorf("llama-server binary not found after install")
	}
	return bin, nil
}

func validateSafeGGUFName(raw string) (string, error) {
	fname := strings.TrimSpace(raw)
	if fname == "" || strings.Contains(fname, "..") || !safeFileRe.MatchString(fname) {
		return "", fmt.Errorf("invalid .gguf model filename %q", raw)
	}
	return fname, nil
}

func (m *Manager) startServerLocked(ctx context.Context, modelFile string, port int) (string, error) {
	desiredPort := m.resolveTargetPort(port)
	fname, err := validateSafeGGUFName(modelFile)
	if err != nil {
		return "", err
	}
	modelPath := filepath.Join(m.dir, fname)
	if _, err := os.Stat(modelPath); err != nil {
		return "", fmt.Errorf("model file not found: %s", fname)
	}
	if err := os.MkdirAll(m.RuntimeDir(), 0o755); err != nil {
		return "", err
	}

	bin, err := m.ensureRuntimeInstalled(ctx)
	if err != nil {
		return "", err
	}

	_ = m.stopServerLocked()

	chosenPort, _, perr := pickAvailableGGUFPort(desiredPort, m.excludedPort())
	if perr != nil {
		return "", perr
	}

	logF, err := os.OpenFile(m.logFilePath(), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return "", err
	}
	cmd := exec.Command(bin,
		"-m", modelPath,
		"--host", "127.0.0.1",
		"--port", fmt.Sprintf("%d", chosenPort),
		"-ngl", "99",
		"-c", "4096",
	)
	cmd.Dir = filepath.Dir(bin)
	cmd.Stdout = logF
	cmd.Stderr = logF
	cmd.SysProcAttr = serverSysProcAttr()

	if err := cmd.Start(); err != nil {
		_ = logF.Close()
		return "", fmt.Errorf("failed to start llama-server: %w", err)
	}
	_ = logF.Close()
	pid := cmd.Process.Pid

	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()

	psBytes, _ := json.Marshal(pidState{PID: pid, Model: fname, Port: chosenPort, DesiredPort: desiredPort})
	_ = os.WriteFile(m.pidFilePath(), psBytes, 0o600)

	ep := fmt.Sprintf("http://127.0.0.1:%d", chosenPort)
	deadline := time.Now().Add(25 * time.Second)
	for time.Now().Before(deadline) {
		if probeHealth(ep) {
			return ep, nil
		}
		select {
		case werr := <-exited:
			_ = os.Remove(m.pidFilePath())
			return "", fmt.Errorf("llama-server exited early (%v): %s", werr, m.readLogTail())
		case <-ctx.Done():
			_ = m.stopServerLocked()
			return "", ctx.Err()
		case <-time.After(150 * time.Millisecond):
		}
	}
	_ = m.stopServerLocked()
	return "", fmt.Errorf("timed out waiting for llama-server /health on %s (log: %s)", ep, m.readLogTail())
}

func (m *Manager) readLogTail() string {
	tail, _ := os.ReadFile(m.logFilePath())
	if len(tail) > 500 {
		tail = tail[len(tail)-500:]
	}
	return strings.TrimSpace(string(tail))
}

func isManagedServerPID(ps pidState) bool {
	if ps.PID <= 0 {
		return false
	}
	if ps.Port > 0 && probeHealth(fmt.Sprintf("http://127.0.0.1:%d", ps.Port)) {
		return true
	}
	return isLlamaProcess(ps.PID)
}

func (m *Manager) StopServer() error {
	m.srvMu.Lock()
	defer m.srvMu.Unlock()
	return m.stopServerLocked()
}

func (m *Manager) stopServerLocked() error {
	ps := m.readPIDState()
	_ = os.Remove(m.pidFilePath())
	if !isManagedServerPID(ps) {
		return nil
	}
	terminateProcessGroup(ps.PID)
	if ps.Port > 0 {
		deadline := time.Now().Add(500 * time.Millisecond)
		for time.Now().Before(deadline) {
			if isLoopbackTCPPortFree(ps.Port) {
				break
			}
			time.Sleep(25 * time.Millisecond)
		}
	}
	return nil
}

func (m *Manager) EnsureRunning(ctx context.Context, modelFile string, port int) error {
	_, err := m.EnsureRunningEndpoint(ctx, modelFile, port)
	return err
}

func (m *Manager) EnsureRunningEndpoint(ctx context.Context, modelFile string, port int) (string, error) {
	if !IsSupportedPlatform() {
		return "", unsupportedPlatformErr()
	}
	targetPort := m.resolveTargetPort(port)
	if ep, ok := m.runningEndpointFor(modelFile, targetPort); ok {
		return ep, nil
	}
	m.srvMu.Lock()
	defer m.srvMu.Unlock()
	if ep, ok := m.runningEndpointFor(modelFile, targetPort); ok {
		return ep, nil
	}
	targetModel := strings.TrimSpace(modelFile)
	if targetModel == "" {
		if ps := m.readPIDState(); ps.Model != "" {
			if _, err := os.Stat(filepath.Join(m.dir, filepath.Base(ps.Model))); err == nil {
				targetModel = ps.Model
			}
		}
	}
	if targetModel == "" {
		installed := m.installedModels()
		if len(installed) == 0 {
			return "", fmt.Errorf("no local GGUF model installed in %s — open %s (Engines tab) to download a model in 1 click, or configure Gemini / Custom Local URL", m.dir, m.dashboardURL())
		}
		targetModel = installed[0].FileName
	}
	return m.startServerLocked(ctx, targetModel, targetPort)
}

func (m *Manager) runningEndpointFor(modelFile string, targetPort int) (string, bool) {
	ps := m.readPIDState()
	wantModel := strings.TrimSpace(modelFile)
	if ps.PID > 0 && ps.Port > 0 && isManagedServerPID(ps) {
		sameModel := wantModel == "" || ps.Model == wantModel
		sameDesired := ps.DesiredPort == 0 || ps.DesiredPort == targetPort || ps.Port == targetPort
		if sameModel && sameDesired {
			candEP := fmt.Sprintf("http://127.0.0.1:%d", ps.Port)
			if probeHealth(candEP) {
				return candEP, true
			}
		}
	}
	ep := fmt.Sprintf("http://127.0.0.1:%d", targetPort)
	if probeHealth(ep) && (wantModel == "" || ps.Model == "" || ps.Model == wantModel) {
		return ep, true
	}
	return "", false
}

func (m *Manager) DeleteModel(modelFile string) error {
	fname, err := validateSafeGGUFName(modelFile)
	if err != nil {
		return err
	}
	ps := m.readPIDState()
	if ps.Model == fname {
		_ = m.StopServer()
	}
	return os.Remove(filepath.Join(m.dir, fname))
}

func (m *Manager) AutoBootstrap(ctx context.Context, port int, onActivated func(endpoint, model string)) (string, string, error) {
	if !IsSupportedPlatform() {
		return "", "", unsupportedPlatformErr()
	}
	targetPort := m.resolveTargetPort(port)
	st := m.Status(targetPort)
	if st.ServerRunning && st.ActiveModel != "" {
		if onActivated != nil {
			onActivated(st.Endpoint, st.ActiveModel)
		}
		return st.Endpoint, st.ActiveModel, nil
	}
	if len(st.Installed) > 0 {
		model := st.Installed[0].FileName
		ep, err := m.StartServer(ctx, model, targetPort)
		if err != nil {
			return "", model, err
		}
		if onActivated != nil {
			onActivated(ep, model)
		}
		return ep, model, nil
	}

	preset := SelectBootstrapPreset()
	m.mu.Lock()
	if m.job.Active {
		m.mu.Unlock()
		return fmt.Sprintf("http://127.0.0.1:%d", targetPort), preset.FileName, nil
	}
	dlCtx, cancel := context.WithCancel(ctx)
	m.cancel = cancel
	m.job = DownloadJob{Active: true, Kind: "model", Target: preset.FileName, Phase: "auto-bootstrapping"}
	m.mu.Unlock()

	go func() {
		defer cancel()
		err := m.downloadFileWithProgress(dlCtx, preset.URL, filepath.Join(m.dir, preset.FileName))
		m.finishJob(err)
		if err != nil {
			return
		}
		if ep, serr := m.StartServer(dlCtx, preset.FileName, targetPort); serr == nil && onActivated != nil {
			onActivated(ep, preset.FileName)
		}
	}()
	return fmt.Sprintf("http://127.0.0.1:%d", targetPort), preset.FileName, nil
}
