package localdaemon

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/mark3labs/mcp-go/server"

	"memorize/internal/domain"
	"memorize/internal/inference/localmodel"
	"memorize/internal/inference/router"
	"memorize/internal/infra/config"
	"memorize/internal/modules/guardrail"
	"memorize/internal/modules/memory"
	"memorize/internal/modules/progress"
	"memorize/internal/transport/cli"
	"memorize/internal/transport/dashboard"
	thttp "memorize/internal/transport/http"
	transportmcp "memorize/internal/transport/mcp"
)

type runtimeBundle struct {
	mgr  *localmodel.Manager
	mem  *memory.Service
	prog *progress.Service
	gr   *guardrail.Service
}

func (b *runtimeBundle) close() {
	if b != nil && b.mem != nil {
		_ = b.mem.Close()
	}
}

func openRuntime(dbPath, modelsDir string, autoBootstrap bool) (*runtimeBundle, error) {
	mgr := localmodel.NewManager(modelsDir)
	mem, err := memory.Open(dbPath, router.SelectStartupEvaluator(mgr))
	if err != nil {
		return nil, err
	}
	mem.SetAutoGitSync(os.Getenv("MEMORY_AUTO_GIT_SYNC") == "1")
	mem.Router().AttachLocalManager(mgr)
	prog := progress.New(mem)
	gr := guardrail.New(mem, prog)
	if autoBootstrap {
		maybeAutoBootstrap(context.Background(), mem, mgr)
	}
	return &runtimeBundle{mgr: mgr, mem: mem, prog: prog, gr: gr}, nil
}

func maybeAutoBootstrap(ctx context.Context, mem *memory.Service, mgr *localmodel.Manager) {
	if !localmodel.IsSupportedPlatform() {
		return
	}
	cfg := mem.GetConfigMap(ctx)
	if strings.TrimSpace(cfg["active_provider"]) != "" ||
		strings.TrimSpace(os.Getenv("MEMORY_PROVIDER")) != "" ||
		strings.TrimSpace(os.Getenv("GEMINI_API_KEY")) != "" ||
		strings.TrimSpace(os.Getenv("TYPESAFE_API_KEY")) != "" {
		return
	}
	st := mgr.Status(0)
	if len(st.Installed) == 0 && os.Getenv("MEMORY_AUTO_BOOTSTRAP") != "1" {
		return
	}
	_, _, _ = mgr.AutoBootstrap(ctx, 0, func(ep, model string) {
		_ = mem.SaveConfig(context.Background(), map[string]string{
			"active_provider": "gguf",
			"gguf_endpoint":   ep,
			"gguf_model":      model,
		})
	})
}

func buildMCPServer(version string, b *runtimeBundle) *server.MCPServer {
	return transportmcp.NewServer(transportmcp.Deps{
		Memory:    b.mem,
		Progress:  b.prog,
		Guardrail: b.gr,
		Version:   version,
	})
}

func filterDaemonEnv(base []string, dbPath, modelsDir, explicitAddr string) []string {
	skip := map[string]bool{
		"MEMORY_DB_PATH":        true,
		"MEMORY_MODELS_DIR":     true,
		"MEMORY_DAEMON_ADDR":    true,
		"MEMORY_DASHBOARD_ADDR": true,
	}
	out := make([]string, 0, len(base)+4)
	for _, entry := range base {
		k := entry
		if idx := strings.IndexByte(entry, '='); idx >= 0 {
			k = entry[:idx]
		}
		if skip[k] {
			continue
		}
		out = append(out, entry)
	}
	out = append(out,
		"MEMORY_DB_PATH="+dbPath,
		"MEMORY_MODELS_DIR="+modelsDir,
	)
	if strings.TrimSpace(explicitAddr) != "" {
		out = append(out, "MEMORY_DAEMON_ADDR="+strings.TrimSpace(explicitAddr))
	}
	return out
}

