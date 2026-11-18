package config_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"memorize/internal/domain"
	"memorize/internal/infra/config"
)

func TestRemoteBindingsRepoVsGlobalAndSoloMode(t *testing.T) {
	dataDir := t.TempDir()
	teamRepo := t.TempDir()
	personalRepo := t.TempDir()

	// Solo mode (unbound): must return false so local daemon stays intact.
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

	// Sibling personal repo remains unbound when no global default exists.
	if _, ok := config.ResolveRemoteTarget(dataDir, personalRepo); ok {
		t.Fatalf("expected unbound personal repo to remain local")
	}

	// Global default applies when no per-repo override matches; repo override wins.
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

	// Disconnect repo and global cleanly.
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

	// Baseline captured before edit; subsequent baseline calls for same file preserve original pre-edit snapshot.
	config.RecordClientTurnBaseline(dataDir, sid, "project:app", fileA, "main.go")
	if err := os.WriteFile(fileA, []byte("package main\nfunc run() { println(\"hi\") }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	config.RecordClientTurnBaseline(dataDir, sid, "project:app", fileA, "main.go")

	// Also test newly created file (did not exist at baseline time).
	newFile := filepath.Join(workDir, "helper.go")
	config.RecordClientTurnBaseline(dataDir, sid, "project:app", newFile, "helper.go")
	if err := os.WriteFile(newFile, []byte("package main\nvar x = 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Without clear=true, diffs remain available (for bounded Stop re-verify).
	diffs := config.ConsumeClientTurnDiffs(dataDir, sid, false)
	if len(diffs) != 2 {
		t.Fatalf("expected 2 client diff sections, got %d (%+v)", len(diffs), diffs)
	}
	joined := diffs[0].Body + "\n" + diffs[1].Body
	if !strings.Contains(joined, "println") || !strings.Contains(joined, "var x = 1") {
		t.Fatalf("unexpected diff bodies: %s", joined)
	}

	// clear=true deletes baselines after returning diffs.
	diffs2 := config.ConsumeClientTurnDiffs(dataDir, sid, true)
	if len(diffs2) != 2 {
		t.Fatalf("expected diffs on clear=true consume, got %d", len(diffs2))
	}
	if rem := config.ConsumeClientTurnDiffs(dataDir, sid, false); len(rem) != 0 {
		t.Fatalf("expected cleared baselines, got %d", len(rem))
	}
}
