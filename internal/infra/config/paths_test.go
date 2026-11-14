package config_test

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"testing"

	"memorize/internal/domain"
	"memorize/internal/infra/config"
	"memorize/internal/infra/sqlite"
)

func TestPortConfigValidationPersistenceAndIsolation(t *testing.T) {
	// 1. In-memory DB never writes ports.json to disk and defaults cleanly.
	if p := config.PortsFilePath(":memory:"); p != "" {
		t.Fatalf("expected empty PortsFilePath for :memory:, got %q", p)
	}
	if err := config.SavePortConfig(":memory:", domain.PortConfig{DaemonPort: 19095, GGUFPort: 19088}); err != nil {
		t.Fatalf("SavePortConfig(:memory:) failed: %v", err)
	}
	if got := config.LoadPortConfig(":memory:"); got.DaemonPort != domain.DefaultDaemonPort || got.GGUFPort != domain.DefaultGGUFPort {
		t.Fatalf("expected default ports for :memory:, got %+v", got)
	}

	// 2. Reject invalid ranges and matching daemon/GGUF ports.
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

	// 3. Persist valid config with 0600 permissions and reload cleanly.
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

	// Older PID must NOT erase a newer PID's daemon.json during restart handover.
	config.RemoveDaemonStateIfPID(dbPath, 9999)
	if _, stillOK := config.ReadDaemonState(dbPath); !stillOK {
		t.Fatalf("expected daemon.json to remain when mismatched PID attempts cleanup")
	}
	config.RemoveDaemonStateIfPID(dbPath, 1111)
	if _, gone := config.ReadDaemonState(dbPath); gone {
		t.Fatalf("expected matching PID cleanup to remove daemon.json")
	}

	// Occupy a loopback port and exclude the next consecutive port to verify fallback + exclusion.
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

	// Non-loopback external endpoint must NOT be overwritten when managed GGUF port changes.
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
	t.Setenv("MEMORY_DATA_DIR", "")
	t.Setenv("MEMORY_DB_PATH", "")

	wantDefault := filepath.Join(home, ".memorize")
	if got := config.DefaultDataDir(); got != wantDefault {
		t.Fatalf("DefaultDataDir() = %q, want %q", got, wantDefault)
	}

	customDB := filepath.Join(home, "custom-db-dir", "memories.db")
	t.Setenv("MEMORY_DB_PATH", customDB)
	if got := config.DefaultDataDir(); got != filepath.Join(home, "custom-db-dir") {
		t.Fatalf("DefaultDataDir() with MEMORY_DB_PATH = %q, want %q", got, filepath.Join(home, "custom-db-dir"))
	}

	customRoot := filepath.Join(home, "explicit-data-dir")
	t.Setenv("MEMORY_DATA_DIR", customRoot)
	if got := config.DefaultDataDir(); got != customRoot {
		t.Fatalf("DefaultDataDir() with MEMORY_DATA_DIR = %q, want %q", got, customRoot)
	}
}
