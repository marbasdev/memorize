package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"memorize/internal/domain"
	"memorize/internal/infra/config"
	"memorize/internal/modules/memory"
	thttp "memorize/internal/transport/http"
)

func resolveCLIUploadTarget(dataDir, explicitURL, explicitToken, profileName, repoPath string) (domain.RemoteTargetConfig, error) {
	if u := config.NormalizeRemoteURL(explicitURL); u != "" {
		tok := strings.TrimSpace(explicitToken)
		if tok == "" {
			return domain.RemoteTargetConfig{}, fmt.Errorf("--token is required when --url is specified")
		}
		return domain.RemoteTargetConfig{URL: u, Token: tok}, nil
	}
	if prof := strings.TrimSpace(profileName); prof != "" {
		t, ok := config.LookupRemoteProfile(dataDir, prof)
		if !ok {
			return domain.RemoteTargetConfig{}, fmt.Errorf("saved profile %q not found", domain.NormalizeAccount(prof))
		}
		return t, nil
	}
	searchDir := strings.TrimSpace(repoPath)
	if searchDir == "" {
		searchDir, _ = os.Getwd()
	}
	t, ok := config.ResolveRemoteTarget(dataDir, searchDir)
	if !ok {
		return domain.RemoteTargetConfig{}, fmt.Errorf("no remote team server configured (run `memorize connect` or pass --url/--token)")
	}
	return t, nil
}

func printImportSummary(res domain.BundleImportResult, destLabel string) {
	prefix := "Imported"
	if res.DryRun {
		prefix = "Dry-run preview"
	}
	enginePart := ""
	if res.Engine != "" {
		enginePart = " via " + res.Engine
	}
	fmt.Printf("%s -> %s (mode=%s%s): total=%d inserted=%d overwritten=%d skipped=%d\n",
		prefix, destLabel, res.Mode, enginePart, res.Total, res.Inserted, res.Overwritten, res.Skipped)
	for _, it := range res.Items {
		tag := strings.ToUpper(it.Action)
		detail := ""
		if it.NewID > 0 {
			detail = fmt.Sprintf(" -> #%d", it.NewID)
		} else if len(it.TargetIDs) > 0 {
			detail = fmt.Sprintf(" %v", it.TargetIDs)
		}
		if it.Reason != "" && it.Action != "insert" {
			detail += " (" + it.Reason + ")"
		}
		fmt.Printf("  [%s] [%s/%s] %s%s\n", tag, it.Scope, it.Category, domain.TruncateUTF8Bytes(it.Content, 100), detail)
	}
}

func runPushCLI(args []string, dbPath string) int {
	fs := flag.NewFlagSet("push", flag.ContinueOnError)
	scopeFlag := fs.String("scope", "", "scope to push from local DB (e.g. global, personal, project:<name>)")
	allFlag := fs.Bool("all", false, "explicitly push all active scopes from local DB")
	targetScope := fs.String("target-scope", "", "optional scope override on destination server")
	modeFlag := fs.String("mode", "exact", "import mode on server: 'exact' (1:1 faithful migration) or 'merge' (AI dedup/conflict merge)")
	dryRun := fs.Bool("dry-run", false, "preview changes without writing to remote server")
	toProfile := fs.String("to", "", "saved account profile name from `memorize switch`")
	urlFlag := fs.String("url", "", "explicit remote server URL")
	tokenFlag := fs.String("token", "", "explicit remote access token")
	repoFlag := fs.String("repo", "", "local repo path for resolving per-repo remote binding")
	jsonFlag := fs.Bool("json", false, "output JSON")

	ordered := reorderFlagsBeforePositional(args, map[string]bool{
		"scope": true, "target-scope": true, "mode": true, "to": true, "url": true, "token": true, "repo": true,
	})
	if err := fs.Parse(ordered); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}

	rawScope := strings.TrimSpace(*scopeFlag)
	if rawScope == "" && !*allFlag {
		fmt.Fprintln(os.Stderr, "safety check: specify --scope <global|personal|project:name> or --all to push local memories")
		return 2
	}
	if rawScope != "" && *allFlag {
		fmt.Fprintln(os.Stderr, "cannot combine --scope and --all")
		return 2
	}

	dataDir := resolveClientDataDir(dbPath)
	target, err := resolveCLIUploadTarget(dataDir, *urlFlag, *tokenFlag, *toProfile, *repoFlag)
	if err != nil {
		fmt.Fprintf(os.Stderr, "push failed: %v\n", err)
		return 1
	}

	mem, err := memory.Open(dbPath, nil)
	if err != nil {
		fmt.Fprintf(os.Stderr, "open local db %s failed: %v\n", dbPath, err)
		return 1
	}
	defer func() { _ = mem.Close() }()

	var rq domain.RecordQuery
	if rawScope != "" {
		rq.Scope = domain.NormalizeScope(rawScope)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	bundle, err := mem.ExportBundle(ctx, rq, nil)
	if err != nil {
		fmt.Fprintf(os.Stderr, "export local memories failed: %v\n", err)
		return 1
	}
	if len(bundle.Items) == 0 {
		if *jsonFlag {
			_ = json.NewEncoder(os.Stdout).Encode(domain.BundleImportResult{DryRun: *dryRun, Mode: *modeFlag})
			return 0
		}
		fmt.Println("No active local memories matched the requested scope.")
		return 0
	}

	res, err := thttp.PostDaemonImportWithAuth(ctx, target.URL, target.Token, bundle, domain.BundleImportOptions{
		Mode:        *modeFlag,
		TargetScope: strings.TrimSpace(*targetScope),
		DryRun:      *dryRun,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "push to %s failed: %v\n", target.URL, err)
		return 1
	}

	if *jsonFlag {
		_ = json.NewEncoder(os.Stdout).Encode(res)
		return 0
	}
	dest := target.URL
	if target.Account != "" {
		dest = fmt.Sprintf("@%s (%s)", target.Account, target.URL)
	}
	printImportSummary(res, dest)
	return 0
}

