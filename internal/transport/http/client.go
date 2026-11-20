package http

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"memorize/internal/domain"
	"memorize/internal/infra/config"
)

var ErrDaemonAuthRequired = errors.New("daemon authentication required")

var (
	daemonProbeHTTP = &http.Client{Timeout: 400 * time.Millisecond}
	daemonRPCClient = &http.Client{Timeout: 35 * time.Second}
	daemonHookHTTP  = &http.Client{}
)

func ExplicitDaemonOverride(explicit string) string {
	for _, v := range []string{explicit, os.Getenv("MEMORY_DAEMON_ADDR"), os.Getenv("MEMORY_DASHBOARD_ADDR")} {
		if s := strings.TrimSpace(v); s != "" {
			return s
		}
	}
	return ""
}

func ResolveDaemonTargetAddr(explicit, dbPath string) (string, bool) {
	if ov := ExplicitDaemonOverride(explicit); ov != "" {
		return ov, true
	}
	cfg := config.LoadPortConfig(dbPath)
	return net.JoinHostPort(domain.DefaultDaemonHost, strconv.Itoa(cfg.DaemonPort)), false
}

func ResolveDaemonAddrForDB(explicit, dbPath string) string {
	addr, _ := ResolveDaemonTargetAddr(explicit, dbPath)
	return addr
}

func SameDBPath(a, b string) bool {
	if a == "" || b == "" {
		return true
	}
	return config.SamePathClean(a, b)
}

func buildEndpointURL(addrOrURL, path string) string {
	s := strings.TrimRight(strings.TrimSpace(addrOrURL), "/")
	if !strings.HasPrefix(s, "http://") && !strings.HasPrefix(s, "https://") {
		s = "http://" + s
	}
	return s + path
}

func QueryDaemonStatus(addr string) (*DaemonStatus, error) {
	return QueryDaemonStatusWithAuth(addr, "")
}

