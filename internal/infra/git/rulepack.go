package git

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"praxis/internal/domain"
)

var (
	ErrPushNonFastForward = errors.New("git push rejected: non-fast-forward (remote advanced)")
	ErrBranchProtected    = errors.New("git push blocked by branch protection rules")
	// ErrNoCommits indicates repository has no initial commit yet to branch or worktree from.
	ErrNoCommits = errors.New("git repository has no commits yet")
)

func runSafe(parent context.Context, timeout time.Duration, dir string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "LC_ALL=C")
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	err := cmd.Run()
	trimmed := strings.TrimSpace(out.String())
	if err != nil {
		if trimmed != "" {
			return trimmed, fmt.Errorf("%w: %s", err, trimmed)
		}
		return "", err
	}
	return trimmed, nil
}

func HasOriginRemote(ctx context.Context, root string) bool {
	if root == "" {
		return false
	}
	out, err := runSafe(ctx, 2*time.Second, root, "remote", "get-url", "origin")
	return err == nil && strings.TrimSpace(out) != ""
}

func runGitConfig(ctx context.Context, root, key string) (string, error) {
	cCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	var args []string
	if strings.TrimSpace(root) != "" {
		args = []string{"-C", root, "config", "--get", key}
	} else {
		args = []string{"config", "--global", "--get", key}
	}
	cmd := exec.CommandContext(cCtx, "git", args...)
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "LC_ALL=C")
	out, err := cmd.Output()
	trimmed := strings.TrimSpace(string(out))
	if err != nil {
		return "", err
	}
	return trimmed, nil
}

func AuthorIdentity(ctx context.Context, root string) string {
	if out, err := runGitConfig(ctx, root, "github.user"); err == nil {
		if h := domain.NormalizeGitHandle(out); h != "" {
			return h
		}
	}
	email, _ := runGitConfig(ctx, root, "user.email")
	email = strings.TrimSpace(email)
	if at := strings.IndexByte(email, '@'); at > 0 {
		local := email[:at]
		host := strings.ToLower(strings.TrimSpace(email[at+1:]))
		if strings.HasSuffix(host, "noreply.github.com") {
			if plus := strings.IndexByte(local, '+'); plus >= 0 && plus+1 < len(local) {
				local = local[plus+1:]
			}
			if h := domain.NormalizeGitHandle(local); h != "" {
				return h
			}
		}
	}
	if out, err := runGitConfig(ctx, root, "user.name"); err == nil {
		if h := domain.NormalizeGitHandle(out); h != "" {
			return h
		}
	}
	if at := strings.IndexByte(email, '@'); at > 0 {
		if h := domain.NormalizeGitHandle(email[:at]); h != "" {
			return h
		}
	}
	return ""
}

func CurrentBranch(ctx context.Context, root string) string {
	if root == "" {
		return ""
	}
	out, err := runSafe(ctx, 2*time.Second, root, "symbolic-ref", "--short", "HEAD")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(out)
}

func DefaultTargetBranch(ctx context.Context, root string) string {
	if root == "" {
		return ""
	}
	if out, err := runSafe(ctx, 2*time.Second, root, "symbolic-ref", "--short", "refs/remotes/origin/HEAD"); err == nil {
		b := strings.TrimPrefix(strings.TrimSpace(out), "origin/")
		if b != "" && b != "HEAD" {
			return b
		}
	}
	for _, cand := range []string{"refs/remotes/origin/main", "refs/remotes/origin/master", "refs/heads/main", "refs/heads/master"} {
		if _, err := runSafe(ctx, 2*time.Second, root, "rev-parse", "--verify", "--quiet", cand); err == nil {
			idx := strings.LastIndex(cand, "/")
			return cand[idx+1:]
		}
	}
	if cur := CurrentBranch(ctx, root); cur != "" {
		return cur
	}
	return "main"
}

