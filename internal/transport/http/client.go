package http

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"memorize/internal/domain"
	"memorize/internal/infra/config"
)

var (
	daemonProbeHTTP = &http.Client{Timeout: 400 * time.Millisecond}
	daemonRPCClient = &http.Client{Timeout: 35 * time.Second}
	daemonHookHTTP  = &http.Client{}
)

// ExplicitDaemonOverride returns any explicitly provided or env-configured daemon listen address.
func ExplicitDaemonOverride(explicit string) string {
	for _, v := range []string{explicit, os.Getenv("MEMORY_DAEMON_ADDR"), os.Getenv("MEMORY_DASHBOARD_ADDR")} {
		if s := strings.TrimSpace(v); s != "" {
			return s
		}
	}
	return ""
}

// ResolveDaemonAddr resolves the explicit override or default daemon address.
func ResolveDaemonAddr(explicit string) string {
	if ov := ExplicitDaemonOverride(explicit); ov != "" {
		return ov
	}
	return domain.DefaultDaemonAddr
}

// ResolveDaemonTargetAddr resolves the target address and whether an explicit override was set.
func ResolveDaemonTargetAddr(explicit, dbPath string) (string, bool) {
	if ov := ExplicitDaemonOverride(explicit); ov != "" {
		return ov, true
	}
	cfg := config.LoadPortConfig(dbPath)
	return net.JoinHostPort(domain.DefaultDaemonHost, strconv.Itoa(cfg.DaemonPort)), false
}

// ResolveDaemonAddrForDB resolves the target daemon listen address from explicit override or persisted port config.
func ResolveDaemonAddrForDB(explicit, dbPath string) string {
	addr, _ := ResolveDaemonTargetAddr(explicit, dbPath)
	return addr
}

// SameDBPath reports whether two database paths match (treating empty paths as wildcard compatible).
func SameDBPath(a, b string) bool {
	if a == "" || b == "" {
		return true
	}
	return config.SamePathClean(a, b)
}

// QueryDaemonStatus fetches live runtime status from the daemon at addr.
func QueryDaemonStatus(addr string) (*DaemonStatus, error) {
	resp, err := daemonProbeHTTP.Get("http://" + addr + "/api/daemon/status")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("daemon status HTTP %d", resp.StatusCode)
	}
	var st DaemonStatus
	if err := json.NewDecoder(resp.Body).Decode(&st); err != nil {
		return nil, err
	}
	return &st, nil
}

// DiscoverLiveDaemonStatus probes candidate daemon addresses and returns the running instance matching dbPath.
func DiscoverLiveDaemonStatus(explicitAddr, dbPath string) (*DaemonStatus, string, bool) {
	seen := make(map[string]bool)
	var candidates []string
	addCand := func(a string) {
		s := strings.TrimSpace(a)
		s = strings.TrimPrefix(strings.TrimPrefix(s, "http://"), "https://")
		s = strings.TrimRight(s, "/")
		if s == "" || seen[s] {
			return
		}
		seen[s] = true
		candidates = append(candidates, s)
	}

	if ov := ExplicitDaemonOverride(explicitAddr); ov != "" {
		addCand(ov)
		if st, ok := config.ReadDaemonState(dbPath); ok && st.ListenAddr != "" {
			addCand(st.ListenAddr)
		}
	} else {
		if st, ok := config.ReadDaemonState(dbPath); ok && st.ListenAddr != "" {
			addCand(st.ListenAddr)
		}
		cfg := config.LoadPortConfig(dbPath)
		addCand(net.JoinHostPort(domain.DefaultDaemonHost, strconv.Itoa(cfg.DaemonPort)))
		addCand(domain.DefaultDaemonAddr)
	}

	for _, cand := range candidates {
		if st, err := QueryDaemonStatus(cand); err == nil && st.Running && SameDBPath(st.DBPath, dbPath) {
			return st, cand, true
		}
	}
	return nil, "", false
}