func listenDaemonWithFallback(targetAddr string, excludePorts ...int) (net.Listener, error) {
	ln, err := ListenWithHandover(targetAddr, false)
	if err == nil {
		return ln, nil
	}

	host, port, ok := domain.ParseHostPort(targetAddr)
	if !ok {
		return nil, err
	}
	excluded := make(map[int]bool, len(excludePorts))
	for _, ex := range excludePorts {
		if ex > 0 {
			excluded[ex] = true
		}
	}

	if port > 0 {
		for offset := 1; offset <= 32; offset++ {
			cand := port + offset
			if cand > 65535 {
				cand = 1024 + (cand - 65535)
			}
			if excluded[cand] {
				continue
			}
			candAddr := net.JoinHostPort(host, strconv.Itoa(cand))
			if fbLn, ferr := net.Listen("tcp", candAddr); ferr == nil {
				log.Printf("[memorize daemon] port %s busy (%v); auto-fallback bound to %s", targetAddr, err, fbLn.Addr().String())
				return fbLn, nil
			}
		}
	}

	for attempt := 0; attempt < 8; attempt++ {
		fbLn, ferr := net.Listen("tcp", net.JoinHostPort(host, "0"))
		if ferr != nil {
			continue
		}
		if tcpAddr, ok := fbLn.Addr().(*net.TCPAddr); ok && excluded[tcpAddr.Port] {
			_ = fbLn.Close()
			continue
		}
		log.Printf("[memorize daemon] port %s busy (%v); auto-fallback bound to %s", targetAddr, err, fbLn.Addr().String())
		return fbLn, nil
	}
	return nil, err
}

func pickNextDaemonAddrForRestart(host string, desiredPort int, excludePorts ...int) (string, int, bool, error) {
	if strings.TrimSpace(host) == "" {
		host = domain.DefaultDaemonHost
	}
	desiredAddr := net.JoinHostPort(host, strconv.Itoa(desiredPort))
	if ln, err := net.Listen("tcp", desiredAddr); err == nil {
		_ = ln.Close()
		return desiredAddr, desiredPort, false, nil
	}
	if thttp.ProbeDashboardMetaAlive(desiredAddr) {
		return desiredAddr, desiredPort, false, nil
	}
	p, fb, err := config.PickAvailableLoopbackPort(host, desiredPort, excludePorts...)
	if err != nil {
		return "", 0, false, err
	}
	return net.JoinHostPort(host, strconv.Itoa(p)), p, fb, nil
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

// EnsureRunning returns the live daemon status matching dbPath, spawning it in the background if needed.
func EnsureRunning(addr, dbPath, modelsDir string) (*thttp.DaemonStatus, bool) {
	if !config.IsPersistentDBPath(dbPath) {
		return nil, false
	}
	if st, _, ok := thttp.DiscoverLiveDaemonStatus(addr, dbPath); ok {
		return st, true
	}

	explicitOv := thttp.ExplicitDaemonOverride(addr)

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
		if bestBin, _ := NewestWatchedBinary(0); bestBin != "" {
			exe = bestBin
		}
		logFile := openDaemonLogFile(config.DaemonLogPath(dbPath))
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
		if st, _, ok := thttp.DiscoverLiveDaemonStatus(addr, dbPath); ok {
			return st, true
		}
	}
	return nil, false
}

