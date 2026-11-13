package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/mark3labs/mcp-go/server"

	"memorize/internal/memory"
	"memorize/internal/memory/dashboard"
	"memorize/internal/memory/localmodel"
)

type daemonStatus struct {
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

var daemonProbeHTTP = &http.Client{Timeout: 400 * time.Millisecond}

func resolveDaemonAddr(explicit string) string {
	for _, v := range []string{explicit, os.Getenv("MEMORY_DAEMON_ADDR"), os.Getenv("MEMORY_DASHBOARD_ADDR")} {
		if s := strings.TrimSpace(v); s != "" {
			return s
		}
	}
	return dashboard.DefaultDashboardAddr
}

func resolveDaemonAddrForDB(explicit, dbPath string) string {
	addr, _ := resolveDaemonTargetAddr(explicit, dbPath)
	return addr
}

func daemonLogPath(dbPath string) string {
	if p := strings.TrimSpace(os.Getenv("MEMORY_DAEMON_LOG")); p != "" {
		return p
	}
	if dbPath != "" && dbPath != ":memory:" {
		if abs, err := filepath.Abs(dbPath); err == nil {
			return filepath.Join(filepath.Dir(abs), "daemon.log")
		}
	}
	if dataDir := memory.DefaultDataDir(); dataDir != "" {
		return filepath.Join(dataDir, "daemon.log")
	}
	return filepath.Join(os.TempDir(), fmt.Sprintf("memorize-daemon-%d.log", os.Getuid()))
}

func isLocalRequest(r *http.Request) bool {
	if strings.EqualFold(strings.TrimSpace(r.Header.Get("Sec-Fetch-Site")), "cross-site") {
		return false
	}
	h := strings.TrimSpace(r.Host)
	if h == "" {
		return false
	}
	if hostOnly, _, err := net.SplitHostPort(h); err == nil {
		h = hostOnly
	}
	h = strings.Trim(strings.ToLower(h), "[]")
	if h != "127.0.0.1" && h != "localhost" && h != "::1" {
		return false
	}
	orig := strings.TrimSpace(r.Header.Get("Origin"))
	if orig == "" {
		return true
	}
	u, err := url.Parse(orig)
	if err != nil {
		return false
	}
	oh := strings.ToLower(u.Hostname())
	return oh == "127.0.0.1" || oh == "localhost" || oh == "::1"
}

func isLocalJSONPost(r *http.Request) bool {
	if !isLocalRequest(r) || r.Method != http.MethodPost {
		return false
	}
	ct := strings.ToLower(strings.TrimSpace(r.Header.Get("Content-Type")))
	return strings.HasPrefix(ct, "application/json")
}

func queryDaemonStatus(addr string) (*daemonStatus, error) {
	resp, err := daemonProbeHTTP.Get("http://" + addr + "/api/daemon/status")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("daemon status HTTP %d", resp.StatusCode)
	}
	var st daemonStatus
	if err := json.NewDecoder(resp.Body).Decode(&st); err != nil {
		return nil, err
	}
	return &st, nil
}

func sameFileClean(a, b string) bool {
	if a == "" || b == "" {
		return true
	}
	absA, errA := filepath.Abs(a)
	absB, errB := filepath.Abs(b)
	if errA == nil && errB == nil {
		return filepath.Clean(absA) == filepath.Clean(absB)
	}
	return filepath.Clean(a) == filepath.Clean(b)
}

