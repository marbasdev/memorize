package dashboard

import (
	"bytes"
	"encoding/json"
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

	"memorize/internal/memory"
	"memorize/internal/memory/localmodel"
)

const DefaultDashboardAddr = "127.0.0.1:18095"

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
	if dir := memory.DefaultDataDir(); dir != "" {
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
			// internal/memory/dashboard/handover.go -> repo root is 3 levels up
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

func remoteDashboardBinModTime(addr string) (int64, bool) {
	client := &http.Client{Timeout: 350 * time.Millisecond}
	resp, err := client.Get("http://" + addr + "/api/dashboard/meta")
	if err != nil {
		return 0, false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, false
	}
	var meta struct {
		BinMtime int64 `json:"bin_mtime"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&meta); err != nil {
		return 0, false
	}
	return meta.BinMtime, true
}

// RequestDashboardYield asks an existing local dashboard server on addr to gracefully close its HTTP listener.
func RequestDashboardYield(addr string) bool {
	client := &http.Client{Timeout: 500 * time.Millisecond}
	req, err := http.NewRequest(http.MethodPost, "http://"+addr+"/api/dashboard/yield", bytes.NewReader([]byte(`{}`)))
	if err != nil {
		return false
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return false
	}
	_ = resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

// ListenWithHandover binds addr, asking an older dashboard listener on the same port to yield if onlyIfNewer is false (always takeover) or when our binary is newer.
func ListenWithHandover(addr string, onlyIfNewer bool) (net.Listener, error) {
	a := strings.TrimSpace(addr)
	if a == "" {
		a = DefaultDashboardAddr
	}
	ln, err := net.Listen("tcp", a)
	if err == nil {
		return ln, nil
	}
	if onlyIfNewer {
		myMtime := currentBinaryModTime()
		remoteMtime, ok := remoteDashboardBinModTime(a)
		// If remote responds and is already at least as new as us, do not take over.
		if ok && remoteMtime >= myMtime && myMtime > 0 {
			return nil, err
		}
	}
	if !RequestDashboardYield(a) {
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

// StartBackgroundServer starts the Control & Calibration Dashboard HTTP server in a
// non-blocking background goroutine alongside the MCP stdio server. If the port is
// occupied by an older binary, it requests a graceful port handover; if occupied by
// a same/newer binary, it returns ("", nil, nil) gracefully.
func StartBackgroundServer(addr string, svc *memory.Service, mgr *localmodel.Manager) (string, func() error, error) {
	a := strings.TrimSpace(addr)
	if a == "" {
		port := memory.DefaultDaemonPort
		if svc != nil {
			port = memory.LoadPortConfig(svc.DBPath()).DaemonPort
		}
		a = fmt.Sprintf("%s:%d", memory.DefaultDaemonHost, port)
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
	srv = &http.Server{
		Handler:           NewHandlerWithYield(svc, closeFn, mgr),
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    64 << 10,
	}
	boundAddr := ln.Addr().String()
	_, startMtime := NewestWatchedBinary(0)

	go func() {
		_ = srv.Serve(ln)
	}()

	// If this background dashboard is bound to a non-ephemeral port and a newer binary is compiled on disk,
	// gracefully yield the HTTP port and spawn the updated binary in standalone -dashboard mode.
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