// RunDaemonWithBundle serves the unified daemon over an already initialized runtime bundle.
func RunDaemonWithBundle(version, addr, dbPath, modelsDir string, b *runtimeBundle) int {
	targetAddr, _ := thttp.ResolveDaemonTargetAddr(addr, dbPath)
	portCfg := config.LoadPortConfig(dbPath)
	ln, err := listenDaemonWithFallback(targetAddr, portCfg.GGUFPort)
	if err != nil {
		log.Printf("memorize daemon listen error on %s: %v", targetAddr, err)
		return 1
	}
	boundAddr := ln.Addr().String()
	boundHost, boundPort, _ := domain.ParseHostPort(boundAddr)
	b.mgr.SetExcludePortFunc(func() int { return boundPort })
	b.mgr.SetDashboardURLFunc(func() string { return "http://" + boundAddr })
	_ = config.WriteDaemonState(dbPath, domain.DaemonRuntimeState{
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
			config.RemoveDaemonStateIfPID(dbPath, os.Getpid())
		}
	}()

	startedAt := time.Now()
	logPath := config.DaemonLogPath(dbPath)
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

	buildStatus := func() thttp.DaemonStatus {
		_, binT := NewestWatchedBinary(0)
		cfg := config.LoadPortConfig(dbPath)
		lm := b.mgr.Status(cfg.GGUFPort)
		return thttp.DaemonStatus{
			Running:              true,
			PID:                  os.Getpid(),
			Version:              version,
			StartedAt:            startedAt.Format(time.RFC3339),
			UptimeSec:            int64(time.Since(startedAt).Seconds()),
			ListenAddr:           boundAddr,
			DBPath:               absDB,
			ModelsDir:            absModels,
			Provider:             b.mem.ProviderName(),
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

	applyPorts := func(ctx context.Context, reqDaemonPort, reqGGUFPort int) (thttp.PortApplyResponse, error) {
		portMu.Lock()
		defer portMu.Unlock()

		curCfg := config.LoadPortConfig(dbPath)
		dp := reqDaemonPort
		if dp <= 0 {
			dp = curCfg.DaemonPort
		}
		gp := reqGGUFPort
		if gp <= 0 {
			gp = curCfg.GGUFPort
		}
		if err := domain.ValidatePortConfig(dp, gp); err != nil {
			return thttp.PortApplyResponse{}, err
		}
		if err := config.SavePortConfig(dbPath, domain.PortConfig{DaemonPort: dp, GGUFPort: gp}); err != nil {
			return thttp.PortApplyResponse{}, err
		}

		lm := b.mgr.Status(gp)
		effGGUFPort := gp
		effGGUFEP := fmt.Sprintf("http://127.0.0.1:%d", gp)
		ggufFallback := false
		ggufRunning := lm.ServerRunning
		if lm.ServerRunning {
			if lm.Port > 0 && lm.Port != gp && lm.Supported && lm.ActiveModel != "" {
				bgCtx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
				ep, err := b.mgr.StartServer(bgCtx, lm.ActiveModel, gp)
				cancel()
				if err != nil {
					return thttp.PortApplyResponse{}, err
				}
				if ep != "" {
					effGGUFEP = ep
					if _, p, okp := domain.ParseHostPort(ep); okp && p > 0 {
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
		_ = config.SyncManagedGGUFEndpoint(context.Background(), b.mem.DB(), dbPath, effGGUFEP)

		res := thttp.PortApplyResponse{
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
			_ = config.WriteDaemonState(dbPath, domain.DaemonRuntimeState{
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
			return thttp.PortApplyResponse{}, perr
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

	dashHandler := dashboard.NewHandler(dashboard.Deps{
		Memory:     b.mem,
		Progress:   b.prog,
		Guardrail:  b.gr,
		LocalModel: b.mgr,
		Hooks: thttp.DashboardHooks{
			YieldFn:    closeFn,
			BinMtimeFn: currentBinaryModTime,
			DaemonStatusFn: func() thttp.DaemonPortState {
				st := buildStatus()
				return thttp.DaemonPortState{
					PID:            st.PID,
					ListenAddr:     st.ListenAddr,
					UptimeSec:      st.UptimeSec,
					ConfiguredPort: st.ConfiguredDaemonPort,
					ActivePort:     st.ActiveDaemonPort,
					Fallback:       st.DaemonFallback,
				}
			},
			ApplyDaemonPortFn: func(ctx context.Context, dp, gp int) (thttp.DaemonPortApplyResult, error) {
				res, err := applyPorts(ctx, dp, gp)
				if err != nil {
					return thttp.DaemonPortApplyResult{}, err
				}
				return thttp.DaemonPortApplyResult{
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
		},
	})

	mcpSrv := buildMCPServer(version, b)
	rootHandler := thttp.NewDaemonMux(thttp.DaemonDeps{
		Guardrail:     b.gr,
		MCPServer:     mcpSrv,
		Dashboard:     dashHandler,
		BuildStatusFn: buildStatus,
		ApplyPortsFn:  applyPorts,
		RestartFn: func() (string, error) {
			nextBin, _ := NewestWatchedBinary(0)
			if nextBin == "" {
				nextBin, _ = os.Executable()
			}
			go func() {
				time.Sleep(35 * time.Millisecond)
				skipStateRemove.Store(true)
				select {
				case reexecCh <- reexecSpec{bin: nextBin, addr: boundAddr}:
				default:
				}
				_ = closeFn()
			}()
			return nextBin, nil
		},
		StopFn: func() {
			go func() {
				time.Sleep(35 * time.Millisecond)
				select {
				case stopRequested <- struct{}{}:
				default:
				}
				_ = b.mgr.StopServer()
				_ = closeFn()
			}()
		},
	})

	_, startMtime := NewestWatchedBinary(0)
	srv = &http.Server{
		Handler:           rootHandler,
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
					if path, t1 := NewestWatchedBinary(startMtime); path != "" {
						time.Sleep(150 * time.Millisecond)
						if path2, t2 := NewestWatchedBinary(startMtime); path2 == path && t2 == t1 {
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
		version, boundAddr, os.Getpid(), absDB, b.mem.ProviderName())

	if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Printf("memorize daemon serve error: %v", err)
		return 1
	}

	select {
	case <-stopRequested:
		log.Printf("memorize daemon (pid=%d) stopped cleanly.", os.Getpid())
		return 0
	case spec := <-reexecCh:
		log.Printf("memorize daemon restarting into %s (%s)...", spec.bin, spec.addr)
		b.close()
		reexecArgs := []string{spec.bin, "-daemon", "-addr", spec.addr}
		if err := execInPlace(spec.bin, reexecArgs, filterDaemonEnv(os.Environ(), absDB, absModels, "")); err != nil {
			log.Printf("memorize daemon re-exec error: %v", err)
			return 1
		}
	default:
	}
	return 0
}

// RunServer initializes runtime dependencies and runs the unified daemon until stop or re-exec.
func RunServer(version, explicitAddr, dbPath, modelsDir string) int {
	b, err := openRuntime(dbPath, modelsDir, true)
	if err != nil {
		log.Printf("failed to initialize memory service: %v", err)
		return 1
	}
	defer b.close()
	return RunDaemonWithBundle(version, explicitAddr, dbPath, modelsDir, b)
}

// RunInlineHook executes a lifecycle hook directly against SQLite when daemon delegation is disabled or unavailable.
func RunInlineHook(ctx context.Context, dbPath, modelsDir, mode string, rawStdin []byte) (string, int) {
	b, err := openRuntime(dbPath, modelsDir, false)
	if err != nil {
		log.Printf("failed to initialize memory service: %v", err)
		return "", 1
	}
	defer b.close()
	out, code, _ := b.gr.ExecuteHook(ctx, mode, rawStdin, "")
	return out, code
}

// ServeInlineStdio serves MCP over stdin/stdout directly (with optional background dashboard) without daemon RPC.
func ServeInlineStdio(version, dbPath, modelsDir string, withDashboard bool, dashAddr string) int {
	b, err := openRuntime(dbPath, modelsDir, true)
	if err != nil {
		log.Printf("failed to initialize memory service: %v", err)
		return 1
	}
	defer b.close()

	if withDashboard {
		_, closeDash, _ := StartBackgroundServer(dashAddr, dashboard.Deps{
			Memory:     b.mem,
			Progress:   b.prog,
			Guardrail:  b.gr,
			LocalModel: b.mgr,
		}, dbPath)
		if closeDash != nil {
			defer closeDash()
		}
	}

	if err := transportmcp.ServeStdio(buildMCPServer(version, b)); err != nil {
		log.Printf("stdio server error: %v", err)
		return 1
	}
	return 0
}

// Hooks returns the cli.RuntimeHooks implementation backed by localdaemon.
func Hooks(version string) cli.RuntimeHooks {
	return cli.RuntimeHooks{
		EnsureDaemonRunning: EnsureRunning,
		RunDaemonServer: func(explicitAddr, dbPath, modelsDir string) int {
			return RunServer(version, explicitAddr, dbPath, modelsDir)
		},
		ServeInlineStdio: func(dbPath, modelsDir string, withDashboard bool, dashAddr string) int {
			return ServeInlineStdio(version, dbPath, modelsDir, withDashboard, dashAddr)
		},
		RunInlineHook: RunInlineHook,
	}
}
