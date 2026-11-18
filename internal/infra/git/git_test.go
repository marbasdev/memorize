package git

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func runTestGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v in %s failed: %v\n%s", args, dir, err, string(out))
	}
	return strings.TrimSpace(string(out))
}

func initTestRepoWithOrigin(t *testing.T) (originDir, repoDir string) {
	t.Helper()
	tmp := t.TempDir()
	originDir = filepath.Join(tmp, "origin.git")
	repoDir = filepath.Join(tmp, "repo")

	runTestGit(t, tmp, "init", "--bare", originDir)
	runTestGit(t, tmp, "-c", "init.defaultBranch=main", "init", repoDir)
	runTestGit(t, repoDir, "config", "user.name", "Memorize Dev")
	runTestGit(t, repoDir, "config", "user.email", "dev@example.com")
	runTestGit(t, repoDir, "config", "github.user", "memorizedev")
	runTestGit(t, repoDir, "checkout", "-B", "main")
	runTestGit(t, repoDir, "remote", "add", "origin", originDir)

	if err := os.WriteFile(filepath.Join(repoDir, "go.mod"), []byte("module demo\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(repoDir, "cmd", "app"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repoDir, "cmd", "app", "main.go"), []byte("package main\nfunc main() {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runTestGit(t, repoDir, "add", ".")
	runTestGit(t, repoDir, "commit", "-m", "feat(core): initial setup")
	runTestGit(t, repoDir, "push", "-u", "origin", "main")
	return originDir, repoDir
}

func TestResolveRootFastHeadCommitsAndDiff(t *testing.T) {
	_, repo := initTestRepoWithOrigin(t)
	ctx := context.Background()

	nested := filepath.Join(repo, "cmd", "app")
	if got := ResolveRoot(nested); got != repo {
		t.Fatalf("ResolveRoot(%q) = %q, want %q", nested, got, repo)
	}
	if got := ResolveRoot(t.TempDir()); got != "" {
		t.Fatalf("expected empty root outside git repo, got %q", got)
	}

	head1 := FastHead(ctx, repo)
	rev1 := RevParse(ctx, repo, "HEAD")
	if head1 == "" || head1 != rev1 {
		t.Fatalf("FastHead=%q did not match RevParse=%q", head1, rev1)
	}
	if br := CurrentBranch(ctx, repo); br != "main" {
		t.Fatalf("CurrentBranch = %q, want main", br)
	}
	if tb := DefaultTargetBranch(ctx, repo); tb != "main" {
		t.Fatalf("DefaultTargetBranch = %q, want main", tb)
	}
	if id := AuthorIdentity(ctx, repo); id != "@memorizedev" {
		t.Fatalf("AuthorIdentity = %q, want @memorizedev", id)
	}

	layout := DetectHeadLayout(repo)
	if !strings.Contains(layout, "go.mod") || !strings.Contains(layout, "cmd/") {
		t.Fatalf("unexpected DetectHeadLayout:\n%s", layout)
	}

	// Second commit to exercise CollectPendingCommits and existing SHA filtering.
	if err := os.WriteFile(filepath.Join(repo, "cmd", "app", "main.go"), []byte("package main\nfunc main() { println(1) }\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runTestGit(t, repo, "add", "cmd/app/main.go")
	runTestGit(t, repo, "commit", "-m", "fix(app): print boot marker")

	newHead, pending := CollectPendingCommits(ctx, repo, head1, 10, nil)
	if newHead == "" || newHead == head1 || len(pending) != 1 {
		t.Fatalf("CollectPendingCommits = (head=%q, count=%d), want 1 commit", newHead, len(pending))
	}
	if got := CleanCommitSubject(pending[0].Subject); got != "print boot marker" {
		t.Fatalf("CleanCommitSubject(%q) = %q", pending[0].Subject, got)
	}
	if len(pending[0].SHA) >= 7 {
		_, filtered := CollectPendingCommits(ctx, repo, "", 10, map[string]bool{
			head1[:7]:          true,
			pending[0].SHA[:7]: true,
		})
		if len(filtered) != 0 {
			t.Fatalf("expected recorded SHAs filtered out, got %+v", filtered)
		}
	}

	// Uncommitted change appears in DiffFile without committing.
	if err := os.WriteFile(filepath.Join(repo, "cmd", "app", "main.go"), []byte("package main\nfunc main() { println(2) }\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	diff := DiffFile(ctx, repo, filepath.Join(repo, "cmd", "app", "main.go"))
	if !strings.Contains(diff, "println(2)") {
		t.Fatalf("expected uncommitted diff in DiffFile, got %q", diff)
	}
	if !IsPathDirty(ctx, repo, "cmd/app/main.go") {
		t.Fatalf("expected IsPathDirty == true for modified main.go")
	}
}

func TestIsolatedRulePackCommitAndProtectedBranchError(t *testing.T) {
	ctx := context.Background()

	// Repo without commits fails cleanly with ErrNoCommits.
	emptyDir := t.TempDir()
	emptyBare := filepath.Join(emptyDir, "bare.git")
	emptyWork := filepath.Join(emptyDir, "empty")
	runTestGit(t, emptyDir, "init", "--bare", emptyBare)
	runTestGit(t, emptyDir, "init", emptyWork)
	runTestGit(t, emptyWork, "remote", "add", "origin", emptyBare)
	if _, err := IsolatedRulePackCommit(ctx, RulePackGitOptions{Root: emptyWork}, func(b []byte) ([]byte, bool, error) {
		return []byte("# empty\n"), true, nil
	}); !errors.Is(err, ErrNoCommits) {
		t.Fatalf("expected ErrNoCommits on repo without HEAD, got %v", err)
	}

	originDir, repo := initTestRepoWithOrigin(t)
	// Leave workspace dirty to verify isolated worktree commit never includes workspace edits.
	if err := os.WriteFile(filepath.Join(repo, "go.mod"), []byte("module dirty_local_edit\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	res, err := IsolatedRulePackCommit(ctx, RulePackGitOptions{
		Root:          repo,
		CommitMessage: "memorize(rulepack): seed test rule",
	}, func(_ []byte) ([]byte, bool, error) {
		return []byte("# Team Rules\n\n- Use Go stdlib.\n"), true, nil
	})
	if err != nil {
		t.Fatalf("IsolatedRulePackCommit failed: %v", err)
	}
	if res.BranchPushed != "main" || res.CommitSHA == "" || res.NoChange {
		t.Fatalf("unexpected RulePackGitResult: %+v", res)
	}
	blob, ok, rerr := ReadBlobAtRef(ctx, repo, "refs/remotes/origin/main", ".memorize/TEAM_RULES.md")
	if rerr != nil || !ok || !strings.Contains(string(blob), "Use Go stdlib.") {
		t.Fatalf("expected TEAM_RULES.md on origin/main, ok=%v err=%v blob=%q", ok, rerr, string(blob))
	}

	// No-op build returns NoChange without pushing.
	noOp, err := IsolatedRulePackCommit(ctx, RulePackGitOptions{Root: repo}, func(b []byte) ([]byte, bool, error) {
		return b, false, nil
	})
	if err != nil || !noOp.NoChange {
		t.Fatalf("expected NoChange=true, got %+v err=%v", noOp, err)
	}

	// Simulated branch protection hook returns ErrBranchProtected.
	if runtime.GOOS != "windows" {
		hookPath := filepath.Join(originDir, "hooks", "pre-receive")
		_ = os.MkdirAll(filepath.Dir(hookPath), 0o755)
		hookBody := "#!/bin/sh\necho 'GH006: Protected branch update failed' >&2\nexit 1\n"
		if err := os.WriteFile(hookPath, []byte(hookBody), 0o755); err != nil {
			t.Fatal(err)
		}
		_, protErr := IsolatedRulePackCommit(ctx, RulePackGitOptions{Root: repo}, func(_ []byte) ([]byte, bool, error) {
			return []byte("# Team Rules\n\n- Updated rule.\n"), true, nil
		})
		if !errors.Is(protErr, ErrBranchProtected) {
			t.Fatalf("expected ErrBranchProtected, got %v", protErr)
		}
	}
}

func TestStagedDiffFilesCompactsWhitespaceAndBinary(t *testing.T) {
	_, repo := initTestRepoWithOrigin(t)
	ctx := context.Background()

	// Seed a baseline file for whitespace-only formatting change test.
	wsFile := filepath.Join(repo, "ws.go")
	if err := os.WriteFile(wsFile, []byte("package main\nfunc keep() int { return 1 }\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runTestGit(t, repo, "add", "ws.go")
	runTestGit(t, repo, "commit", "-m", "chore: seed ws.go")

	// Stage 1: whitespace-only change, 2: semantic + long minified line, 3: binary file.
	if err := os.WriteFile(wsFile, []byte("package main\n\n  func keep() int { return 1 }\n\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	longLine := "var blob = \"" + strings.Repeat("abcdefghij", 120) + "\""
	if err := os.WriteFile(filepath.Join(repo, "code.go"), []byte("package main\n"+longLine+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "bin.dat"), []byte{0x00, 0x01, 0x02, 0xff}, 0o600); err != nil {
		t.Fatal(err)
	}
	runTestGit(t, repo, "add", "ws.go", "code.go", "bin.dat")

	files, err := StagedDiffFiles(ctx, repo)
	if err != nil {
		t.Fatalf("StagedDiffFiles: %v", err)
	}
	byPath := make(map[string]StagedFileDiff, len(files))
	for _, f := range files {
		byPath[f.Path] = f
	}
	if f, ok := byPath["ws.go"]; !ok || f.Body != "(whitespace/formatting only)" {
		t.Fatalf("expected ws.go collapsed to whitespace-only summary, got %+v", byPath)
	}
	if f, ok := byPath["bin.dat"]; !ok || !f.Binary {
		t.Fatalf("expected bin.dat marked Binary=true, got %+v", byPath)
	}
	if f, ok := byPath["code.go"]; !ok || !strings.Contains(f.Body, "+var blob =") || len(f.Body) > 950 {
		t.Fatalf("expected code.go long line capped safely, got len=%d body=%q", len(f.Body), f.Body)
	}
}
