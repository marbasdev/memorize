package localdaemon

import (
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"memorize/internal/domain"
	"memorize/internal/infra/config"
	"memorize/internal/transport/dashboard"
	thttp "memorize/internal/transport/http"
)

func isDevHotReloadEnabled() bool {
	return strings.TrimSpace(os.Getenv("MEMORIZE_DEV_HOTRELOAD")) == "1"
}

// WatchedBinaryCandidates returns executable binary paths checked for hot-reload and daemon spawn.
func WatchedBinaryCandidates() []string {
	seen := make(map[string]bool)
	var out []string
	add := func(p string) {
		p = strings.TrimSpace(p)
		if p == "" {
			return
		}
		if abs, err := filepath.Abs(p); err == nil {
			p = abs
		}
		if !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
		if resolved, err := filepath.EvalSymlinks(p); err == nil && !seen[resolved] {
			seen[resolved] = true
			out = append(out, resolved)
		}
	}
	if envBin := strings.TrimSpace(os.Getenv("MEMORY_DAEMON_BIN")); envBin != "" {
		add(envBin)
		return out
	}
	if exe, err := os.Executable(); err == nil {
		add(exe)
	}
	binName := "memorize"
	if runtime.GOOS == "windows" {
		binName = "memorize.exe"
	}
	if dir := config.DefaultDataDir(); dir != "" {
		add(filepath.Join(dir, "bin", binName))
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		add(filepath.Join(home, ".memorize", "bin", binName))
	}
	if isDevHotReloadEnabled() {
		if wd, err := os.Getwd(); err == nil {
			add(filepath.Join(wd, "bin", binName))
		}
		if _, callerFile, _, ok := runtime.Caller(0); ok && filepath.IsAbs(callerFile) {
			// internal/app/localdaemon/handover.go -> repo root is 3 levels up
			repoRoot := filepath.Clean(filepath.Join(filepath.Dir(callerFile), "..", "..", ".."))
			add(filepath.Join(repoRoot, "bin", binName))
		}
	}
	return out
}

func isExecutableBinary(path string, info os.FileInfo) bool {
	if info == nil || info.IsDir() || info.Size() < 1024 {
		return false
	}
	if runtime.GOOS == "windows" {
		return strings.EqualFold(filepath.Ext(path), ".exe")
	}
	return (info.Mode() & 0o111) != 0
}

// NewestWatchedBinary returns the newest executable candidate strictly newer than afterMtime.
func NewestWatchedBinary(afterMtime int64) (string, int64) {
	var bestPath string
	bestTime := afterMtime
	for _, p := range WatchedBinaryCandidates() {
		info, err := os.Stat(p)
		if err != nil || !isExecutableBinary(p, info) {
			continue
		}
		if t := info.ModTime().UnixNano(); t > bestTime {
			bestTime = t
			bestPath = p
		}
	}
	return bestPath, bestTime
}

func currentBinaryModTime() int64 {
	_, t := NewestWatchedBinary(0)
	return t
}

// ListenWithHandover binds addr, asking an existing listener on the same port to yield when appropriate.
func ListenWithHandover(addr string, onlyIfNewer bool) (net.Listener, error) {
	a := strings.TrimSpace(addr)
	if a == "" {
		a = domain.DefaultDaemonAddr
	}
	ln, err := net.Listen("tcp", a)
	if err == nil {
		return ln, nil
	}
	if onlyIfNewer {
		myMtime := currentBinaryModTime()
		remoteMtime, ok := thttp.RemoteDashboardBinModTime(a)
		// If remote responds and is already at least as new as us, do not take over.
		if ok && remoteMtime >= myMtime && myMtime > 0 {
			return nil, err
		}
	}
	if !thttp.RequestDashboardYield(a) {
		return nil, err
	}
	for i := 0; i < 15; i++ {
		time.Sleep(40 * time.Millisecond)
		if ln, err = net.Listen("tcp", a); err == nil {
			return ln, nil
		}
	}
	return nil, err
}

// StartBackgroundServer starts the dashboard HTTP server in a non-blocking goroutine alongside inline MCP stdio.
func StartBackgroundServer(addr string, deps dashboard.Deps, dbPath string) (string, func() error, error) {
	a := strings.TrimSpace(addr)
	if a == "" {
		port := config.LoadPortConfig(dbPath).DaemonPort
		a = fmt.Sprintf("%s:%d", domain.DefaultDaemonHost, port)
	}
	ln, err := ListenWithHandover(a, true)
	if err != nil {
		return "", nil, nil
	}
	var srv *http.Server
	var closeOnce sync.Once
	stopWatch := make(chan struct{})
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
	deps.Hooks.YieldFn = closeFn
	if deps.Hooks.BinMtimeFn == nil {
		deps.Hooks.BinMtimeFn = currentBinaryModTime
	}
	srv = &http.Server{
		Handler:           dashboard.NewHandler(deps),
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    64 << 10,
	}
	boundAddr := ln.Addr().String()
	_, startMtime := NewestWatchedBinary(0)

	go func() {
		_ = srv.Serve(ln)
	}()

	// If bound to a fixed port and a newer binary appears, gracefully yield and spawn the updated binary.
	if !strings.HasSuffix(a, ":0") && runtime.GOOS != "windows" {
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
							_ = closeFn()
							cmd := exec.Command(path, "-dashboard", boundAddr)
							cmd.Env = os.Environ()
							if err := cmd.Start(); err != nil {
								log.Printf("memorize dashboard handover restart failed: %v", err)
							}
							return
						}
					}
				}
			}
		}()
	}

	return boundAddr, closeFn, nil
}
