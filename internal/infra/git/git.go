package git

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"memorize/internal/domain"
)

// BaselineManifests lists project metadata files scanned for repository onboarding baselines.
var BaselineManifests = []string{
	// Code/build manifests & CONSTRAINTS.md only; static AGENTS.md/CLAUDE.md excluded.
	"CONSTRAINTS.md", "README.md",
	"go.mod", "package.json", "pyproject.toml", "Cargo.toml",
	"pom.xml", "build.gradle", "Makefile", "Dockerfile",
	".golangci.yml", "tsconfig.json", ".github/workflows",
}

// Run executes a short-bounded git command in dir and returns trimmed stdout.
func Run(parent context.Context, dir string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(parent, 2*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// ResolveRoot walks up from dir to find the enclosing .git repository root, stopping before home or filesystem root.
func ResolveRoot(dir string) string {
	dir = strings.TrimSpace(dir)
	if dir == "" {
		return ""
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return ""
	}
	home, _ := os.UserHomeDir()
	home = filepath.Clean(home)

	for cur := filepath.Clean(abs); cur != "/" && cur != "."; {
		if cur == home {
			return ""
		}
		if _, err := os.Stat(filepath.Join(cur, ".git")); err == nil {
			return cur
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			break
		}
		cur = parent
	}
	return ""
}

// FastHead resolves the 12-char HEAD commit hash quickly by reading .git/HEAD directly before invoking git.
func FastHead(ctx context.Context, root string) string {
	raw, err := os.ReadFile(filepath.Join(root, ".git", "HEAD"))
	if err == nil {
		s := strings.TrimSpace(string(raw))
		if strings.HasPrefix(s, "ref:") {
			ref := strings.TrimSpace(strings.TrimPrefix(s, "ref:"))
			if refBytes, err := os.ReadFile(filepath.Join(root, ".git", filepath.FromSlash(ref))); err == nil {
				if h := strings.TrimSpace(string(refBytes)); len(h) >= 12 {
					return h[:12]
				}
			}
		} else if len(s) >= 12 {
			return s[:12]
		}
	}
	return RevParse(ctx, root, "HEAD")
}

// RevParse resolves a revision spec into a 12-char commit hash.
func RevParse(ctx context.Context, root, rev string) string {
	sha, err := Run(ctx, root, "rev-parse", "--verify", "--short=12", rev+"^{commit}")
	if err != nil {
		return ""
	}
	return sha
}

// DetectHeadLayout builds a compact summary of project manifests and top-level directories at root.
func DetectHeadLayout(root string) string {
	var found []string
	for _, rel := range BaselineManifests {
		if _, err := os.Stat(filepath.Join(root, rel)); err == nil {
			found = append(found, rel)
		}
	}
	entries, _ := os.ReadDir(root)
	var dirs []string
	for _, ent := range entries {
		name := ent.Name()
		if !ent.IsDir() || strings.HasPrefix(name, ".") || name == "node_modules" || name == "vendor" || name == "dist" || name == "build" || name == "target" {
			continue
		}
		dirs = append(dirs, name+"/")
		if len(dirs) >= 12 {
			break
		}
	}
	var parts []string
	if len(found) > 0 {
		parts = append(parts, "Manifests/Docs: "+strings.Join(found, ", "))
	}
	if len(dirs) > 0 {
		parts = append(parts, "Top Directories: "+strings.Join(dirs, ", "))
	}
	if len(parts) == 0 {
		return "(standard manifests not detected; inspect root files at HEAD)"
	}
	return strings.Join(parts, "\n")
}

// CollectPendingCommits inspects commits from lastCommit..HEAD (or up to maxInitial) excluding recorded 7-char SHAs.
func CollectPendingCommits(ctx context.Context, repoRoot, lastCommit string, maxInitial int, existingSHAs map[string]bool) (string, []domain.GitCommitBrief) {
	if repoRoot == "" {
		return "", nil
	}
	head := FastHead(ctx, repoRoot)
	if head == "" {
		return "", nil
	}

	var args []string
	if lastCommit != "" {
		if lastCommit == head || RevParse(ctx, repoRoot, lastCommit) == head {
			return head, nil
		}
		if _, err := Run(ctx, repoRoot, "merge-base", "--is-ancestor", lastCommit, "HEAD"); err == nil {
			args = []string{"log", "--reverse", "-n", "15", "--format=COMMIT\t%h\t%cs\t%s", "--name-only", lastCommit + "..HEAD"}
		} else {
			args = []string{"log", "--reverse", "-n", strconv.Itoa(maxInitial), "--format=COMMIT\t%h\t%cs\t%s", "--name-only"}
		}
	} else {
		args = []string{"log", "--reverse", "-n", strconv.Itoa(maxInitial), "--format=COMMIT\t%h\t%cs\t%s", "--name-only"}
	}

	out, err := Run(ctx, repoRoot, args...)
	if err != nil || strings.TrimSpace(out) == "" {
		return head, nil
	}

	var commits []domain.GitCommitBrief
	var cur *domain.GitCommitBrief
	for _, rawLine := range strings.Split(out, "\n") {
		line := strings.TrimSpace(rawLine)
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "COMMIT\t") {
			if cur != nil {
				if len(cur.SHA) < 7 || !existingSHAs[cur.SHA[:7]] {
					commits = append(commits, *cur)
				}
			}
			parts := strings.SplitN(line, "\t", 4)
			if len(parts) >= 4 {
				cur = &domain.GitCommitBrief{
					SHA:     strings.TrimSpace(parts[1]),
					Date:    strings.TrimSpace(parts[2]),
					Subject: strings.TrimSpace(parts[3]),
				}
			} else {
				cur = nil
			}
			continue
		}
		if cur != nil && len(cur.Files) < 8 {
			cur.Files = append(cur.Files, line)
		}
	}
	if cur != nil && (len(cur.SHA) < 7 || !existingSHAs[cur.SHA[:7]]) {
		commits = append(commits, *cur)
	}
	return head, commits
}

// CleanCommitSubject strips conventional-commit prefixes (e.g. "feat(scope): ") when a meaningful body follows.
func CleanCommitSubject(subject string) string {
	s := strings.TrimSpace(subject)
	if idx := strings.Index(s, ":"); idx > 0 && idx < 28 {
		if body := strings.TrimSpace(s[idx+1:]); len(body) >= 4 {
			return body
		}
	}
	return s
}
