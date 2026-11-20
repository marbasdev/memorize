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

	"praxis/internal/domain"
	"praxis/internal/infra/config"
	"praxis/internal/transport/dashboard"
	thttp "praxis/internal/transport/http"
)

func isDevHotReloadEnabled() bool {
	return strings.TrimSpace(os.Getenv("PRAXIS_DEV_HOTRELOAD")) == "1"
}

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
	if envBin := strings.TrimSpace(os.Getenv("PRAXIS_DAEMON_BIN")); envBin != "" {
		add(envBin)
		return out
	}
	if exe, err := os.Executable(); err == nil {
		add(exe)
	}
	binName := "praxis"
	if runtime.GOOS == "windows" {
		binName = "praxis.exe"
	}
	if dir := config.DefaultDataDir(); dir != "" {
		add(filepath.Join(dir, "bin", binName))
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		add(filepath.Join(home, ".praxis", "bin", binName))
	}
	if isDevHotReloadEnabled() {
		if wd, err := os.Getwd(); err == nil {
			add(filepath.Join(wd, "bin", binName))
		}
		if _, callerFile, _, ok := runtime.Caller(0); ok && filepath.IsAbs(callerFile) {
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
								log.Printf("praxis dashboard handover restart failed: %v", err)
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
