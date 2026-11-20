package config_test

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"praxis/internal/domain"
	"praxis/internal/infra/config"
	"praxis/internal/infra/sqlite"
)

func TestPortConfigValidationPersistenceAndIsolation(t *testing.T) {
	if p := config.PortsFilePath(":memory:"); p != "" {
		t.Fatalf("expected empty PortsFilePath for :memory:, got %q", p)
	}
	if err := config.SavePortConfig(":memory:", domain.PortConfig{DaemonPort: 19095, GGUFPort: 19088}); err != nil {
		t.Fatalf("SavePortConfig(:memory:) failed: %v", err)
	}
	if got := config.LoadPortConfig(":memory:"); got.DaemonPort != domain.DefaultDaemonPort || got.GGUFPort != domain.DefaultGGUFPort {
		t.Fatalf("expected default ports for :memory:, got %+v", got)
	}

	tmp := t.TempDir()
	dbPath := filepath.Join(tmp, "memories.db")
	for _, tc := range [][2]int{
		{0, 18088},
		{18095, 70000},
		{-1, 18088},
		{19000, 19000},
	} {
		if err := domain.ValidatePortConfig(tc[0], tc[1]); err == nil {
			t.Fatalf("expected ValidatePortConfig(%d, %d) to fail", tc[0], tc[1])
		}
	}

	want := domain.PortConfig{DaemonPort: 19195, GGUFPort: 19188}
	if err := config.SavePortConfig(dbPath, want); err != nil {
		t.Fatalf("SavePortConfig failed: %v", err)
	}
	info, err := os.Stat(config.PortsFilePath(dbPath))
	if err != nil || (runtime.GOOS != "windows" && info.Mode().Perm() != 0o600) {
		t.Fatalf("expected ports.json perm 0600, got info=%v err=%v", info, err)
	}
	if got := config.LoadPortConfig(dbPath); got != want {
		t.Fatalf("LoadPortConfig mismatch: got %+v want %+v", got, want)
	}
}

func TestDaemonRuntimeStatePIDGuardAndFallbackPortPicker(t *testing.T) {
	tmp := t.TempDir()
	dbPath := filepath.Join(tmp, "memories.db")

	st := domain.DaemonRuntimeState{
		PID:        1111,
		ListenAddr: "127.0.0.1:19501",
		DBPath:     dbPath,
		GGUFPort:   18088,
	}
	if err := config.WriteDaemonState(dbPath, st); err != nil {
		t.Fatalf("WriteDaemonState failed: %v", err)
	}
	got, ok := config.ReadDaemonState(dbPath)
	if !ok || got.ListenAddr != "127.0.0.1:19501" || got.PID != 1111 {
		t.Fatalf("ReadDaemonState mismatch: ok=%v got=%+v", ok, got)
	}
	if u := config.ActiveDaemonURL(dbPath); u != "http://127.0.0.1:19501" {
		t.Fatalf("expected ActiveDaemonURL http://127.0.0.1:19501, got %q", u)
	}

	config.RemoveDaemonStateIfPID(dbPath, 9999)
	if _, stillOK := config.ReadDaemonState(dbPath); !stillOK {
		t.Fatalf("expected daemon.json to remain when mismatched PID attempts cleanup")
	}
	config.RemoveDaemonStateIfPID(dbPath, 1111)
	if _, gone := config.ReadDaemonState(dbPath); gone {
		t.Fatalf("expected matching PID cleanup to remove daemon.json")
	}

	busyLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen busy port: %v", err)
	}
	defer busyLn.Close()
	busyPort := busyLn.Addr().(*net.TCPAddr).Port
	excluded := busyPort + 1

	chosen, isFB, err := config.PickAvailableLoopbackPort("127.0.0.1", busyPort, excluded)
	if err != nil {
		t.Fatalf("PickAvailableLoopbackPort failed: %v", err)
	}
	if !isFB || chosen == busyPort || chosen == excluded {
		t.Fatalf("expected fallback port distinct from busy (%d) and excluded (%d), got chosen=%d fb=%v", busyPort, excluded, chosen, isFB)
	}
	verifyLn, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(chosen)))
	if err != nil {
		t.Fatalf("expected chosen fallback port %d to be bindable: %v", chosen, err)
	}
	_ = verifyLn.Close()
}

