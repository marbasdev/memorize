package localdaemon

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"praxis/internal/domain"
	"praxis/internal/infra/config"
	"praxis/internal/transport/dashboard"
	thttp "praxis/internal/transport/http"
)

type reexecSpec struct {
	bin  string
	addr string
}

type daemonLifecycle struct {
	version   string
	bundle    *runtimeBundle
	dbPath    string
	modelsDir string
	absDB     string
	absModels string
	logPath   string

	boundAddr  string
	boundHost  string
	boundPort  int
	serverMode bool
	startedAt  time.Time

	srv             *http.Server
	closeOnce       sync.Once
	portMu          sync.Mutex
	skipStateRemove atomic.Bool
	stopWatch       chan struct{}
	reexecCh        chan reexecSpec
	stopRequested   chan struct{}
}

func newDaemonLifecycle(version, dbPath, modelsDir, boundAddr string, b *runtimeBundle) *daemonLifecycle {
	boundHost, boundPort, _ := domain.ParseHostPort(boundAddr)
	return &daemonLifecycle{
		version:   version,
		bundle:    b,
		dbPath:    dbPath,
		modelsDir: modelsDir,
		absDB:     absPathOrFallback(dbPath),
		absModels: absPathOrFallback(modelsDir),
		logPath:   config.DaemonLogPath(dbPath),
		boundAddr: boundAddr,
		boundHost: boundHost,
		boundPort: boundPort,
		startedAt: time.Now(),
		srv: &http.Server{
			ReadHeaderTimeout: 5 * time.Second,
			IdleTimeout:       60 * time.Second,
			MaxHeaderBytes:    64 << 10,
		},
		stopWatch:     make(chan struct{}),
		reexecCh:      make(chan reexecSpec, 1),
		stopRequested: make(chan struct{}, 1),
	}
}

func absPathOrFallback(p string) string {
	if abs, err := filepath.Abs(p); err == nil && abs != "" {
		return abs
	}
	return p
}

func (d *daemonLifecycle) cleanupState() {
	if !d.skipStateRemove.Load() {
		config.RemoveDaemonStateIfPID(d.dbPath, os.Getpid())
	}
}

func (d *daemonLifecycle) close() error {
	var cerr error
	d.closeOnce.Do(func() {
		close(d.stopWatch)
		if d.bundle != nil && d.bundle.mem != nil {
			d.bundle.mem.Bus().Close()
		}
		if d.srv != nil {
			shutCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			if err := d.srv.Shutdown(shutCtx); err != nil {
				cerr = d.srv.Close()
			}
		}
	})
	return cerr
}

func (d *daemonLifecycle) writeRuntimeState(ggufPort int, ggufURL string) {
	_ = config.WriteDaemonState(d.dbPath, domain.DaemonRuntimeState{
		PID:        os.Getpid(),
		ListenAddr: d.boundAddr,
		DBPath:     d.dbPath,
		ModelsDir:  d.modelsDir,
		GGUFPort:   ggufPort,
		GGUFURL:    ggufURL,
		StartedAt:  d.startedAt.UTC().Format(time.RFC3339),
	})
}

func (d *daemonLifecycle) buildStatus() thttp.DaemonStatus {
	_, binT := NewestWatchedBinary(0)
	cfg := config.LoadPortConfig(d.dbPath)
	lm := d.bundle.mgr.Status(cfg.GGUFPort)
	return thttp.DaemonStatus{
		Running:              true,
		PID:                  os.Getpid(),
		Version:              d.version,
		StartedAt:            d.startedAt.Format(time.RFC3339),
		UptimeSec:            int64(time.Since(d.startedAt).Seconds()),
		ListenAddr:           d.boundAddr,
		DBPath:               d.absDB,
		ModelsDir:            d.absModels,
		Provider:             d.bundle.mem.ProviderName(),
		LogPath:              d.logPath,
		BinMtime:             binT,
		ConfiguredDaemonPort: cfg.DaemonPort,
		ActiveDaemonPort:     d.boundPort,
		DaemonFallback:       d.boundPort != cfg.DaemonPort,
		ConfiguredGGUFPort:   cfg.GGUFPort,
		ActiveGGUFPort:       lm.Port,
		GGUFEndpoint:         lm.Endpoint,
		GGUFFallback:         lm.PortFallback,
		GGUFRunning:          lm.ServerRunning,
	}
}

