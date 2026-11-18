package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"memorize/internal/domain"
	"memorize/internal/infra/config"
	"memorize/internal/infra/git"
	"memorize/internal/infra/sqlite"
	thttp "memorize/internal/transport/http"
)

func runServeCLI(args []string, dbPath, modelsDir string, hooks RuntimeHooks) int {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	addr := fs.String("addr", domain.DefaultServeAddr, "team server listen address")
	account := fs.String("account", "", "bootstrap admin account name on first run")
	token := fs.String("token", "", "bootstrap admin token on first run (auto-generated if omitted)")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if strings.TrimSpace(*account) != "" || strings.TrimSpace(*token) != "" {
		db, err := sqlite.Open(dbPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "open database failed: %v\n", err)
			return 1
		}
		if err := sqlite.InitSchema(db); err != nil {
			_ = db.Close()
			fmt.Fprintf(os.Stderr, "init schema failed: %v\n", err)
			return 1
		}
		issued, created, err := sqlite.EnsureBootstrapAdmin(context.Background(), db, *account, *token)
		_ = db.Close()
		if err != nil {
			fmt.Fprintf(os.Stderr, "bootstrap admin initialization failed: %v\n", err)
			return 1
		}
		if created && strings.TrimSpace(*token) == "" {
			_ = printIssuedToken(issued, false, "Initialized admin account")
		}
	}
	if hooks.RunTeamServer == nil {
		fmt.Fprintln(os.Stderr, "team server handler not configured")
		return 1
	}
	listenAddr := strings.TrimSpace(*addr)
	if listenAddr == "" {
		listenAddr = domain.DefaultServeAddr
	}
	return hooks.RunTeamServer(listenAddr, dbPath, modelsDir)
}

