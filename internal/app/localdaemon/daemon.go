package localdaemon

import (
	"context"
	"fmt"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/mark3labs/mcp-go/server"

	"praxis/internal/domain"
	"praxis/internal/inference/localmodel"
	"praxis/internal/inference/router"
	"praxis/internal/infra/config"
	"praxis/internal/infra/sqlite"
	"praxis/internal/modules/guardrail"
	"praxis/internal/modules/memory"
	"praxis/internal/modules/progress"
	"praxis/internal/modules/rulepack"
	"praxis/internal/transport/cli"
	"praxis/internal/transport/dashboard"
	thttp "praxis/internal/transport/http"
	transportmcp "praxis/internal/transport/mcp"
)

type runtimeBundle struct {
	mgr  *localmodel.Manager
	mem  *memory.Service
	prog *progress.Service
	gr   *guardrail.Service
	rp   *rulepack.Service
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
	mem.SetAutoGitSync(os.Getenv("PRAXIS_AUTO_GIT_SYNC") == "1")
	mem.Router().AttachLocalManager(mgr)
	prog := progress.New(mem)
	rp := rulepack.New(mem)
	mem.AttachExtraFactsProvider(rp)
	gr := guardrail.New(mem, prog)
	gr.AttachRulePack(rp)
	if autoBootstrap {
		maybeAutoBootstrap(context.Background(), mem, mgr)
	}
	return &runtimeBundle{mgr: mgr, mem: mem, prog: prog, gr: gr, rp: rp}, nil
}

func maybeAutoBootstrap(ctx context.Context, mem *memory.Service, mgr *localmodel.Manager) {
	if !localmodel.IsSupportedPlatform() {
		return
	}
	cfg := mem.GetConfigMap(ctx)
	if strings.TrimSpace(cfg[config.CfgActiveProvider]) != "" ||
		strings.TrimSpace(os.Getenv("PRAXIS_PROVIDER")) != "" ||
		strings.TrimSpace(os.Getenv("GEMINI_API_KEY")) != "" ||
		strings.TrimSpace(os.Getenv("TYPESAFE_API_KEY")) != "" {
		return
	}
	st := mgr.Status(0)
	if len(st.Installed) == 0 && os.Getenv("PRAXIS_AUTO_BOOTSTRAP") != "1" {
		return
	}
	_, _, _ = mgr.AutoBootstrap(ctx, 0, func(ep, model string) {
		_ = mem.SaveConfig(context.Background(), map[string]string{
			config.CfgActiveProvider: "gguf",
			config.CfgGGUFEndpoint:   ep,
			config.CfgGGUFModel:      model,
		})
	})
}

func buildMCPServer(version string, b *runtimeBundle) *server.MCPServer {
	return transportmcp.NewServer(transportmcp.Deps{
		Memory:    b.mem,
		Progress:  b.prog,
		Guardrail: b.gr,
		RulePack:  b.rp,
		Version:   version,
	})
}