func runExportCLI(args []string, dbPath string) int {
	fs := flag.NewFlagSet("export", flag.ContinueOnError)
	scopeFlag := fs.String("scope", "", "filter by scope (e.g. global, personal, project:<name>)")
	idsFlag := fs.String("ids", "", "comma-separated memory IDs to export")
	catFlag := fs.String("category", "", "filter by category")
	subFlag := fs.String("sub-category", "", "filter by sub-category")
	stateFlag := fs.String("state", "active", "state filter: active, pinned, general")
	outFlag := fs.String("out", "", "output file path (default: stdout)")
	outShort := fs.String("o", "", "output file path (shorthand)")
	remoteFlag := fs.Bool("remote", false, "export from connected remote team server")
	localFlag := fs.Bool("local", false, "force export from local SQLite DB")
	profileFlag := fs.String("profile", "", "export from saved account profile")

	ordered := reorderFlagsBeforePositional(args, map[string]bool{
		"scope": true, "ids": true, "category": true, "sub-category": true, "state": true, "out": true, "o": true, "profile": true,
	})
	if err := fs.Parse(ordered); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}

	dataDir := resolveClientDataDir(dbPath)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	var bundle domain.MemoryBundle
	var err error
	useRemote := *remoteFlag || strings.TrimSpace(*profileFlag) != ""
	var remoteTarget domain.RemoteTargetConfig
	if !*localFlag {
		if useRemote {
			remoteTarget, err = resolveCLIUploadTarget(dataDir, "", "", *profileFlag, "")
			if err != nil {
				fmt.Fprintf(os.Stderr, "export failed: %v\n", err)
				return 1
			}
		} else {
			cwd, _ := os.Getwd()
			if t, ok := config.ResolveRemoteTarget(dataDir, cwd); ok {
				remoteTarget = t
				useRemote = true
			}
		}
	}

	if useRemote {
		q := url.Values{}
		if sc := strings.TrimSpace(*scopeFlag); sc != "" {
			q.Set("scope", domain.NormalizeScope(sc))
		}
		if ids := strings.TrimSpace(*idsFlag); ids != "" {
			q.Set("ids", ids)
		}
		if cat := strings.TrimSpace(*catFlag); cat != "" {
			q.Set("category", cat)
		}
		if sub := strings.TrimSpace(*subFlag); sub != "" {
			q.Set("sub_category", sub)
		}
		if st := strings.TrimSpace(*stateFlag); st != "" {
			q.Set("state", st)
		}
		bundle, err = thttp.FetchDaemonExportWithAuth(ctx, remoteTarget.URL, remoteTarget.Token, q)
	} else {
		mem, oerr := memory.Open(dbPath, nil)
		if oerr != nil {
			fmt.Fprintf(os.Stderr, "open local db failed: %v\n", oerr)
			return 1
		}
		defer func() { _ = mem.Close() }()

		var ids []int64
		if rawIDs := strings.TrimSpace(*idsFlag); rawIDs != "" {
			for _, part := range strings.Split(rawIDs, ",") {
				if id, perr := strconv.ParseInt(strings.TrimSpace(part), 10, 64); perr == nil && id > 0 {
					ids = append(ids, id)
				}
			}
		}
		rq := domain.RecordQuery{
			State:       strings.TrimSpace(*stateFlag),
			Category:    strings.TrimSpace(*catFlag),
			SubCategory: strings.TrimSpace(*subFlag),
		}
		if sc := strings.TrimSpace(*scopeFlag); sc != "" {
			rq.Scope = domain.NormalizeScope(sc)
		}
		bundle, err = mem.ExportBundle(ctx, rq, ids)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "export failed: %v\n", err)
		return 1
	}

	b, err := json.MarshalIndent(bundle, "", "  ")
	if err != nil {
		fmt.Fprintf(os.Stderr, "encode bundle failed: %v\n", err)
		return 1
	}
	b = append(b, '\n')

	outPath := domain.FirstNonEmpty(*outFlag, *outShort)
	if outPath != "" {
		if err := os.WriteFile(outPath, b, 0o600); err != nil {
			fmt.Fprintf(os.Stderr, "write %s failed: %v\n", outPath, err)
			return 1
		}
		fmt.Fprintf(os.Stderr, "Exported %d memories -> %s\n", len(bundle.Items), outPath)
		return 0
	}
	_, _ = os.Stdout.Write(b)
	return 0
}