// ensureDaemonRunning checks if the unified daemon is listening (with a matching DBPath),
// and auto-spawns it in the background if not yet running.
func ensureDaemonRunning(addr, dbPath, modelsDir string) (*daemonStatus, bool) {
	if dbPath == ":memory:" || strings.HasPrefix(dbPath, "file::memory:") {
		return nil, false
	}
	if st, _, ok := discoverLiveDaemonStatus(addr, dbPath); ok {
		return st, true
	}

	explicitOv := explicitDaemonOverride(addr)

	// Acquire a short-lived directory lock so concurrent hook + MCP startups don't double-spawn.
	lockDir := ""
	if absDB, err := filepath.Abs(dbPath); err == nil {
		_ = os.MkdirAll(filepath.Dir(absDB), 0o700)
		lockDir = filepath.Join(filepath.Dir(absDB), ".daemon-spawn.lock")
	}
	haveLock := false
	if lockDir != "" {
		if st, err := os.Stat(lockDir); err == nil && time.Since(st.ModTime()) > 10*time.Second {
			_ = os.Remove(lockDir)
		}
		if os.Mkdir(lockDir, 0o700) == nil {
			haveLock = true
			defer os.Remove(lockDir)
		}
	}

	if haveLock || lockDir == "" {
		exe, err := os.Executable()
		if err != nil || exe == "" {
			return nil, false
		}
		if bestBin, _ := dashboard.NewestWatchedBinary(0); bestBin != "" {
			exe = bestBin
		}
		logFile := openDaemonLogFile(daemonLogPath(dbPath))
		cmdArgs := []string{"-daemon"}
		if explicitOv != "" {
			cmdArgs = append(cmdArgs, "-addr", explicitOv)
		}
		cmd := exec.Command(exe, cmdArgs...)
		cmd.SysProcAttr = daemonSysProcAttr()
		cmd.Env = filterDaemonEnv(os.Environ(), dbPath, modelsDir, explicitOv)
		if logFile != nil {
			cmd.Stdout = logFile
			cmd.Stderr = logFile
		}
		err = cmd.Start()
		if logFile != nil {
			_ = logFile.Close()
		}
		if err != nil {
			return nil, false
		}
		_ = cmd.Process.Release()
	}

	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		time.Sleep(40 * time.Millisecond)
		if st, _, ok := discoverLiveDaemonStatus(addr, dbPath); ok {
			return st, true
		}
	}
	return nil, false
}

func openDaemonLogFile(logPath string) *os.File {
	if logPath == "" {
		return nil
	}
	_ = os.MkdirAll(filepath.Dir(logPath), 0o700)
	if info, err := os.Stat(logPath); err == nil && info.Size() > 2*1024*1024 {
		_ = os.Rename(logPath, logPath+".1")
	}
	f, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil
	}
	return f
}

func buildMCPServer(svc *memory.Service) *server.MCPServer {
	srv := server.NewMCPServer("memorize", version,
		server.WithToolCapabilities(false),
		server.WithResourceCapabilities(false, false),
		server.WithPromptCapabilities(false),
		server.WithRoots(),
		server.WithInstructions(serverInstructions),
	)
	resolver := &scopeResolver{srv: srv, cache: make(map[string][]resolvedWorkspace)}
	registerMCPResourcesAndPrompts(srv)
	registerMCPTools(srv, svc, resolver)
	return srv
}

