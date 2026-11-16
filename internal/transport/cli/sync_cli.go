package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"strings"

	"memorize/internal/domain"
	thttp "memorize/internal/transport/http"
)

type multiScopeFlag []string

func (m *multiScopeFlag) String() string { return strings.Join(*m, ",") }
func (m *multiScopeFlag) Set(val string) error {
	for _, part := range strings.Split(val, ",") {
		if t := strings.TrimSpace(part); t != "" {
			*m = append(*m, t)
		}
	}
	return nil
}

func runSyncServeCommand(explicitAddr, dbPath, modelsDir string, hooks RuntimeHooks) int {
	if !domain.IsMemorySyncEnabled() {
		fmt.Fprintf(os.Stderr, "sync-serve error: %v\n", domain.ErrMemorySyncDisabled)
		return 1
	}
	if err := serveSyncIO(context.Background(), explicitAddr, dbPath, modelsDir, hooks, os.Stdin, os.Stdout); err != nil {
		log.Printf("sync-serve error: %v", err)
		return 1
	}
	return 0
}

func serveSyncIO(ctx context.Context, explicitAddr, dbPath, modelsDir string, hooks RuntimeHooks, r io.Reader, w io.Writer) error {
	if !isInlineModeForced(dbPath) && hooks.EnsureDaemonRunning != nil {
		if st, ok := hooks.EnsureDaemonRunning(explicitAddr, dbPath, modelsDir); ok {
			addr := thttp.ResolveDaemonAddrForDB(explicitAddr, dbPath)
			if st != nil && st.ListenAddr != "" {
				addr = st.ListenAddr
			}
			return thttp.ServeSyncStreamOverDaemon(ctx, addr, r, w)
		}
	}
	if hooks.ServeInlineSync != nil {
		return hooks.ServeInlineSync(ctx, dbPath, modelsDir, r, w)
	}
	if hooks.EnsureDaemonRunning != nil {
		return errors.New("memorize daemon unavailable for sync-serve")
	}
	return errors.New("sync-serve handler not configured")
}

func resolveSyncDaemonAddr(explicitAddr, dbPath, modelsDir string, hooks RuntimeHooks) (string, error) {
	if hooks.EnsureDaemonRunning != nil {
		st, ok := hooks.EnsureDaemonRunning(explicitAddr, dbPath, modelsDir)
		if !ok {
			return "", fmt.Errorf("memorize daemon unavailable at %s", thttp.ResolveDaemonAddrForDB(explicitAddr, dbPath))
		}
		if st != nil && st.ListenAddr != "" {
			return st.ListenAddr, nil
		}
	} else if strings.TrimSpace(explicitAddr) == "" {
		return "", errors.New("memorize daemon hooks unavailable")
	}
	return thttp.ResolveDaemonAddrForDB(explicitAddr, dbPath), nil
}

func reorderFlagsBeforePositional(args []string, valueFlags map[string]bool) []string {
	var flags, pos []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			pos = append(pos, args[i+1:]...)
			break
		}
		if strings.HasPrefix(a, "-") && len(a) > 1 {
			flags = append(flags, a)
			name := strings.TrimLeft(a, "-")
			if eq := strings.IndexByte(name, '='); eq >= 0 {
				continue
			}
			if valueFlags[name] && i+1 < len(args) {
				flags = append(flags, args[i+1])
				i++
			}
			continue
		}
		pos = append(pos, a)
	}
	return append(flags, pos...)
}