// ProbeDashboardMetaAlive checks whether a local dashboard server responds on /api/dashboard/meta.
func ProbeDashboardMetaAlive(addr string) bool {
	client := &http.Client{Timeout: 300 * time.Millisecond}
	resp, err := client.Get("http://" + addr + "/api/dashboard/meta")
	if err != nil {
		return false
	}
	_ = resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

// RemoteDashboardBinModTime retrieves the binary modification timestamp reported by a local dashboard server.
func RemoteDashboardBinModTime(addr string) (int64, bool) {
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

// RequestDashboardYield asks an existing local dashboard listener on addr to close gracefully.
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

// PostDaemonStop sends a stop signal to the daemon listening at addr.
func PostDaemonStop(addr string) {
	req, _ := http.NewRequest(http.MethodPost, "http://"+addr+"/api/daemon/stop", bytes.NewReader([]byte(`{}`)))
	req.Header.Set("Content-Type", "application/json")
	if resp, err := daemonProbeHTTP.Do(req); err == nil {
		_ = resp.Body.Close()
	}
}

// PostDaemonRestart triggers an in-place daemon restart on addr.
func PostDaemonRestart(addr string) {
	req, _ := http.NewRequest(http.MethodPost, "http://"+addr+"/api/daemon/restart", bytes.NewReader([]byte(`{}`)))
	req.Header.Set("Content-Type", "application/json")
	if resp, err := daemonProbeHTTP.Do(req); err == nil {
		_ = resp.Body.Close()
	}
}

// PostDaemonPortApply sends a port update request to the running daemon on addr.
func PostDaemonPortApply(addr string, req PortApplyRequest) (PortApplyResponse, error) {
	var out PortApplyResponse
	body, err := json.Marshal(req)
	if err != nil {
		return out, err
	}
	httpReq, err := http.NewRequest(http.MethodPost, "http://"+addr+"/api/daemon/port", bytes.NewReader(body))
	if err != nil {
		return out, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(httpReq)
	if err != nil {
		return out, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err := json.Unmarshal(raw, &out); err != nil && resp.StatusCode >= 300 {
		return out, fmt.Errorf("daemon returned %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	if resp.StatusCode >= 300 || !out.OK {
		msg := out.Error
		if msg == "" {
			msg = strings.TrimSpace(string(raw))
		}
		return out, fmt.Errorf("port update failed: %s", msg)
	}
	return out, nil
}

// WaitDaemonPortHealthy polls expectedAddr and daemon state until the restarted daemon serves dbPath cleanly.
func WaitDaemonPortHealthy(expectedAddr, excludeOldAddr, dbPath string, timeout time.Duration) (*DaemonStatus, bool) {
	deadline := time.Now().Add(timeout)
	exp := strings.TrimPrefix(strings.TrimPrefix(strings.TrimRight(expectedAddr, "/"), "http://"), "https://")
	old := strings.TrimPrefix(strings.TrimPrefix(strings.TrimRight(excludeOldAddr, "/"), "http://"), "https://")
	for time.Now().Before(deadline) {
		if exp != "" {
			if st, err := QueryDaemonStatus(exp); err == nil && st.Running && SameDBPath(st.DBPath, dbPath) {
				return st, true
			}
		}
		if rt, ok := config.ReadDaemonState(dbPath); ok && rt.ListenAddr != "" && (old == "" || !strings.EqualFold(rt.ListenAddr, old)) {
			if st, err := QueryDaemonStatus(rt.ListenAddr); err == nil && st.Running && SameDBPath(st.DBPath, dbPath) {
				return st, true
			}
		}
		time.Sleep(80 * time.Millisecond)
	}
	return nil, false
}

// PostDaemonHook delivers a lifecycle hook execution RPC to addr.
func PostDaemonHook(ctx context.Context, addr, mode string, rawStdin []byte, cwd, account string, timeoutMs int64) (string, int, bool) {
	payload := map[string]any{
		"mode":    mode,
		"stdin":   string(rawStdin),
		"cwd":     cwd,
		"account": account,
	}
	if timeoutMs > 0 {
		payload["timeout_ms"] = timeoutMs
	}
	body, _ := json.Marshal(payload)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://"+addr+"/api/daemon/hook", bytes.NewReader(body))
	if err != nil {
		return "", 0, false
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := daemonHookHTTP.Do(req)
	if err != nil {
		return "", 0, false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", 0, false
	}
	var out struct {
		Output   string `json:"output"`
		ExitCode int    `json:"exit_code"`
	}
	if json.NewDecoder(resp.Body).Decode(&out) != nil {
		return "", 0, false
	}
	return out.Output, out.ExitCode, true
}

// PostDaemonMCP forwards one JSON-RPC request from the stdio bridge to /api/daemon/mcp.
func PostDaemonMCP(addr, sid, cwd, account string, roots []string, payload []byte) ([]byte, bool, error) {
	req, err := http.NewRequest(http.MethodPost, "http://"+addr+"/api/daemon/mcp", bytes.NewReader(payload))
	if err != nil {
		return nil, false, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Memorize-Session", sid)
	if cwd != "" {
		req.Header.Set("X-Memorize-Cwd", cwd)
	}
	if account != "" {
		req.Header.Set("X-Memorize-Account", account)
	}
	if len(roots) > 0 {
		req.Header.Set("X-Memorize-Roots", strings.Join(roots, "\t"))
	}

	resp, err := daemonRPCClient.Do(req)
	if err != nil {
		return nil, false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNoContent {
		return nil, true, nil
	}
	respBytes, err := io.ReadAll(resp.Body)
	if resp.StatusCode == http.StatusOK && err == nil {
		return respBytes, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return nil, false, fmt.Errorf("daemon mcp HTTP %d", resp.StatusCode)
}