// runDaemonServer runs the single unified local daemon owning SQLite, localmodel.Manager,
// Web Dashboard UI, Streamable HTTP MCP (/mcp), Stateless Bridge MCP (/api/daemon/mcp),
// Lifecycle Hooks (/api/daemon/hook), and Daemon Control API (/api/daemon/status|restart|stop).
func runDaemonServer(addr, dbPath, modelsDir string, svc *memory.Service, mgr *localmodel.Manager) {
	targetAddr, _ := resolveDaemonTargetAddr(addr, dbPath)
	portCfg := memory.LoadPortConfig(dbPath)
	ln, err := listenDaemonWithFallback(targetAddr, portCfg.GGUFPort)
	if err != nil {
		log.Fatalf("memorize daemon listen error on %s: %v", targetAddr, err)
	}
	boundAddr := ln.Addr().String()
	boundHost, boundPort, _ := memory.ParseHostPort(boundAddr)
	mgr.SetPreferredPortFunc(func() int { return memory.LoadPortConfig(dbPath).GGUFPort })
	mgr.SetExcludePortFunc(func() int { return boundPort })
	mgr.SetDashboardURLFunc(func() string { return "http://" + boundAddr })
	_ = memory.WriteDaemonState(dbPath, memory.DaemonRuntimeState{
		PID:        os.Getpid(),
		ListenAddr: boundAddr,
		DBPath:     dbPath,
		ModelsDir:  modelsDir,
		GGUFPort:   portCfg.GGUFPort,
		StartedAt:  time.Now().UTC().Format(time.RFC3339),
	})
	var skipStateRemove atomic.Bool
	defer func() {
		if !skipStateRemove.Load() {
			memory.RemoveDaemonStateIfPID(dbPath, os.Getpid())
		}
	}()
	startedAt := time.Now()
	logPath := daemonLogPath(dbPath)
	absDB, _ := filepath.Abs(dbPath)
	if absDB == "" {
		absDB = dbPath
	}
	absModels, _ := filepath.Abs(modelsDir)
	if absModels == "" {
		absModels = modelsDir
	}

	type reexecSpec struct {
		bin  string
		addr string
	}

	var srv *http.Server
	var closeOnce sync.Once
	var portMu sync.Mutex
	stopWatch := make(chan struct{})
	reexecCh := make(chan reexecSpec, 1)
	stopRequested := make(chan struct{}, 1)

	closeFn := func() error {
		var cerr error
		closeOnce.Do(func() {
			close(stopWatch)
			if srv != nil {
				cerr = srv.Close()
			}
		})
		return cerr
	}
	defer func() { _ = closeFn() }()

	buildStatus := func() daemonStatus {
		_, binT := dashboard.NewestWatchedBinary(0)
		cfg := memory.LoadPortConfig(dbPath)
		lm := mgr.Status(cfg.GGUFPort)
		return daemonStatus{
			Running:              true,
			PID:                  os.Getpid(),
			Version:              version,
			StartedAt:            startedAt.Format(time.RFC3339),
			UptimeSec:            int64(time.Since(startedAt).Seconds()),
			ListenAddr:           boundAddr,
			DBPath:               absDB,
			ModelsDir:            absModels,
			Provider:             svc.ProviderName(),
			LogPath:              logPath,
			BinMtime:             binT,
			ConfiguredDaemonPort: cfg.DaemonPort,
			ActiveDaemonPort:     boundPort,
			DaemonFallback:       boundPort != cfg.DaemonPort,
			ConfiguredGGUFPort:   cfg.GGUFPort,
			ActiveGGUFPort:       lm.Port,
			GGUFEndpoint:         lm.Endpoint,
			GGUFFallback:         lm.PortFallback,
			GGUFRunning:          lm.ServerRunning,
		}
	}

	applyPorts := func(ctx context.Context, reqDaemonPort, reqGGUFPort int) (portApplyResponse, error) {
		portMu.Lock()
		defer portMu.Unlock()

		curCfg := memory.LoadPortConfig(dbPath)
		dp := reqDaemonPort
		if dp <= 0 {
			dp = curCfg.DaemonPort
		}
		gp := reqGGUFPort
		if gp <= 0 {
			gp = curCfg.GGUFPort
		}
		if err := memory.ValidatePortConfig(dp, gp); err != nil {
			return portApplyResponse{}, err
		}
		if err := memory.SavePortConfig(dbPath, memory.PortConfig{DaemonPort: dp, GGUFPort: gp}); err != nil {
			return portApplyResponse{}, err
		}

		lm := mgr.Status(gp)
		effGGUFPort := gp
		effGGUFEP := fmt.Sprintf("http://127.0.0.1:%d", gp)
		ggufFallback := false
		ggufRunning := lm.ServerRunning
		if lm.ServerRunning {
			if lm.Port > 0 && lm.Port != gp && lm.Supported && lm.ActiveModel != "" {
				bgCtx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
				ep, err := mgr.StartServer(bgCtx, lm.ActiveModel, gp)
				cancel()
				if err != nil {
					return portApplyResponse{}, err
				}
				if ep != "" {
					effGGUFEP = ep
					if _, p, okp := memory.ParseHostPort(ep); okp && p > 0 {
						effGGUFPort = p
						ggufFallback = p != gp
					}
				}
			} else if lm.Port > 0 {
				effGGUFPort = lm.Port
				effGGUFEP = lm.Endpoint
				ggufFallback = lm.PortFallback
			}
		}
		_ = memory.SyncManagedGGUFEndpoint(context.Background(), svc, effGGUFEP)

		res := portApplyResponse{
			OK:               true,
			ConfiguredDaemon: dp,
			ActiveDaemon:     boundPort,
			ListenAddr:       boundAddr,
			DaemonFallback:   boundPort != dp,
			Restarting:       false,
			NewURL:           "http://" + boundAddr,
			ConfiguredGGUF:   gp,
			ActiveGGUF:       effGGUFPort,
			GGUFEndpoint:     effGGUFEP,
			GGUFFallback:     ggufFallback,
			GGUFRunning:      ggufRunning,
		}

		if dp == boundPort {
			_ = memory.WriteDaemonState(dbPath, memory.DaemonRuntimeState{
				PID:        os.Getpid(),
				ListenAddr: boundAddr,
				DBPath:     dbPath,
				ModelsDir:  modelsDir,
				GGUFPort:   effGGUFPort,
				GGUFURL:    effGGUFEP,
				StartedAt:  startedAt.UTC().Format(time.RFC3339),
			})
			return res, nil
		}

		nextAddr, nextPort, nextFB, perr := pickNextDaemonAddrForRestart(boundHost, dp, effGGUFPort)
		if perr != nil {
			return portApplyResponse{}, perr
		}
		res.ActiveDaemon = nextPort
		res.ListenAddr = nextAddr
		res.DaemonFallback = nextFB
		res.Restarting = true
		res.NewURL = "http://" + nextAddr

		go func() {
			time.Sleep(80 * time.Millisecond)
			skipStateRemove.Store(true)
			nextBin, _ := os.Executable()
			select {
			case reexecCh <- reexecSpec{bin: nextBin, addr: nextAddr}:
			default:
			}
			_ = closeFn()
		}()
		return res, nil
	}

	dashHandler := dashboard.NewHandlerWithHooks(svc, mgr, dashboard.ServerHooks{
		YieldFn: closeFn,
		DaemonStatusFn: func() dashboard.DaemonPortState {
			st := buildStatus()
			return dashboard.DaemonPortState{
				PID:            st.PID,
				ListenAddr:     st.ListenAddr,
				UptimeSec:      st.UptimeSec,
				ConfiguredPort: st.ConfiguredDaemonPort,
				ActivePort:     st.ActiveDaemonPort,
				Fallback:       st.DaemonFallback,
			}
		},
		ApplyDaemonPortFn: func(ctx context.Context, dp, gp int) (dashboard.DaemonPortApplyResult, error) {
			res, err := applyPorts(ctx, dp, gp)
			if err != nil {
				return dashboard.DaemonPortApplyResult{}, err
			}
			return dashboard.DaemonPortApplyResult{
				ListenAddr:       res.ListenAddr,
				ActiveDaemonPort: res.ActiveDaemon,
				DaemonFallback:   res.DaemonFallback,
				Restarting:       res.Restarting,
				NewURL:           res.NewURL,
				ActiveGGUFPort:   res.ActiveGGUF,
				GGUFEndpoint:     res.GGUFEndpoint,
				GGUFFallback:     res.GGUFFallback,
				GGUFRunning:      res.GGUFRunning,
			}, nil
		},
	})
	mcpSrv := buildMCPServer(svc)
	streamHTTP := server.NewStreamableHTTPServer(mcpSrv)

	type bridgeSessionEntry struct {
		sess     *server.InProcessSession
		lastSeen time.Time
	}
	const maxBridgeSessions = 256
	var sessMu sync.Mutex
	bridgeSessions := make(map[string]*bridgeSessionEntry)
	getBridgeSession := func(sid string) *server.InProcessSession {
		now := time.Now()
		sessMu.Lock()
		defer sessMu.Unlock()
		if entry, ok := bridgeSessions[sid]; ok {
			entry.lastSeen = now
			return entry.sess
		}
		if len(bridgeSessions) >= maxBridgeSessions {
			var oldestKey string
			var oldestTime time.Time
			for k, v := range bridgeSessions {
				if now.Sub(v.lastSeen) > 6*time.Hour {
					mcpSrv.UnregisterSession(context.Background(), k)
					delete(bridgeSessions, k)
					continue
				}
				if oldestKey == "" || v.lastSeen.Before(oldestTime) {
					oldestKey = k
					oldestTime = v.lastSeen
				}
			}
			if len(bridgeSessions) >= maxBridgeSessions && oldestKey != "" {
				mcpSrv.UnregisterSession(context.Background(), oldestKey)
				delete(bridgeSessions, oldestKey)
			}
		}
		s := server.NewInProcessSession(sid, nil)
		s.Initialize()
		_ = mcpSrv.RegisterSession(context.Background(), s)
		bridgeSessions[sid] = &bridgeSessionEntry{sess: s, lastSeen: now}
		return s
	}

	_, startMtime := dashboard.NewestWatchedBinary(0)

	mux := http.NewServeMux()
	mux.Handle("/mcp", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !isLocalRequest(r) {
			http.Error(w, `{"error":"non-local request blocked"}`, http.StatusForbidden)
			return
		}
		if r.Method == http.MethodPost {
			ct := strings.ToLower(strings.TrimSpace(r.Header.Get("Content-Type")))
			if !strings.HasPrefix(ct, "application/json") {
				http.Error(w, `{"error":"Content-Type application/json required"}`, http.StatusUnsupportedMediaType)
				return
			}
		} else if r.Method != http.MethodGet && r.Method != http.MethodDelete {
			http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
			return
		}
		streamHTTP.ServeHTTP(w, r)
	}))

	mux.HandleFunc("/api/daemon/status", func(w http.ResponseWriter, r *http.Request) {
		if !isLocalRequest(r) {
			http.Error(w, `{"error":"non-local request blocked"}`, http.StatusForbidden)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(buildStatus())
	})

	mux.HandleFunc("/api/daemon/port", func(w http.ResponseWriter, r *http.Request) {
		if !isLocalJSONPost(r) {
			http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
			return
		}
		var req portApplyRequest
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&req); err != nil {
			http.Error(w, `{"error":"invalid port request json"}`, http.StatusBadRequest)
			return
		}
		dp, gp := req.DaemonPort, req.GGUFPort
		if req.Reset {
			dp, gp = memory.DefaultDaemonPort, memory.DefaultGGUFPort
		}
		res, err := applyPorts(r.Context(), dp, gp)
		w.Header().Set("Content-Type", "application/json")
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(portApplyResponse{OK: false, Error: err.Error()})
			return
		}
		_ = json.NewEncoder(w).Encode(res)
	})

	mux.HandleFunc("/api/daemon/restart", func(w http.ResponseWriter, r *http.Request) {
		if !isLocalJSONPost(r) {
			http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
			return
		}
		nextBin, _ := dashboard.NewestWatchedBinary(0)
		if nextBin == "" {
			nextBin, _ = os.Executable()
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"restarting": true,
			"old_pid":    os.Getpid(),
			"binary":     nextBin,
		})
		go func() {
			time.Sleep(35 * time.Millisecond)
			skipStateRemove.Store(true)
			select {
			case reexecCh <- reexecSpec{bin: nextBin, addr: boundAddr}:
			default:
			}
			_ = closeFn()
		}()
	})

	mux.HandleFunc("/api/daemon/stop", func(w http.ResponseWriter, r *http.Request) {
		if !isLocalJSONPost(r) {
			http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"stopping": true,
			"pid":      os.Getpid(),
		})
		go func() {
			time.Sleep(35 * time.Millisecond)
			select {
			case stopRequested <- struct{}{}:
			default:
			}
			_ = mgr.StopServer()
			_ = closeFn()
		}()
	})

	mux.HandleFunc("/api/daemon/mcp", func(w http.ResponseWriter, r *http.Request) {
		if !isLocalJSONPost(r) {
			http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
			return
		}
		rawMsg, err := io.ReadAll(io.LimitReader(r.Body, 4<<20))
		if err != nil || len(bytes.TrimSpace(rawMsg)) == 0 {
			http.Error(w, `{"error":"empty rpc payload"}`, http.StatusBadRequest)
			return
		}
		sid := strings.TrimSpace(r.Header.Get("X-Memorize-Session"))
		if sid == "" {
			sid = "bridge-default"
		}
		var roots []string
		if rawRoots := strings.TrimSpace(r.Header.Get("X-Memorize-Roots")); rawRoots != "" {
			for _, part := range strings.Split(rawRoots, "\t") {
				if part = strings.TrimSpace(part); part != "" {
					roots = append(roots, part)
				}
			}
		}
		ctx := r.Context()
		if acc := strings.TrimSpace(r.Header.Get("X-Memorize-Account")); acc != "" {
			ctx = memory.WithAccount(ctx, acc)
		}
		ctx = withBridgeMeta(ctx, bridgeReqMeta{
			sessionID: sid,
			cwd:       strings.TrimSpace(r.Header.Get("X-Memorize-Cwd")),
			roots:     roots,
		})
		ctx = mcpSrv.WithContext(ctx, getBridgeSession(sid))

		rpcResp := mcpSrv.HandleMessage(ctx, json.RawMessage(rawMsg))
		w.Header().Set("Content-Type", "application/json")
		if rpcResp == nil {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		_ = json.NewEncoder(w).Encode(rpcResp)
	})

	mux.HandleFunc("/api/daemon/hook", func(w http.ResponseWriter, r *http.Request) {
		if !isLocalJSONPost(r) {
			http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
			return
		}
		var req struct {
			Mode      string `json:"mode"`
			Stdin     string `json:"stdin"`
			Cwd       string `json:"cwd"`
			Account   string `json:"account"`
			TimeoutMs int64  `json:"timeout_ms,omitempty"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 2<<20)).Decode(&req); err != nil {
			http.Error(w, `{"error":"invalid hook json"}`, http.StatusBadRequest)
			return
		}
		ctx := r.Context()
		var cancel context.CancelFunc = func() {}
		if req.TimeoutMs > 0 {
			ctx, cancel = context.WithTimeout(ctx, time.Duration(req.TimeoutMs)*time.Millisecond)
		}
		defer cancel()
		if strings.TrimSpace(req.Account) != "" {
			ctx = memory.WithAccount(ctx, req.Account)
		}
		out, code, _ := svc.ExecuteHook(ctx, req.Mode, []byte(req.Stdin), req.Cwd)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"output":    out,
			"exit_code": code,
		})
	})

	mux.Handle("/", dashHandler)
	srv = &http.Server{
		Handler:           dashboard.SecurityHeadersMiddleware(mux),
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    64 << 10,
	}

	if runtime.GOOS != "windows" {
		go func() {
			ticker := time.NewTicker(1 * time.Second)
			defer ticker.Stop()
			for {
				select {
				case <-stopWatch:
					return
				case <-ticker.C:
					if path, t1 := dashboard.NewestWatchedBinary(startMtime); path != "" {
						time.Sleep(150 * time.Millisecond)
						if path2, t2 := dashboard.NewestWatchedBinary(startMtime); path2 == path && t2 == t1 {
							skipStateRemove.Store(true)
							select {
							case reexecCh <- reexecSpec{bin: path, addr: boundAddr}:
							default:
							}
							_ = closeFn()
							return
						}
					}
				}
			}
		}()
	}

	log.Printf("memorize daemon v%s listening on http://%s (pid=%d, db=%s, provider=%s)",
		version, boundAddr, os.Getpid(), absDB, svc.ProviderName())

	if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatalf("memorize daemon serve error: %v", err)
	}

	select {
	case <-stopRequested:
		log.Printf("memorize daemon (pid=%d) stopped cleanly.", os.Getpid())
		return
	case spec := <-reexecCh:
		log.Printf("memorize daemon restarting into %s (%s)...", spec.bin, spec.addr)
		_ = svc.Close()
		reexecArgs := []string{spec.bin, "-daemon", "-addr", spec.addr}
		if err := execInPlace(spec.bin, reexecArgs, filterDaemonEnv(os.Environ(), absDB, absModels, "")); err != nil {
			log.Printf("memorize daemon re-exec error: %v", err)
		}
	default:
	}
}