func QueryDaemonStatusWithAuth(addrOrURL, bearerToken string) (*DaemonStatus, error) {
	req, err := http.NewRequest(http.MethodGet, buildEndpointURL(addrOrURL, "/api/daemon/status"), nil)
	if err != nil {
		return nil, err
	}
	if tok := strings.TrimSpace(bearerToken); tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	client := daemonProbeHTTP
	if strings.HasPrefix(strings.TrimSpace(addrOrURL), "http://") || strings.HasPrefix(strings.TrimSpace(addrOrURL), "https://") {
		client = daemonRPCClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return nil, fmt.Errorf("%w (HTTP %d)", ErrDaemonAuthRequired, resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("daemon status HTTP %d", resp.StatusCode)
	}
	var st DaemonStatus
	if err := json.NewDecoder(resp.Body).Decode(&st); err != nil {
		return nil, err
	}
	return &st, nil
}

func ProbeDaemonHealthz(addrOrURL string) bool {
	req, err := http.NewRequest(http.MethodGet, buildEndpointURL(addrOrURL, "/healthz"), nil)
	if err != nil {
		return false
	}
	resp, err := daemonProbeHTTP.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

func ProbeLiveDaemonState(explicitAddr, dbPath string) (*DaemonStatus, string, bool, bool) {
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

	rtState, hasRT := config.ReadDaemonState(dbPath)
	if ov := ExplicitDaemonOverride(explicitAddr); ov != "" {
		addCand(ov)
		if hasRT && rtState.ListenAddr != "" {
			addCand(rtState.ListenAddr)
		}
	} else {
		if hasRT && rtState.ListenAddr != "" {
			addCand(rtState.ListenAddr)
		}
		cfg := config.LoadPortConfig(dbPath)
		addCand(net.JoinHostPort(domain.DefaultDaemonHost, strconv.Itoa(cfg.DaemonPort)))
		addCand(domain.DefaultDaemonAddr)
	}

	authAddr := ""
	for _, cand := range candidates {
		st, err := QueryDaemonStatus(cand)
		if err == nil && st.Running && SameDBPath(st.DBPath, dbPath) {
			return st, cand, true, false
		}
		if errors.Is(err, ErrDaemonAuthRequired) && authAddr == "" {
			authAddr = cand
		}
	}
	if authAddr != "" {
		return &DaemonStatus{
			Running:    true,
			ServerMode: true,
			ListenAddr: authAddr,
		}, authAddr, false, true
	}
	return nil, "", false, false
}

func DiscoverLiveDaemonStatus(explicitAddr, dbPath string) (*DaemonStatus, string, bool) {
	st, cand, ok, _ := ProbeLiveDaemonState(explicitAddr, dbPath)
	return st, cand, ok
}

func ProbeDashboardMetaAlive(addr string) bool {
	client := &http.Client{Timeout: 300 * time.Millisecond}
	resp, err := client.Get("http://" + addr + "/api/dashboard/meta")
	if err != nil {
		return false
	}
	_ = resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

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

func PostDaemonHook(ctx context.Context, addr, mode string, rawStdin []byte, cwd, account string, timeoutMs int64) (string, int, bool) {
	out, code, err := PostDaemonHookWithAuth(ctx, addr, "", mode, rawStdin, cwd, account, timeoutMs, nil)
	return out, code, err == nil
}

func PostDaemonHookWithAuth(ctx context.Context, addrOrURL, bearerToken, mode string, rawStdin []byte, cwd, account string, timeoutMs int64, clientCtx *domain.ClientHookContext) (string, int, error) {
	payload := DaemonHookRequest{
		Mode:      mode,
		Stdin:     string(rawStdin),
		Cwd:       cwd,
		Account:   account,
		TimeoutMs: timeoutMs,
		ClientCtx: clientCtx,
	}
	body, _ := json.Marshal(payload)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, buildEndpointURL(addrOrURL, "/api/daemon/hook"), bytes.NewReader(body))
	if err != nil {
		return "", 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	if tok := strings.TrimSpace(bearerToken); tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	resp, err := daemonHookHTTP.Do(req)
	if err != nil {
		return "", 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", 0, fmt.Errorf("daemon hook HTTP %d", resp.StatusCode)
	}
	var out struct {
		Output   string `json:"output"`
		ExitCode int    `json:"exit_code"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", 0, err
	}
	return out.Output, out.ExitCode, nil
}

func PostDaemonMCPWithAuth(addrOrURL, bearerToken, sid, cwd, account string, roots []string, scopeHints map[string]string, payload []byte) ([]byte, bool, error) {
	req, err := http.NewRequest(http.MethodPost, buildEndpointURL(addrOrURL, "/api/daemon/mcp"), bytes.NewReader(payload))
	if err != nil {
		return nil, false, err
	}
	req.Header.Set("Content-Type", "application/json")
	if tok := strings.TrimSpace(bearerToken); tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
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
	if len(scopeHints) > 0 {
		if rawHints, err := json.Marshal(scopeHints); err == nil {
			req.Header.Set("X-Memorize-Scope-Hints", string(rawHints))
		}
	}

	resp, err := daemonRPCClient.Do(req)
	if err != nil {
		return nil, false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNoContent {
		return nil, true, nil
	}
	respBytes, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if resp.StatusCode == http.StatusOK && err == nil {
		return respBytes, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return nil, false, fmt.Errorf("daemon mcp HTTP %d", resp.StatusCode)
}

func FetchDaemonExportWithAuth(ctx context.Context, addrOrURL, bearerToken string, query url.Values) (domain.MemoryBundle, error) {
	ep := buildEndpointURL(addrOrURL, "/api/daemon/export")
	if encoded := query.Encode(); encoded != "" {
		ep += "?" + encoded
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ep, nil)
	if err != nil {
		return domain.MemoryBundle{}, err
	}
	if tok := strings.TrimSpace(bearerToken); tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	resp, err := daemonRPCClient.Do(req)
	if err != nil {
		return domain.MemoryBundle{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return domain.MemoryBundle{}, ErrDaemonAuthRequired
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return domain.MemoryBundle{}, fmt.Errorf("export HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var out domain.MemoryBundle
	if err := json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(&out); err != nil {
		return domain.MemoryBundle{}, err
	}
	return out, nil
}

func PostDaemonImportWithAuth(ctx context.Context, addrOrURL, bearerToken string, bundle domain.MemoryBundle, opts domain.BundleImportOptions) (domain.BundleImportResult, error) {
	payload, err := json.Marshal(map[string]any{
		"bundle":  bundle,
		"options": opts,
	})
	if err != nil {
		return domain.BundleImportResult{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, buildEndpointURL(addrOrURL, "/api/daemon/import"), bytes.NewReader(payload))
	if err != nil {
		return domain.BundleImportResult{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	if tok := strings.TrimSpace(bearerToken); tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	resp, err := daemonHookHTTP.Do(req)
	if err != nil {
		return domain.BundleImportResult{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return domain.BundleImportResult{}, ErrDaemonAuthRequired
	}
	if resp.StatusCode != http.StatusOK {
		var errBody struct {
			Error string `json:"error"`
		}
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		if json.Unmarshal(raw, &errBody) == nil && strings.TrimSpace(errBody.Error) != "" {
			return domain.BundleImportResult{}, fmt.Errorf("%s", errBody.Error)
		}
		return domain.BundleImportResult{}, fmt.Errorf("import HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	var out domain.BundleImportResult
	if err := json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(&out); err != nil {
		return domain.BundleImportResult{}, err
	}
	return out, nil
}