func TestSyncManagedGGUFEndpointRespectsExternalURLAndEnv(t *testing.T) {
	t.Setenv("LOCAL_GGUF_URL", "")
	db, err := sqlite.Open(":memory:")
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	defer db.Close()

	ctx := context.Background()
	if err := config.SyncManagedGGUFEndpoint(ctx, db, ":memory:", "http://127.0.0.1:19088"); err != nil {
		t.Fatalf("SyncManagedGGUFEndpoint (empty): %v", err)
	}
	if got := config.GetConfigMap(ctx, db)["gguf_endpoint"]; got != "http://127.0.0.1:19088" {
		t.Fatalf("expected gguf_endpoint synced to http://127.0.0.1:19088, got %q", got)
	}

	extURL := "https://gpu.internal.example:8080"
	if err := config.SaveConfig(domain.WithAdminBypass(ctx), db, ":memory:", map[string]string{"gguf_endpoint": extURL}); err != nil {
		t.Fatalf("SaveConfig external URL: %v", err)
	}
	if err := config.SyncManagedGGUFEndpoint(ctx, db, ":memory:", "http://127.0.0.1:19099"); err != nil {
		t.Fatalf("SyncManagedGGUFEndpoint (external): %v", err)
	}
	if got := config.GetConfigMap(ctx, db)["gguf_endpoint"]; got != extURL {
		t.Fatalf("expected external gguf_endpoint %q to remain untouched, got %q", extURL, got)
	}
}

func TestDefaultDataDirUsesNeutralHomeDirectory(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("PRAXIS_DATA_DIR", "")
	t.Setenv("PRAXIS_DB_PATH", "")

	wantDefault := filepath.Join(home, ".praxis")
	if got := config.DefaultDataDir(); got != wantDefault {
		t.Fatalf("DefaultDataDir() = %q, want %q", got, wantDefault)
	}

	customDB := filepath.Join(home, "custom-db-dir", "memories.db")
	t.Setenv("PRAXIS_DB_PATH", customDB)
	if got := config.DefaultDataDir(); got != filepath.Join(home, "custom-db-dir") {
		t.Fatalf("DefaultDataDir() with PRAXIS_DB_PATH = %q, want %q", got, filepath.Join(home, "custom-db-dir"))
	}

	customRoot := filepath.Join(home, "explicit-data-dir")
	t.Setenv("PRAXIS_DATA_DIR", customRoot)
	if got := config.DefaultDataDir(); got != customRoot {
		t.Fatalf("DefaultDataDir() with PRAXIS_DATA_DIR = %q, want %q", got, customRoot)
	}
}

func TestRemoteBindingsRepoVsGlobalAndSoloMode(t *testing.T) {
	dataDir := t.TempDir()
	teamRepo := t.TempDir()
	personalRepo := t.TempDir()

	if _, ok := config.ResolveRemoteTarget(dataDir, personalRepo); ok {
		t.Fatalf("expected unbound repo to stay in solo mode")
	}

	if err := config.SaveRemoteBinding(dataDir, teamRepo, false, domain.RemoteTargetConfig{
		URL:   "https://team.example.com:18095/",
		Token: "mmz_team_tok",
	}); err != nil {
		t.Fatalf("SaveRemoteBinding repo: %v", err)
	}

	subDir := filepath.Join(teamRepo, "services", "api")
	if err := os.MkdirAll(subDir, 0o755); err != nil {
		t.Fatal(err)
	}
	got, ok := config.ResolveRemoteTarget(dataDir, subDir)
	if !ok {
		t.Fatalf("expected nested dir inside bound repo to resolve remote target")
	}
	if got.URL != "https://team.example.com:18095" || got.Token != "mmz_team_tok" {
		t.Fatalf("unexpected resolved target: %+v", got)
	}

	if _, ok := config.ResolveRemoteTarget(dataDir, personalRepo); ok {
		t.Fatalf("expected unbound personal repo to remain local")
	}

	if err := config.SaveRemoteBinding(dataDir, "", true, domain.RemoteTargetConfig{
		URL:   "http://default.example.com:18095",
		Token: "mmz_default",
	}); err != nil {
		t.Fatalf("SaveRemoteBinding global: %v", err)
	}
	gotDef, ok := config.ResolveRemoteTarget(dataDir, personalRepo)
	if !ok || gotDef.Token != "mmz_default" {
		t.Fatalf("expected global default fallback, got %+v (ok=%v)", gotDef, ok)
	}
	gotTeam, ok := config.ResolveRemoteTarget(dataDir, teamRepo)
	if !ok || gotTeam.Token != "mmz_team_tok" {
		t.Fatalf("expected per-repo binding to win over global default, got %+v", gotTeam)
	}

	removed, err := config.RemoveRemoteBinding(dataDir, teamRepo, false)
	if err != nil || !removed {
		t.Fatalf("RemoveRemoteBinding repo failed: removed=%v err=%v", removed, err)
	}
	removed, err = config.RemoveRemoteBinding(dataDir, "", true)
	if err != nil || !removed {
		t.Fatalf("RemoveRemoteBinding global failed: removed=%v err=%v", removed, err)
	}
	if _, ok := config.ResolveRemoteTarget(dataDir, teamRepo); ok {
		t.Fatalf("expected all remote bindings disconnected")
	}
}