func (d *daemonLifecycle) reconcileGGUFPort(gp int) (effPort int, effEP string, fallback, running bool, err error) {
	lm := d.bundle.mgr.Status(gp)
	effPort = gp
	effEP = fmt.Sprintf("http://127.0.0.1:%d", gp)
	running = lm.ServerRunning
	if !lm.ServerRunning {
		return effPort, effEP, false, false, nil
	}
	if lm.Port > 0 && lm.Port != gp && lm.Supported && lm.ActiveModel != "" {
		bgCtx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
		ep, startErr := d.bundle.mgr.StartServer(bgCtx, lm.ActiveModel, gp)
		cancel()
		if startErr != nil {
			return 0, "", false, false, startErr
		}
		if ep != "" {
			effEP = ep
			if _, p, okp := domain.ParseHostPort(ep); okp && p > 0 {
				effPort = p
				fallback = p != gp
			}
		}
		return effPort, effEP, fallback, running, nil
	}
	if lm.Port > 0 {
		effPort = lm.Port
		effEP = lm.Endpoint
		fallback = lm.PortFallback
	}
	return effPort, effEP, fallback, running, nil
}

func (d *daemonLifecycle) applyPorts(_ context.Context, reqDaemonPort, reqGGUFPort int) (thttp.PortApplyResponse, error) {
	d.portMu.Lock()
	defer d.portMu.Unlock()

	curCfg := config.LoadPortConfig(d.dbPath)
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
	if err := config.SavePortConfig(d.dbPath, domain.PortConfig{DaemonPort: dp, GGUFPort: gp}); err != nil {
		return thttp.PortApplyResponse{}, err
	}

	effGGUFPort, effGGUFEP, ggufFallback, ggufRunning, err := d.reconcileGGUFPort(gp)
	if err != nil {
		return thttp.PortApplyResponse{}, err
	}
	_ = config.SyncManagedGGUFEndpoint(context.Background(), d.bundle.mem.DB(), d.dbPath, effGGUFEP)

	res := thttp.PortApplyResponse{
		OK:               true,
		ConfiguredDaemon: dp,
		ActiveDaemon:     d.boundPort,
		ListenAddr:       d.boundAddr,
		DaemonFallback:   d.boundPort != dp,
		Restarting:       false,
		NewURL:           "http://" + d.boundAddr,
		ConfiguredGGUF:   gp,
		ActiveGGUF:       effGGUFPort,
		GGUFEndpoint:     effGGUFEP,
		GGUFFallback:     ggufFallback,
		GGUFRunning:      ggufRunning,
	}

	if dp == d.boundPort {
		d.writeRuntimeState(effGGUFPort, effGGUFEP)
		return res, nil
	}

	nextAddr, nextPort, nextFB, perr := pickNextDaemonAddrForRestart(d.boundHost, dp, effGGUFPort)
	if perr != nil {
		return thttp.PortApplyResponse{}, perr
	}
	res.ActiveDaemon = nextPort
	res.ListenAddr = nextAddr
	res.DaemonFallback = nextFB
	res.Restarting = true
	res.NewURL = "http://" + nextAddr

	nextBin, _ := os.Executable()
	d.triggerReexecAsync(nextBin, nextAddr, 80*time.Millisecond)
	return res, nil
}

func (d *daemonLifecycle) triggerReexecNow(bin, addr string) {
	d.skipStateRemove.Store(true)
	select {
	case d.reexecCh <- reexecSpec{bin: bin, addr: addr}:
	default:
	}
	_ = d.close()
}

func (d *daemonLifecycle) triggerReexecAsync(bin, addr string, delay time.Duration) {
	go func() {
		if delay > 0 {
			time.Sleep(delay)
		}
		d.triggerReexecNow(bin, addr)
	}()
}

func (d *daemonLifecycle) dashboardHooks() thttp.DashboardHooks {
	hooks := thttp.DashboardHooks{
		BinMtimeFn: currentBinaryModTime,
		DaemonStatusFn: func() thttp.DaemonPortState {
			st := d.buildStatus()
			return thttp.DaemonPortState{
				PID:            st.PID,
				ListenAddr:     st.ListenAddr,
				UptimeSec:      st.UptimeSec,
				ConfiguredPort: st.ConfiguredDaemonPort,
				ActivePort:     st.ActiveDaemonPort,
				Fallback:       st.DaemonFallback,
			}
		},
	}
	if !d.serverMode {
		hooks.YieldFn = d.close
		hooks.ApplyDaemonPortFn = func(ctx context.Context, dp, gp int) (thttp.DaemonPortApplyResult, error) {
			res, err := d.applyPorts(ctx, dp, gp)
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
		}
	}
	return hooks
}

