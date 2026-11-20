package git_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"praxis/internal/infra/git"
)

func runGitCmd(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v failed: %v\n%s", args, err, string(out))
	}
}

func TestCanonicalScopeWorktreesAndOriginRemote(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git binary unavailable")
	}
	ctx := context.Background()
	baseDir := t.TempDir()
	mainRepo := filepath.Join(baseDir, "local-folder-a")
	if err := os.MkdirAll(mainRepo, 0o755); err != nil {
		t.Fatal(err)
	}
	runGitCmd(t, mainRepo, "init")
	runGitCmd(t, mainRepo, "config", "user.name", "Test")
	runGitCmd(t, mainRepo, "config", "user.email", "test@example.com")
	if err := os.WriteFile(filepath.Join(mainRepo, "README.md"), []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitCmd(t, mainRepo, "add", "README.md")
	runGitCmd(t, mainRepo, "commit", "-m", "init")

	wtDir := filepath.Join(baseDir, "wt-feature-xyz")
	runGitCmd(t, mainRepo, "worktree", "add", "-b", "feat/x", wtDir)

	scMain, rootMain := git.CanonicalScope(ctx, mainRepo)
	scWT, rootWT := git.CanonicalScope(ctx, wtDir)
	if scMain != "project:local-folder-a" || scWT != scMain {
		t.Fatalf("expected matching worktree canonical scope project:local-folder-a, got main=%q wt=%q", scMain, scWT)
	}
	if rootMain == "" || rootWT == "" {
		t.Fatalf("expected non-empty repo roots: main=%q wt=%q", rootMain, rootWT)
	}

	runGitCmd(t, mainRepo, "remote", "add", "origin", "git@github.com:acme-org/Team-Service.git")
	scRemoteMain, _ := git.CanonicalScope(ctx, mainRepo)
	scRemoteWT, _ := git.CanonicalScope(ctx, wtDir)
	if scRemoteMain != "project:team-service" || scRemoteWT != "project:team-service" {
		t.Fatalf("expected origin slug scope project:team-service, got main=%q wt=%q", scRemoteMain, scRemoteWT)
	}

	if err := os.WriteFile(filepath.Join(wtDir, "app.go"), []byte("package main\nfunc secret() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitCmd(t, wtDir, "add", "app.go")
	staged, err := git.StagedDiffFiles(ctx, wtDir)
	if err != nil || len(staged) != 1 {
		t.Fatalf("StagedDiffFiles: len=%d err=%v", len(staged), err)
	}
	if staged[0].Path != "app.go" || !strings.Contains(staged[0].Body, "secret") {
		t.Fatalf("unexpected staged diff entry: %+v", staged[0])
	}

	if id := git.AuthorIdentity(ctx, wtDir); id == "" {
		t.Fatalf("expected non-empty AuthorIdentity")
	}
	if br := git.CurrentBranch(ctx, wtDir); br != "feat/x" {
		t.Fatalf("expected CurrentBranch=feat/x, got %q", br)
	}
	if layout := git.DetectHeadLayout(wtDir); layout == "" {
		t.Fatalf("expected non-empty DetectHeadLayout")
	}
	if !git.IsPathDirty(ctx, wtDir, "app.go") {
		t.Fatalf("expected staged app.go to report dirty")
	}
	if git.FastHead(ctx, mainRepo) == "" || !git.HasOriginRemote(ctx, mainRepo) {
		t.Fatalf("expected FastHead and HasOriginRemote on mainRepo")
	}
	if blob, ok, err := git.ReadBlobAtRef(ctx, mainRepo, "HEAD", "README.md"); err != nil || !ok || string(blob) != "hello\n" {
		t.Fatalf("ReadBlobAtRef unexpected: ok=%v err=%v blob=%q", ok, err, string(blob))
	}
	if headSHA, commits := git.CollectPendingCommits(ctx, mainRepo, "", 5, nil); headSHA == "" || len(commits) != 1 || commits[0].Subject != "init" {
		t.Fatalf("CollectPendingCommits unexpected: head=%q commits=%+v", headSHA, commits)
	}
	if got := git.CleanCommitSubject("  feat(api): add healthz  "); got != "add healthz" {
		t.Fatalf("CleanCommitSubject(conventional) = %q", got)
	}
	if got := git.CleanCommitSubject("  plain commit  "); got != "plain commit" {
		t.Fatalf("CleanCommitSubject(plain) = %q", got)
	}
	parsed := git.ParseUnifiedDiffFiles("diff --git a/bin.dat b/bin.dat\nBinary files a/bin.dat and b/bin.dat differ\n")
	if len(parsed) != 1 || !parsed[0].Binary {
		t.Fatalf("expected binary diff entry, got %+v", parsed)
	}
}