func runSyncCLI(args []string, explicitAddr, dbPath, modelsDir string, hooks RuntimeHooks) int {
	if !domain.IsMemorySyncEnabled() {
		fmt.Fprintf(os.Stderr, "sync error: %v\n", domain.ErrMemorySyncDisabled)
		return 1
	}
	fs := flag.NewFlagSet("memorize sync", flag.ContinueOnError)
	var scopes multiScopeFlag
	fs.Var(&scopes, "scope", "sync only specified scope(s); repeatable or comma-separated")
	allScopes := fs.Bool("all-scopes", false, "sync all scopes")
	testOnly := fs.Bool("test", false, "test non-interactive SSH connectivity without syncing")
	remoteBin := fs.String("remote-bin", "", "explicit path to memorize binary on remote peer")
	addrOverride := fs.String("addr", "", "daemon listen address override")
	asJSON := fs.Bool("json", false, "emit JSON output")

	normArgs := reorderFlagsBeforePositional(args, map[string]bool{"scope": true, "remote-bin": true, "addr": true})
	if err := fs.Parse(normArgs); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	explicitAddr = domain.FirstNonEmpty(*addrOverride, explicitAddr)
	pos := fs.Args()
	if len(pos) == 0 {
		return runSyncStatusCLI(explicitAddr, dbPath, modelsDir, hooks, *asJSON)
	}

	sub := strings.ToLower(strings.TrimSpace(pos[0]))
	switch sub {
	case "status":
		return runSyncStatusCLI(explicitAddr, dbPath, modelsDir, hooks, *asJSON)
	case "resolve":
		if len(pos) < 3 {
			fmt.Fprintln(os.Stderr, "usage: memorize sync resolve <sync_id> <keep_local|use_remote|keep_both> [--json]")
			return 2
		}
		return runSyncResolveCLI(explicitAddr, dbPath, modelsDir, hooks, pos[1], pos[2], *asJSON)
	}

	target := strings.TrimSpace(pos[0])
	addr, err := resolveSyncDaemonAddr(explicitAddr, dbPath, modelsDir, hooks)
	if err != nil {
		fmt.Fprintf(os.Stderr, "sync error: %v\n", err)
		return 1
	}

	if *testOnly {
		resp, err := thttp.PostDaemonSync(context.Background(), addr, thttp.SyncDaemonRequest{
			Action:         "test_peer",
			PeerIDOrTarget: target,
			RemoteBin:      strings.TrimSpace(*remoteBin),
		})
		if *asJSON {
			b, _ := json.MarshalIndent(resp, "", "  ")
			fmt.Println(string(b))
			if err != nil {
				return 1
			}
			return 0
		}
		if err != nil {
			fmt.Fprintf(os.Stderr, "SSH probe failed for %s: %v\n", target, err)
			return 1
		}
		fmt.Printf("SSH OK: %s -> device=%s (%s) protocol=v%d elapsed=%dms\n",
			resp.Probe.SSHTarget, resp.Probe.RemoteDeviceID, resp.Probe.RemoteLabel, resp.Probe.ProtocolVersion, resp.Probe.ElapsedMs)
		return 0
	}

	scopeMode := ""
	if *allScopes {
		scopeMode = domain.SyncScopeModeAll
	} else if len(scopes) > 0 {
		scopeMode = domain.SyncScopeModeSelected
	}
	resp, err := thttp.PostDaemonSync(context.Background(), addr, thttp.SyncDaemonRequest{
		Action:         "run_peer",
		PeerIDOrTarget: target,
		ScopeMode:      scopeMode,
		Scopes:         scopes,
		RemoteBin:      strings.TrimSpace(*remoteBin),
	})
	if *asJSON {
		b, _ := json.MarshalIndent(resp, "", "  ")
		fmt.Println(string(b))
		if err != nil {
			return 1
		}
		return 0
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "sync failed: %v\n", err)
		return 1
	}
	r := resp.Report
	fmt.Printf("Synced with %s (%s) in %dms\n", r.SSHTarget, r.RemoteDeviceID, r.ElapsedMs)
	fmt.Printf("  Scopes: %s\n", strings.Join(r.MatchedScopes, ", "))
	fmt.Printf("  Pull: applied=%d deduped=%d skipped=%d conflicts=%d\n", r.PulledApplied, r.PulledDeduped, r.PulledSkipped, r.PullConflicts)
	fmt.Printf("  Push: sent=%d applied=%d conflicts=%d\n", r.PushedSent, r.PushedApplied, r.PushedConflicts)
	if r.PullConflicts > 0 || r.PushedConflicts > 0 {
		fmt.Println("  Resolve pending conflicts in Dashboard Memories -> Sync or run `memorize sync status`.")
	}
	return 0
}

func runSyncStatusCLI(explicitAddr, dbPath, modelsDir string, hooks RuntimeHooks, asJSON bool) int {
	addr, err := resolveSyncDaemonAddr(explicitAddr, dbPath, modelsDir, hooks)
	if err != nil {
		fmt.Fprintf(os.Stderr, "sync status error: %v\n", err)
		return 1
	}
	resp, err := thttp.PostDaemonSync(context.Background(), addr, thttp.SyncDaemonRequest{Action: "status"})
	if asJSON {
		b, _ := json.MarshalIndent(resp, "", "  ")
		fmt.Println(string(b))
		if err != nil {
			return 1
		}
		return 0
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "sync status error: %v\n", err)
		return 1
	}
	fmt.Printf("Device: %s (%s)\n", resp.DeviceID, resp.DeviceLabel)
	fmt.Printf("Saved Peers (%d):\n", len(resp.Peers))
	for _, p := range resp.Peers {
		scopeDesc := "all scopes"
		if p.ScopeMode != domain.SyncScopeModeAll {
			scopeDesc = strings.Join(p.Scopes, ",")
		}
		fmt.Printf("  - %s -> %s [%s] status=%s\n", p.PeerID, p.SSHTarget, scopeDesc, p.LastStatus)
	}
	fmt.Printf("Pending Conflicts (%d):\n", len(resp.Conflicts))
	for _, c := range resp.Conflicts {
		fmt.Printf("  - %s [%s]: %s\n    local : %s\n    remote: %s\n", c.SyncID, c.Scope, c.Reason, c.LocalSnapshot.Content, c.RemoteSnapshot.Content)
	}
	return 0
}

func runSyncResolveCLI(explicitAddr, dbPath, modelsDir string, hooks RuntimeHooks, syncID, decision string, asJSON bool) int {
	addr, err := resolveSyncDaemonAddr(explicitAddr, dbPath, modelsDir, hooks)
	if err != nil {
		fmt.Fprintf(os.Stderr, "sync resolve error: %v\n", err)
		return 1
	}
	resp, err := thttp.PostDaemonSync(context.Background(), addr, thttp.SyncDaemonRequest{
		Action:   "resolve",
		SyncID:   syncID,
		Decision: decision,
	})
	if asJSON {
		b, _ := json.MarshalIndent(resp, "", "  ")
		fmt.Println(string(b))
		if err != nil {
			return 1
		}
		return 0
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "sync resolve failed: %v\n", err)
		return 1
	}
	fmt.Println(resp.Message)
	return 0
}