func filterDaemonEnv(base []string, dbPath, modelsDir, explicitAddr string) []string {
	skip := map[string]bool{
		"PRAXIS_DB_PATH":        true,
		"PRAXIS_MODELS_DIR":     true,
		"PRAXIS_DAEMON_ADDR":    true,
		"PRAXIS_DASHBOARD_ADDR": true,
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
		"PRAXIS_DB_PATH="+dbPath,
		"PRAXIS_MODELS_DIR="+modelsDir,
	)
	if strings.TrimSpace(explicitAddr) != "" {
		out = append(out, "PRAXIS_DAEMON_ADDR="+strings.TrimSpace(explicitAddr))
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
				log.Printf("[praxis daemon] port %s busy (%v); auto-fallback bound to %s", targetAddr, err, fbLn.Addr().String())
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
		log.Printf("[praxis daemon] port %s busy (%v); auto-fallback bound to %s", targetAddr, err, fbLn.Addr().String())
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

func EnsureRunning(addr, dbPath, modelsDir string) (*thttp.DaemonStatus, bool) {
	if !config.IsPersistentDBPath(dbPath) {
		return nil, false
	}
	if st, _, ok, authReq := thttp.ProbeLiveDaemonState(addr, dbPath); ok {
		return st, true
	} else if authReq {
		log.Printf("praxis team server already running with authentication on %s; skipping unauthenticated daemon spawn", st.ListenAddr)
		return st, false
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
		if st, _, ok, authReq := thttp.ProbeLiveDaemonState(addr, dbPath); ok {
			return st, true
		} else if authReq {
			return st, false
		}
	}
	return nil, false
}

func RunDaemonWithBundle(version, addr, dbPath, modelsDir string, b *runtimeBundle) int {
	targetAddr, _ := thttp.ResolveDaemonTargetAddr(addr, dbPath)
	portCfg := config.LoadPortConfig(dbPath)
	ln, err := listenDaemonWithFallback(targetAddr, portCfg.GGUFPort)
	if err != nil {
		log.Printf("praxis daemon listen error on %s: %v", targetAddr, err)
		return 1
	}

	lc := newDaemonLifecycle(version, dbPath, modelsDir, ln.Addr().String(), b)
	b.mgr.SetExcludePortFunc(func() int { return lc.boundPort })
	b.mgr.SetDashboardURLFunc(func() string { return "http://" + lc.boundAddr })
	lc.writeRuntimeState(portCfg.GGUFPort, "")
	defer lc.cleanupState()

	return lc.serve(ln)
}

func RunServer(version, explicitAddr, dbPath, modelsDir string) int {
	b, err := openRuntime(dbPath, modelsDir, true)
	if err != nil {
		log.Printf("failed to initialize memory service: %v", err)
		return 1
	}
	defer b.close()
	return RunDaemonWithBundle(version, explicitAddr, dbPath, modelsDir, b)
}

func RunTeamServer(version, explicitAddr, dbPath, modelsDir string) int {
	b, err := openRuntime(dbPath, modelsDir, true)
	if err != nil {
		log.Printf("failed to initialize memory service: %v", err)
		return 1
	}
	defer b.close()

	listenAddr := strings.TrimSpace(explicitAddr)
	if listenAddr == "" {
		listenAddr = domain.DefaultServeAddr
	}
	ln, err := net.Listen("tcp", listenAddr)
	if err != nil {
		log.Printf("praxis team server listen error on %s: %v", listenAddr, err)
		return 1
	}

	bootstrapAccount := domain.FirstNonEmpty(os.Getenv("PRAXIS_BOOTSTRAP_ACCOUNT"), b.mem.CurrentAccount(context.Background()))
	issued, created, err := sqlite.EnsureBootstrapAdmin(
		context.Background(),
		b.mem.DB(),
		bootstrapAccount,
		os.Getenv("PRAXIS_BOOTSTRAP_TOKEN"),
	)
	if err != nil {
		_ = ln.Close()
		log.Printf("bootstrap admin initialization failed: %v", err)
		return 1
	}

	lc := newDaemonLifecycle(version, dbPath, modelsDir, ln.Addr().String(), b)
	lc.serverMode = true
	b.mgr.SetExcludePortFunc(func() int { return lc.boundPort })
	b.mgr.SetDashboardURLFunc(func() string { return "http://" + lc.boundAddr })

	localDiscoverAddr := lc.boundAddr
	if h, p, ok := domain.ParseHostPort(lc.boundAddr); ok && p > 0 && (h == "" || h == "0.0.0.0" || h == "::") {
		localDiscoverAddr = net.JoinHostPort(domain.DefaultDaemonHost, strconv.Itoa(p))
	}
	_ = config.WriteDaemonState(dbPath, domain.DaemonRuntimeState{
		PID:        os.Getpid(),
		ListenAddr: localDiscoverAddr,
		ServerMode: true,
		DBPath:     dbPath,
		ModelsDir:  modelsDir,
		StartedAt:  lc.startedAt.Format(time.RFC3339),
	})
	defer lc.cleanupState()

	if created {
		hintHost := lc.boundAddr
		if h, p, ok := domain.ParseHostPort(lc.boundAddr); ok && (h == "" || h == "0.0.0.0" || h == "::") {
			hintHost = net.JoinHostPort("<server-host>", strconv.Itoa(p))
		}
		fmt.Fprintf(os.Stderr, "\n====================================================================\n")
		fmt.Fprintf(os.Stderr, "[praxis team server] Initial admin account created: @%s\n", issued.User.Account)
		fmt.Fprintf(os.Stderr, "Access Token (shown ONCE — save now): %s\n", issued.PlainToken)
		fmt.Fprintf(os.Stderr, "Web Console:  http://%s/login\n", hintHost)
		fmt.Fprintf(os.Stderr, "Client Bind:  praxis connect --url http://%s --token %s\n", hintHost, issued.PlainToken)
		fmt.Fprintf(os.Stderr, "Add Users:    praxis user add <account> --role member\n")
		fmt.Fprintf(os.Stderr, "====================================================================\n\n")
	}

	return lc.serve(ln)
}

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

func Hooks(version string) cli.RuntimeHooks {
	return cli.RuntimeHooks{
		EnsureDaemonRunning: EnsureRunning,
		RunDaemonServer: func(explicitAddr, dbPath, modelsDir string) int {
			return RunServer(version, explicitAddr, dbPath, modelsDir)
		},
		RunTeamServer: func(explicitAddr, dbPath, modelsDir string) int {
			return RunTeamServer(version, explicitAddr, dbPath, modelsDir)
		},
		ServeInlineStdio: func(dbPath, modelsDir string, withDashboard bool, dashAddr string) int {
			return ServeInlineStdio(version, dbPath, modelsDir, withDashboard, dashAddr)
		},
		RunInlineHook: RunInlineHook,
	}
}