func (d *daemonLifecycle) buildRootHandler() http.Handler {
	dashHandler := dashboard.NewHandler(dashboard.Deps{
		ServerMode: d.serverMode,
		DB:         d.bundle.mem.DB(),
		Memory:     d.bundle.mem,
		Progress:   d.bundle.prog,
		Guardrail:  d.bundle.gr,
		RulePack:   d.bundle.rp,
		LocalModel: d.bundle.mgr,
		Hooks:      d.dashboardHooks(),
	})
	deps := thttp.DaemonDeps{
		ServerMode:    d.serverMode,
		DB:            d.bundle.mem.DB(),
		Memory:        d.bundle.mem,
		Guardrail:     d.bundle.gr,
		MCPServer:     buildMCPServer(d.version, d.bundle),
		Dashboard:     dashHandler,
		BuildStatusFn: d.buildStatus,
		StopFn: func() {
			go func() {
				time.Sleep(35 * time.Millisecond)
				select {
				case d.stopRequested <- struct{}{}:
				default:
				}
				_ = d.bundle.mgr.StopServer()
				_ = d.close()
			}()
		},
	}
	if !d.serverMode {
		deps.ApplyPortsFn = d.applyPorts
		deps.RestartFn = func() (string, error) {
			nextBin, _ := NewestWatchedBinary(0)
			if nextBin == "" {
				nextBin, _ = os.Executable()
			}
			d.triggerReexecAsync(nextBin, d.boundAddr, 35*time.Millisecond)
			return nextBin, nil
		}
	}
	return thttp.NewDaemonMux(deps)
}

func (d *daemonLifecycle) watchBinaryReload(startMtime int64) {
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-d.stopWatch:
			return
		case <-ticker.C:
			if path, t1 := NewestWatchedBinary(startMtime); path != "" {
				time.Sleep(150 * time.Millisecond)
				if path2, t2 := NewestWatchedBinary(startMtime); path2 == path && t2 == t1 {
					d.triggerReexecNow(path, d.boundAddr)
					return
				}
			}
		}
	}
}

func (d *daemonLifecycle) serve(ln net.Listener) int {
	defer func() { _ = d.close() }()

	sigCtx, stopSig := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stopSig()
	go func() {
		select {
		case <-sigCtx.Done():
			select {
			case d.stopRequested <- struct{}{}:
			default:
			}
			if d.bundle != nil && d.bundle.mgr != nil {
				_ = d.bundle.mgr.StopServer()
			}
			_ = d.close()
		case <-d.stopWatch:
		}
	}()

	d.srv.Handler = d.buildRootHandler()
	_, startMtime := NewestWatchedBinary(0)
	if !d.serverMode && runtime.GOOS != "windows" {
		go d.watchBinaryReload(startMtime)
	}

	kind := "daemon"
	if d.serverMode {
		kind = "team server"
	}
	log.Printf("praxis %s v%s listening on http://%s (pid=%d, db=%s, provider=%s)",
		kind, d.version, d.boundAddr, os.Getpid(), d.absDB, d.bundle.mem.ProviderName())

	if err := d.srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Printf("praxis daemon serve error: %v", err)
		return 1
	}

	select {
	case <-d.stopRequested:
		log.Printf("praxis daemon (pid=%d) stopped cleanly.", os.Getpid())
		return 0
	case spec := <-d.reexecCh:
		log.Printf("praxis daemon restarting into %s (%s)...", spec.bin, spec.addr)
		d.bundle.close()
		reexecArgs := []string{spec.bin, "-daemon", "-addr", spec.addr}
		if err := execInPlace(spec.bin, reexecArgs, filterDaemonEnv(os.Environ(), d.absDB, d.absModels, "")); err != nil {
			log.Printf("praxis daemon re-exec error: %v", err)
			return 1
		}
	default:
	}
	return 0
}
