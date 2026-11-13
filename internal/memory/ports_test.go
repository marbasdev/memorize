package memory

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"testing"
)

func TestPortConfigValidationPersistenceAndIsolation(t *testing.T) {
	// 1. In-memory DB never writes ports.json to disk and defaults cleanly.
	if p := PortsFilePath(":memory:"); p != "" {
		t.Fatalf("expected empty PortsFilePath for :memory:, got %q", p)
	}
	if err := SavePortConfig(":memory:", PortConfig{DaemonPort: 19095, GGUFPort: 19088}); err != nil {
		t.Fatalf("SavePortConfig(:memory:) failed: %v", err)
	}
	if got := LoadPortConfig(":memory:"); got.DaemonPort != DefaultDaemonPort || got.GGUFPort != DefaultGGUFPort {
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
		if err := ValidatePortConfig(tc[0], tc[1]); err == nil {
			t.Fatalf("expected ValidatePortConfig(%d, %d) to fail", tc[0], tc[1])
		}
	}

	// 3. Persist valid config with 0600 permissions and reload cleanly.
	want := PortConfig{DaemonPort: 19195, GGUFPort: 19188}
	if err := SavePortConfig(dbPath, want); err != nil {
		t.Fatalf("SavePortConfig failed: %v", err)
	}
	info, err := os.Stat(PortsFilePath(dbPath))
	if err != nil || (runtime.GOOS != "windows" && info.Mode().Perm() != 0o600) {
		t.Fatalf("expected ports.json perm 0600, got info=%v err=%v", info, err)
	}
	if got := LoadPortConfig(dbPath); got != want {
		t.Fatalf("LoadPortConfig mismatch: got %+v want %+v", got, want)
	}
}

func TestDaemonRuntimeStatePIDGuardAndFallbackPortPicker(t *testing.T) {
	tmp := t.TempDir()
	dbPath := filepath.Join(tmp, "memories.db")

	st := DaemonRuntimeState{
		PID:        1111,
		ListenAddr: "127.0.0.1:19501",
		DBPath:     dbPath,
		GGUFPort:   18088,
	}
	if err := WriteDaemonState(dbPath, st); err != nil {
		t.Fatalf("WriteDaemonState failed: %v", err)
	}
	got, ok := ReadDaemonState(dbPath)
	if !ok || got.ListenAddr != "127.0.0.1:19501" || got.PID != 1111 {
		t.Fatalf("ReadDaemonState mismatch: ok=%v got=%+v", ok, got)
	}
	if u := ActiveDaemonURL(dbPath); u != "http://127.0.0.1:19501" {
		t.Fatalf("expected ActiveDaemonURL http://127.0.0.1:19501, got %q", u)
	}

	// Older PID must NOT erase a newer PID's daemon.json during restart handover.
	RemoveDaemonStateIfPID(dbPath, 9999)
	if _, stillOK := ReadDaemonState(dbPath); !stillOK {
		t.Fatalf("expected daemon.json to remain when mismatched PID attempts cleanup")
	}
	RemoveDaemonStateIfPID(dbPath, 1111)
	if _, gone := ReadDaemonState(dbPath); gone {
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

	chosen, isFB, err := PickAvailableLoopbackPort("127.0.0.1", busyPort, excluded)
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
	svc, err := NewService(":memory:", nil)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	defer svc.Close()

	ctx := context.Background()
	if err := SyncManagedGGUFEndpoint(ctx, svc, "http://127.0.0.1:19088"); err != nil {
		t.Fatalf("SyncManagedGGUFEndpoint (empty): %v", err)
	}
	if got := svc.GetConfigMap(ctx)["gguf_endpoint"]; got != "http://127.0.0.1:19088" {
		t.Fatalf("expected gguf_endpoint synced to http://127.0.0.1:19088, got %q", got)
	}

	// Non-loopback external endpoint must NOT be overwritten when managed GGUF port changes.
	extURL := "https://gpu.internal.example:8080"
	if err := svc.SaveConfig(WithAdminBypass(ctx), map[string]string{"gguf_endpoint": extURL}); err != nil {
		t.Fatalf("SaveConfig external URL: %v", err)
	}
	if err := SyncManagedGGUFEndpoint(ctx, svc, "http://127.0.0.1:19099"); err != nil {
		t.Fatalf("SyncManagedGGUFEndpoint (external): %v", err)
	}
	if got := svc.GetConfigMap(ctx)["gguf_endpoint"]; got != extURL {
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
	if got := DefaultDataDir(); got != wantDefault {
		t.Fatalf("DefaultDataDir() = %q, want %q", got, wantDefault)
	}

	customDB := filepath.Join(home, "custom-db-dir", "memories.db")
	t.Setenv("MEMORY_DB_PATH", customDB)
	if got := DefaultDataDir(); got != filepath.Join(home, "custom-db-dir") {
		t.Fatalf("DefaultDataDir() with MEMORY_DB_PATH = %q, want %q", got, filepath.Join(home, "custom-db-dir"))
	}

	customRoot := filepath.Join(home, "explicit-data-dir")
	t.Setenv("MEMORY_DATA_DIR", customRoot)
	if got := DefaultDataDir(); got != customRoot {
		t.Fatalf("DefaultDataDir() with MEMORY_DATA_DIR = %q, want %q", got, customRoot)
	}
}