func RemoteRefFastSHA(root, remote, branch string) string {
	if root == "" || remote == "" || branch == "" {
		return ""
	}
	refPath := filepath.Join(root, ".git", "refs", "remotes", remote, filepath.FromSlash(branch))
	if raw, err := os.ReadFile(refPath); err == nil {
		s := strings.TrimSpace(string(raw))
		if len(s) >= 12 {
			return s[:12]
		}
	}
	packed, err := os.ReadFile(filepath.Join(root, ".git", "packed-refs"))
	if err != nil {
		return ""
	}
	targetRef := "refs/remotes/" + remote + "/" + branch
	for _, line := range strings.Split(string(packed), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "^") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) == 2 && fields[1] == targetRef && len(fields[0]) >= 12 {
			return fields[0][:12]
		}
	}
	return ""
}

func FetchBranch(ctx context.Context, root, remote, branch string) error {
	if root == "" || remote == "" || branch == "" {
		return nil
	}
	_, err := runSafe(ctx, 10*time.Second, root, "fetch", "--no-tags", "--quiet", remote, branch)
	return err
}

func ReadBlobAtRef(ctx context.Context, root, ref, relPath string) ([]byte, bool, error) {
	if root == "" || ref == "" || relPath == "" {
		return nil, false, nil
	}
	objSpec := ref + ":" + filepath.ToSlash(relPath)
	if _, err := runSafe(ctx, 2*time.Second, root, "cat-file", "-e", objSpec); err != nil {
		return nil, false, nil
	}
	cCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	cmd := exec.CommandContext(cCtx, "git", "-C", root, "show", objSpec)
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "LC_ALL=C")
	out, err := cmd.Output()
	if err != nil {
		return nil, false, err
	}
	return out, true, nil
}

func IsPathDirty(ctx context.Context, root, relPath string) bool {
	if root == "" || relPath == "" {
		return false
	}
	out, err := runSafe(ctx, 2*time.Second, root, "status", "--porcelain", "--", filepath.ToSlash(relPath))
	return err == nil && strings.TrimSpace(out) != ""
}

func AheadCount(ctx context.Context, root, localBranch, upstreamRef string) int {
	if root == "" || localBranch == "" || upstreamRef == "" {
		return 0
	}
	out, err := runSafe(ctx, 2*time.Second, root, "rev-list", "--count", upstreamRef+".."+localBranch)
	if err != nil {
		return 0
	}
	n, _ := strconv.Atoi(strings.TrimSpace(out))
	return n
}

func classifyPushError(err error) error {
	if err == nil {
		return nil
	}
	lower := strings.ToLower(err.Error())
	switch {
	case strings.Contains(lower, "protected branch"),
		strings.Contains(lower, "gh006"),
		strings.Contains(lower, "cannot push to protected"):
		return fmt.Errorf("%w (%v)", ErrBranchProtected, err)
	case strings.Contains(lower, "non-fast-forward"),
		strings.Contains(lower, "fetch first"),
		strings.Contains(lower, "cannot lock ref"),
		strings.Contains(lower, "updates were rejected because the remote contains work"):
		return fmt.Errorf("%w (%v)", ErrPushNonFastForward, err)
	case strings.Contains(lower, "pre-receive hook declined"):
		return fmt.Errorf("%w (%v)", ErrBranchProtected, err)
	default:
		return err
	}
}

type RulePackGitOptions struct {
	Root          string
	TargetBranch  string
	CommitMessage string
	AuthorName    string
	AuthorEmail   string
}

type RulePackGitResult struct {
	TargetBranch    string
	BranchPushed    string
	CommitSHA       string
	WrittenBytes    []byte
	WorkspaceSynced bool
	NoChange        bool
}