func runUserCLI(args []string, dbPath string) int {
	if len(args) == 0 {
		printUserCLIUsage()
		return 2
	}
	sub := strings.ToLower(strings.TrimSpace(args[0]))
	fs := flag.NewFlagSet("user "+sub, flag.ContinueOnError)
	roleFlag := fs.String("role", domain.RoleMember, "account role (member or admin)")
	jsonFlag := fs.Bool("json", false, "output JSON")
	ordered := reorderFlagsBeforePositional(args[1:], map[string]bool{"role": true})
	if err := fs.Parse(ordered); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	pos := fs.Args()

	db, err := sqlite.Open(dbPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "open database failed: %v\n", err)
		return 1
	}
	defer db.Close()
	if err := sqlite.InitSchema(db); err != nil {
		fmt.Fprintf(os.Stderr, "init schema failed: %v\n", err)
		return 1
	}
	ctx := context.Background()

	switch sub {
	case "list", "ls":
		users, err := sqlite.ListUsers(ctx, db)
		if err != nil {
			fmt.Fprintf(os.Stderr, "list users failed: %v\n", err)
			return 1
		}
		if *jsonFlag {
			_ = json.NewEncoder(os.Stdout).Encode(map[string]any{"users": users})
			return 0
		}
		if len(users) == 0 {
			fmt.Println("No team server accounts yet. Create an admin with: memorize user add admin --role admin")
			return 0
		}
		for _, u := range users {
			status := "active"
			if !u.Active {
				status = "disabled"
			}
			lastSeen := u.LastSeenAt
			if lastSeen == "" {
				lastSeen = "-"
			}
			fmt.Printf("@%-18s role=%-6s status=%-8s hint=%s... last_seen=%s\n",
				u.Account, u.Role, status, u.TokenHint, lastSeen)
		}
		return 0

	case "add", "create":
		if len(pos) < 1 || strings.TrimSpace(pos[0]) == "" {
			fmt.Fprintln(os.Stderr, "usage: memorize user add <account> [--role member|admin] [--json]")
			return 2
		}
		issued, err := sqlite.CreateUser(ctx, db, pos[0], *roleFlag)
		if err != nil {
			fmt.Fprintf(os.Stderr, "create user failed: %v\n", err)
			return 1
		}
		return printIssuedToken(issued, *jsonFlag, "Created account")

	case "rotate":
		if len(pos) < 1 || strings.TrimSpace(pos[0]) == "" {
			fmt.Fprintln(os.Stderr, "usage: memorize user rotate <account> [--json]")
			return 2
		}
		issued, err := sqlite.RotateUserToken(ctx, db, pos[0])
		if err != nil {
			fmt.Fprintf(os.Stderr, "rotate token failed: %v\n", err)
			return 1
		}
		return printIssuedToken(issued, *jsonFlag, "Rotated token")

	case "enable", "activate":
		if len(pos) < 1 || strings.TrimSpace(pos[0]) == "" {
			fmt.Fprintln(os.Stderr, "usage: memorize user enable <account>")
			return 2
		}
		if err := sqlite.SetUserActive(ctx, db, pos[0], true); err != nil {
			fmt.Fprintf(os.Stderr, "enable user failed: %v\n", err)
			return 1
		}
		acc := domain.NormalizeAccount(pos[0])
		if *jsonFlag {
			_ = json.NewEncoder(os.Stdout).Encode(map[string]any{"account": acc, "active": true})
			return 0
		}
		fmt.Printf("Enabled account @%s\n", acc)
		return 0

	case "disable", "suspend":
		if len(pos) < 1 || strings.TrimSpace(pos[0]) == "" {
			fmt.Fprintln(os.Stderr, "usage: memorize user disable <account>")
			return 2
		}
		if err := sqlite.SetUserActive(ctx, db, pos[0], false); err != nil {
			fmt.Fprintf(os.Stderr, "disable user failed: %v\n", err)
			return 1
		}
		acc := domain.NormalizeAccount(pos[0])
		if *jsonFlag {
			_ = json.NewEncoder(os.Stdout).Encode(map[string]any{"account": acc, "active": false})
			return 0
		}
		fmt.Printf("Disabled account @%s\n", acc)
		return 0

	case "role":
		if len(pos) < 1 || strings.TrimSpace(pos[0]) == "" {
			fmt.Fprintln(os.Stderr, "usage: memorize user role <account> --role <member|admin>")
			return 2
		}
		roleArg := *roleFlag
		if len(pos) >= 2 {
			roleArg = pos[1]
		}
		if err := sqlite.UpdateUserRole(ctx, db, pos[0], roleArg); err != nil {
			fmt.Fprintf(os.Stderr, "update role failed: %v\n", err)
			return 1
		}
		acc := domain.NormalizeAccount(pos[0])
		if *jsonFlag {
			_ = json.NewEncoder(os.Stdout).Encode(map[string]any{"account": acc, "role": domain.NormalizeRole(roleArg)})
			return 0
		}
		fmt.Printf("Updated role for @%s -> %s\n", acc, domain.NormalizeRole(roleArg))
		return 0

	case "remove", "rm", "delete":
		if len(pos) < 1 || strings.TrimSpace(pos[0]) == "" {
			fmt.Fprintln(os.Stderr, "usage: memorize user remove <account>")
			return 2
		}
		if err := sqlite.DeleteUser(ctx, db, pos[0]); err != nil {
			fmt.Fprintf(os.Stderr, "remove user failed: %v\n", err)
			return 1
		}
		acc := domain.NormalizeAccount(pos[0])
		if *jsonFlag {
			_ = json.NewEncoder(os.Stdout).Encode(map[string]any{"account": acc, "removed": true})
			return 0
		}
		fmt.Printf("Removed account @%s\n", acc)
		return 0

	default:
		printUserCLIUsage()
		return 2
	}
}

func printIssuedToken(issued domain.UserTokenIssue, asJSON bool, verb string) int {
	if asJSON {
		_ = json.NewEncoder(os.Stdout).Encode(issued)
		return 0
	}
	fmt.Printf("%s: @%s (role=%s, hint=%s...)\n", verb, issued.User.Account, issued.User.Role, issued.User.TokenHint)
	fmt.Printf("Access Token (shown once — save now): %s\n", issued.PlainToken)
	fmt.Printf("Client connect command:\n  memorize connect --url http://<host>:18095 --token %s\n", issued.PlainToken)
	return 0
}

func printUserCLIUsage() {
	fmt.Fprintln(os.Stderr, `usage: memorize user <command> [args]
  add <account> [--role member|admin] [--json]
  list [--json]
  rotate <account> [--json]
  enable <account> [--json]
  disable <account> [--json]
  role <account> --role <member|admin> [--json]
  remove <account> [--json]`)
}

func resolveBindingRepoRoot(repoFlag string) string {
	dir := strings.TrimSpace(repoFlag)
	if dir == "" {
		wd, _ := os.Getwd()
		dir = wd
	}
	if dir == "" {
		return ""
	}
	if _, root := git.CanonicalScope(context.Background(), dir); root != "" {
		return root
	}
	if abs, err := filepath.Abs(dir); err == nil {
		return abs
	}
	return filepath.Clean(dir)
}