func TestClientTurnBaselinesAndDiffSections(t *testing.T) {
	dataDir := t.TempDir()
	workDir := t.TempDir()
	sid := "sess-diff-1"
	fileA := filepath.Join(workDir, "main.go")
	if err := os.WriteFile(fileA, []byte("package main\nfunc run() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	config.RecordClientTurnBaseline(dataDir, sid, "project:app", fileA, "main.go")
	if err := os.WriteFile(fileA, []byte("package main\nfunc run() { println(\"hi\") }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	config.RecordClientTurnBaseline(dataDir, sid, "project:app", fileA, "main.go")

	newFile := filepath.Join(workDir, "helper.go")
	config.RecordClientTurnBaseline(dataDir, sid, "project:app", newFile, "helper.go")
	if err := os.WriteFile(newFile, []byte("package main\nvar x = 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	diffs := config.ConsumeClientTurnDiffs(dataDir, sid, false)
	if len(diffs) != 2 {
		t.Fatalf("expected 2 client diff sections, got %d (%+v)", len(diffs), diffs)
	}
	joined := diffs[0].Body + "\n" + diffs[1].Body
	if !strings.Contains(joined, "println") || !strings.Contains(joined, "var x = 1") {
		t.Fatalf("unexpected diff bodies: %s", joined)
	}

	diffs2 := config.ConsumeClientTurnDiffs(dataDir, sid, true)
	if len(diffs2) != 2 {
		t.Fatalf("expected diffs on clear=true consume, got %d", len(diffs2))
	}
	if rem := config.ConsumeClientTurnDiffs(dataDir, sid, false); len(rem) != 0 {
		t.Fatalf("expected cleared baselines, got %d", len(rem))
	}
}

func TestRemoteProfilesLookupAndRemove(t *testing.T) {
	dataDir := t.TempDir()
	if err := config.SaveRemoteBinding(dataDir, "", true, domain.RemoteTargetConfig{
		URL: "https://home.example:18095", Token: "prx_home_tok", Account: "Home", Role: "admin",
	}); err != nil {
		t.Fatal(err)
	}
	if err := config.SaveRemoteBinding(dataDir, "", true, domain.RemoteTargetConfig{
		URL: "https://home.example:18095", Token: "prx_work_tok", Account: "work", Role: "member",
	}); err != nil {
		t.Fatal(err)
	}

	pHome, ok := config.LookupRemoteProfile(dataDir, "home")
	if !ok || pHome.Token != "prx_home_tok" || pHome.Account != "home" {
		t.Fatalf("LookupRemoteProfile(home) = %+v ok=%v", pHome, ok)
	}
	pWork, ok := config.LookupRemoteProfile(dataDir, "WORK")
	if !ok || pWork.Token != "prx_work_tok" {
		t.Fatalf("LookupRemoteProfile(WORK) = %+v ok=%v", pWork, ok)
	}
	if removed, err := config.RemoveRemoteProfile(dataDir, "home"); err != nil || !removed {
		t.Fatalf("RemoveRemoteProfile(home) = %v %v", removed, err)
	}
	if _, ok := config.LookupRemoteProfile(dataDir, "home"); ok {
		t.Fatalf("expected removed profile home not found")
	}
}