func runImportCLI(args []string, dbPath, modelsDir string, hooks RuntimeHooks) int {
	fs := flag.NewFlagSet("import", flag.ContinueOnError)
	scopeFlag := fs.String("scope", "", "optional target scope override for all imported items")
	modeFlag := fs.String("mode", "merge", "import mode: 'merge' (AI dedup/overwrite) or 'exact' (deterministic exact dedup)")
	dryRun := fs.Bool("dry-run", false, "preview import decisions without saving")
	remoteFlag := fs.Bool("remote", false, "import into connected remote team server")
	localFlag := fs.Bool("local", false, "force import into local daemon/DB")
	profileFlag := fs.String("profile", "", "import into saved account profile")
	jsonFlag := fs.Bool("json", false, "output JSON")

	ordered := reorderFlagsBeforePositional(args, map[string]bool{
		"scope": true, "mode": true, "profile": true,
	})
	if err := fs.Parse(ordered); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}

	pos := fs.Args()
	if len(pos) == 0 {
		fmt.Fprintln(os.Stderr, "usage: memorize import <bundle.json|-> [--scope <target>] [--mode merge|exact] [--dry-run] [--profile <account>]")
		return 2
	}

	var rawBytes []byte
	var err error
	if pos[0] == "-" {
		rawBytes, err = io.ReadAll(io.LimitReader(os.Stdin, 4<<20))
	} else {
		rawBytes, err = os.ReadFile(pos[0])
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "read bundle failed: %v\n", err)
		return 1
	}

	var bundle domain.MemoryBundle
	if err := json.Unmarshal(rawBytes, &bundle); err != nil {
		fmt.Fprintf(os.Stderr, "invalid bundle JSON: %v\n", err)
		return 1
	}
	if len(bundle.Items) == 0 {
		fmt.Fprintln(os.Stderr, "bundle contains 0 items")
		return 1
	}

	opts := domain.BundleImportOptions{
		Mode:        strings.TrimSpace(*modeFlag),
		TargetScope: strings.TrimSpace(*scopeFlag),
		DryRun:      *dryRun,
	}
	dataDir := resolveClientDataDir(dbPath)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	var res domain.BundleImportResult
	destLabel := "local"

	useRemote := *remoteFlag || strings.TrimSpace(*profileFlag) != ""
	var target domain.RemoteTargetConfig
	if !*localFlag {
		if useRemote {
			target, err = resolveCLIUploadTarget(dataDir, "", "", *profileFlag, "")
			if err != nil {
				fmt.Fprintf(os.Stderr, "import failed: %v\n", err)
				return 1
			}
		} else {
			cwd, _ := os.Getwd()
			if t, ok := config.ResolveRemoteTarget(dataDir, cwd); ok {
				target = t
				useRemote = true
			}
		}
	}

	if useRemote {
		res, err = thttp.PostDaemonImportWithAuth(ctx, target.URL, target.Token, bundle, opts)
		destLabel = target.URL
		if target.Account != "" {
			destLabel = fmt.Sprintf("@%s (%s)", target.Account, target.URL)
		}
	} else {
		var daemonAddr string
		if !isInlineModeForced(dbPath) {
			if st, liveAddr, ok := thttp.DiscoverLiveDaemonStatus("", dbPath); ok && st != nil {
				daemonAddr = liveAddr
			} else if hooks.EnsureDaemonRunning != nil {
				if st, ok := hooks.EnsureDaemonRunning("", dbPath, modelsDir); ok && st != nil && st.ListenAddr != "" {
					daemonAddr = st.ListenAddr
				}
			}
		}
		if daemonAddr != "" {
			res, err = thttp.PostDaemonImportWithAuth(ctx, daemonAddr, "", bundle, opts)
			destLabel = daemonAddr
		} else {
			mem, oerr := memory.Open(dbPath, nil)
			if oerr != nil {
				fmt.Fprintf(os.Stderr, "open local db failed: %v\n", oerr)
				return 1
			}
			defer func() { _ = mem.Close() }()
			res, err = mem.ImportBundle(ctx, bundle, opts)
		}
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "import failed: %v\n", err)
		return 1
	}

	if *jsonFlag {
		_ = json.NewEncoder(os.Stdout).Encode(res)
		return 0
	}
	printImportSummary(res, destLabel)
	return 0
}