func runConnectCLI(args []string, dbPath string) int {
	fs := flag.NewFlagSet("connect", flag.ContinueOnError)
	urlFlag := fs.String("url", "", "team server URL (e.g. https://memorize.team:18095)")
	tokenFlag := fs.String("token", "", "team access token (mmz_...)")
	repoFlag := fs.String("repo", "", "local repository path to bind (default: current repository)")
	globalFlag := fs.Bool("global", false, "bind as default server for all repositories without per-repo overrides")
	skipVerify := fs.Bool("skip-verify", false, "save binding without calling remote /api/daemon/status")
	jsonFlag := fs.Bool("json", false, "output JSON")

	ordered := reorderFlagsBeforePositional(args, map[string]bool{
		"url": true, "token": true, "repo": true,
	})
	if err := fs.Parse(ordered); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}

	rawURL := domain.FirstNonEmpty(*urlFlag, os.Getenv("MEMORY_REMOTE_URL"))
	if rawURL == "" && len(fs.Args()) > 0 {
		rawURL = fs.Args()[0]
	}
	normURL := config.NormalizeRemoteURL(rawURL)
	tok := strings.TrimSpace(domain.FirstNonEmpty(*tokenFlag, os.Getenv("MEMORY_REMOTE_TOKEN")))
	if normURL == "" || tok == "" {
		fmt.Fprintln(os.Stderr, "usage: memorize connect --url <server-url> --token <token> [--global] [--repo <path>]")
		return 2
	}

	target := domain.RemoteTargetConfig{
		URL:   normURL,
		Token: tok,
	}
	if !*skipVerify {
		st, err := thttp.QueryDaemonStatusWithAuth(normURL, tok)
		if err != nil {
			fmt.Fprintf(os.Stderr, "failed to verify remote team server %s: %v\n", normURL, err)
			return 1
		}
		if st != nil {
			target.Account = st.Account
			target.Role = st.Role
		}
	}

	repoRoot := ""
	if !*globalFlag {
		repoRoot = resolveBindingRepoRoot(*repoFlag)
		if repoRoot == "" {
			fmt.Fprintln(os.Stderr, "cannot determine current directory; pass --repo or --global")
			return 1
		}
	}
	dataDir := resolveClientDataDir(dbPath)
	if err := config.SaveRemoteBinding(dataDir, repoRoot, *globalFlag, target); err != nil {
		fmt.Fprintf(os.Stderr, "save remote binding failed: %v\n", err)
		return 1
	}

	if *jsonFlag {
		_ = json.NewEncoder(os.Stdout).Encode(map[string]any{
			"connected": true,
			"url":       target.URL,
			"account":   target.Account,
			"role":      target.Role,
			"global":    *globalFlag,
			"repo_root": repoRoot,
			"file":      config.RemotesFilePath(dataDir),
		})
		return 0
	}

	scopeDesc := repoRoot
	if *globalFlag {
		scopeDesc = "global default"
	}
	if target.Account != "" {
		fmt.Printf("Connected @%s (%s) -> %s [%s]\n", target.Account, target.Role, target.URL, scopeDesc)
	} else {
		fmt.Printf("Connected -> %s [%s]\n", target.URL, scopeDesc)
	}
	return 0
}

func runDisconnectCLI(args []string, dbPath string) int {
	fs := flag.NewFlagSet("disconnect", flag.ContinueOnError)
	repoFlag := fs.String("repo", "", "local repository path to unbind")
	globalFlag := fs.Bool("global", false, "remove global default remote server binding")
	jsonFlag := fs.Bool("json", false, "output JSON")

	ordered := reorderFlagsBeforePositional(args, map[string]bool{"repo": true})
	if err := fs.Parse(ordered); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	repoRoot := ""
	if !*globalFlag {
		repoRoot = resolveBindingRepoRoot(*repoFlag)
	}
	dataDir := resolveClientDataDir(dbPath)
	removed, err := config.RemoveRemoteBinding(dataDir, repoRoot, *globalFlag)
	if err != nil {
		fmt.Fprintf(os.Stderr, "disconnect failed: %v\n", err)
		return 1
	}
	if *jsonFlag {
		_ = json.NewEncoder(os.Stdout).Encode(map[string]any{
			"disconnected": removed,
			"global":       *globalFlag,
			"repo_root":    repoRoot,
		})
		return 0
	}
	if !removed {
		fmt.Println("No matching remote binding found; using local solo mode.")
		return 0
	}
	if *globalFlag {
		fmt.Println("Removed global default remote binding.")
	} else {
		fmt.Printf("Disconnected %s (reverted to local solo mode).\n", repoRoot)
	}
	return 0
}