// IsolatedRulePackCommit executes buildFn inside a clean temporary git worktree (never touching workspace staged/dirty code),
// commits only .praxis/TEAM_RULES.md, pushes directly to TargetBranch, and
// fast-forwards the checked-out workspace only when safely on TargetBranch with no unpushed code commits.
func IsolatedRulePackCommit(ctx context.Context, opts RulePackGitOptions, buildFn func(baseRaw []byte) ([]byte, bool, error)) (RulePackGitResult, error) {
	root := ResolveRoot(opts.Root)
	if root == "" {
		return RulePackGitResult{}, errors.New("not inside a Git repository")
	}
	relPath := filepath.ToSlash(domain.RulePackRelPath)

	targetBranch := strings.TrimSpace(opts.TargetBranch)
	if targetBranch == "" {
		targetBranch = DefaultTargetBranch(ctx, root)
	}

	if !HasOriginRemote(ctx, root) {
		return RulePackGitResult{}, errors.New("team rules require an 'origin' remote for push")
	}

	baseRef := ""
	if err := FetchBranch(ctx, root, "origin", targetBranch); err == nil {
		if _, verifyErr := runSafe(ctx, 2*time.Second, root, "rev-parse", "--verify", "--quiet", "refs/remotes/origin/"+targetBranch); verifyErr == nil {
			baseRef = "refs/remotes/origin/" + targetBranch
		}
	}
	if baseRef == "" {
		if head := FastHead(ctx, root); head != "" {
			baseRef = "HEAD"
		} else {
			return RulePackGitResult{}, ErrNoCommits
		}
	}

	tmpDir, err := os.MkdirTemp("", "praxis-rulepack-*")
	if err != nil {
		return RulePackGitResult{}, err
	}
	defer func() {
		_, _ = runSafe(context.Background(), 5*time.Second, root, "worktree", "remove", "--force", tmpDir)
		_ = os.RemoveAll(tmpDir)
		_, _ = runSafe(context.Background(), 3*time.Second, root, "worktree", "prune")
	}()

	if _, err := runSafe(ctx, 10*time.Second, root, "worktree", "add", "--detach", tmpDir, baseRef); err != nil {
		return RulePackGitResult{}, fmt.Errorf("create isolated worktree: %w", err)
	}

	tmpFile := filepath.Join(tmpDir, filepath.FromSlash(relPath))
	var baseBytes []byte
	if raw, err := os.ReadFile(tmpFile); err == nil {
		baseBytes = raw
	}

	updatedBytes, changed, err := buildFn(baseBytes)
	if err != nil {
		return RulePackGitResult{}, err
	}
	if !changed {
		return RulePackGitResult{
			TargetBranch: targetBranch,
			WrittenBytes: baseBytes,
			NoChange:     true,
		}, nil
	}

	if err := os.MkdirAll(filepath.Dir(tmpFile), 0o755); err != nil {
		return RulePackGitResult{}, err
	}
	if err := os.WriteFile(tmpFile, updatedBytes, 0o644); err != nil {
		return RulePackGitResult{}, err
	}
	if _, err := runSafe(ctx, 5*time.Second, tmpDir, "add", "--", relPath); err != nil {
		return RulePackGitResult{}, fmt.Errorf("stage %s in isolated worktree: %w", relPath, err)
	}

	commitMsg := strings.TrimSpace(opts.CommitMessage)
	if commitMsg == "" {
		commitMsg = "praxis(rulepack): update team rules"
	}
	commitArgs := []string{"commit", "--only", "-m", commitMsg, "--", relPath}
	if _, err := runSafe(ctx, 5*time.Second, tmpDir, commitArgs...); err != nil {
		return RulePackGitResult{}, fmt.Errorf("commit isolated rulepack: %w", err)
	}
	commitSHA := RevParse(ctx, tmpDir, "HEAD")

	res := RulePackGitResult{
		TargetBranch: targetBranch,
		CommitSHA:    commitSHA,
		WrittenBytes: updatedBytes,
	}

	if _, err := runSafe(ctx, 15*time.Second, tmpDir, "push", "origin", "HEAD:refs/heads/"+targetBranch); err != nil {
		return RulePackGitResult{}, classifyPushError(err)
	}
	res.BranchPushed = targetBranch

	curBranch := CurrentBranch(ctx, root)
	if curBranch == targetBranch &&
		!IsPathDirty(ctx, root, relPath) &&
		AheadCount(ctx, root, curBranch, "refs/remotes/origin/"+targetBranch) == 0 {
		_ = FetchBranch(ctx, root, "origin", targetBranch)
		if _, err := runSafe(ctx, 5*time.Second, root, "merge", "--ff-only", "refs/remotes/origin/"+targetBranch); err == nil {
			res.WorkspaceSynced = true
		}
	}
	return res, nil
}
