package rulepack

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"praxis/internal/domain"
	"praxis/internal/infra/git"
)

const maxPublishAttempts = 3 // 1 initial attempt + up to 2 CAS retries on concurrent push conflicts

var (
	errAbortAlreadyCovered   = errors.New("rulepack publish aborted: rule already covered in latest remote baseline")
	errAbortSemanticConflict = errors.New("rulepack publish paused: policy conflict with latest team rules")
)

type ConflictEvaluator func(ctx context.Context, rule domain.TeamRule, latestDoc *Document) (alreadyCovered bool, conflictReason string, err error)

// PublishInput configures promoting or adding a Team Rule cleanly via isolated Git worktree push.
type PublishInput struct {
	Scope         string
	RepoDir       string
	Proposal      domain.RulePlacementProposal
	TargetBranch  string
	LocalMemoryID int64
	CommitMessage string
	ConflictCheck ConflictEvaluator
}

// UpdateInput configures updating an existing Team Rule in place via isolated Git worktree push.
type UpdateInput struct {
	Scope         string
	RepoDir       string
	TargetKey     string
	Updated       domain.TeamRule
	TargetBranch  string
	CommitMessage string
	ConflictCheck ConflictEvaluator
}

// RemoveInput configures deleting an existing Team Rule via isolated Git worktree push.
type RemoveInput struct {
	Scope         string
	RepoDir       string
	TargetKey     string
	TargetBranch  string
	CommitMessage string
}

type PublishResult struct {
	Status          string // "published" | "updated" | "removed" | "already_covered" | "conflict_pending"
	Rule            domain.TeamRule
	DocumentHash    string
	TargetBranch    string
	BranchPushed    string
	CommitSHA       string
	Attempts        int
	WorkspaceSynced bool
	ConflictReason  string
	Document        *Document
}

type casMutationConfig struct {
	Scope         string
	RepoDir       string
	TargetBranch  string
	CommitMessage string
	SuccessStatus string
	Rule          domain.TeamRule
}

func (s *Service) resolveMutationTarget(ctx context.Context, rawScope, repoDir string) (sc, root string, err error) {
	sc = domain.NormalizeScope(rawScope)
	if !strings.HasPrefix(sc, "project:") {
		return "", "", fmt.Errorf("team rule pack requires a project:* scope (got %q)", sc)
	}
	root = s.resolveScopeRepoPath(ctx, sc, repoDir)
	if root == "" {
		return "", "", errors.New("could not resolve Git repository root for scope " + sc)
	}
	if !git.HasOriginRemote(ctx, root) {
		return "", "", errors.New("team rules require an 'origin' remote for push")
	}
	return sc, root, nil
}

func (s *Service) runCASMutation(ctx context.Context, cfg casMutationConfig, build func(baseRaw []byte, latestDoc *Document, latestHash *string, conflictReason *string) ([]byte, bool, error)) (PublishResult, error) {
	var conflictReason string
	var latestDoc *Document
	var latestHash string

	buildOnce := func(baseRaw []byte) ([]byte, bool, error) {
		var err error
		if len(baseRaw) > 0 {
			latestHash = CanonicalHash(baseRaw)
			latestDoc, err = ParseDocument(baseRaw, cfg.Scope)
			if err != nil {
				return nil, false, err
			}
		} else {
			latestHash = ""
			latestDoc = &Document{Exists: false}
		}
		return build(baseRaw, latestDoc, &latestHash, &conflictReason)
	}

	var gitRes git.RulePackGitResult
	var err error
	for try := 1; try <= maxPublishAttempts; try++ {
		conflictReason = ""
		gitRes, err = git.IsolatedRulePackCommit(ctx, git.RulePackGitOptions{
			Root:          cfg.RepoDir,
			TargetBranch:  cfg.TargetBranch,
			CommitMessage: cfg.CommitMessage,
		}, buildOnce)

		if errors.Is(err, errAbortAlreadyCovered) {
			if s != nil && latestDoc != nil {
				_, _ = s.MirrorSnapshot(ctx, cfg.Scope, cfg.RepoDir, gitRes.TargetBranch, serializeLines(latestDoc.Lines))
			}
			return PublishResult{
				Status:       "already_covered",
				Rule:         cfg.Rule,
				DocumentHash: latestHash,
				Attempts:     try,
				Document:     latestDoc,
			}, nil
		}
		if errors.Is(err, errAbortSemanticConflict) {
			if s != nil && latestDoc != nil {
				_, _ = s.MirrorSnapshot(ctx, cfg.Scope, cfg.RepoDir, gitRes.TargetBranch, serializeLines(latestDoc.Lines))
			}
			return PublishResult{
				Status:         "conflict_pending",
				Rule:           cfg.Rule,
				DocumentHash:   latestHash,
				Attempts:       try,
				ConflictReason: conflictReason,
				Document:       latestDoc,
			}, nil
		}
		if err == nil {
			doc, parseErr := ParseDocument(gitRes.WrittenBytes, cfg.Scope)
			if parseErr != nil {
				return PublishResult{}, parseErr
			}
			remoteMirrorBranch := ""
			if !gitRes.WorkspaceSynced {
				remoteMirrorBranch = gitRes.TargetBranch
			}
			if s != nil {
				_, _ = s.MirrorSnapshot(ctx, cfg.Scope, cfg.RepoDir, remoteMirrorBranch, gitRes.WrittenBytes)
			}
			return PublishResult{
				Status:          cfg.SuccessStatus,
				Rule:            cfg.Rule,
				DocumentHash:    CanonicalHash(gitRes.WrittenBytes),
				TargetBranch:    gitRes.TargetBranch,
				BranchPushed:    gitRes.BranchPushed,
				CommitSHA:       gitRes.CommitSHA,
				Attempts:        try,
				WorkspaceSynced: gitRes.WorkspaceSynced,
				Document:        doc,
			}, nil
		}
		if errors.Is(err, git.ErrPushNonFastForward) && try < maxPublishAttempts {
			continue
		}
		return PublishResult{Attempts: try}, err
	}
	return PublishResult{Attempts: maxPublishAttempts}, err
}

func checkConflictEvaluator(ctx context.Context, check ConflictEvaluator, rule domain.TeamRule, doc *Document, conflictReason *string) error {
	if check == nil {
		return nil
	}
	covered, cReason, err := check(ctx, rule, doc)
	if err != nil {
		return err
	}
	if covered {
		return errAbortAlreadyCovered
	}
	if reason := strings.TrimSpace(cReason); reason != "" {
		*conflictReason = reason
		return errAbortSemanticConflict
	}
	return nil
}

func (s *Service) PublishProposal(ctx context.Context, in PublishInput) (PublishResult, error) {
	sc, root, err := s.resolveMutationTarget(ctx, in.Scope, in.RepoDir)
	if err != nil {
		return PublishResult{}, err
	}

	prop := in.Proposal
	prop.AddedBy, prop.AddedAt = s.normalizeAttribution(ctx, root, prop.AddedBy, prop.AddedAt)
	prop.Category = domain.NormalizeCategoryForScope(sc, prop.Category)
	prop.SubCategory = domain.NormalizeSubCategory(prop.SubCategory)
	prop.RuleKey = domain.ComputeRuleKey(prop.Category, prop.Title)
	prop.Pinned = true

	rule := prop.ToTeamRule(sc)
	if err := domain.ValidateTeamRule(rule); err != nil {
		return PublishResult{}, err
	}

	commitMsg := strings.TrimSpace(in.CommitMessage)
	if commitMsg == "" {
		commitMsg = fmt.Sprintf("praxis(rulepack): add %s team rule", rule.Category)
	}

	return s.runCASMutation(ctx, casMutationConfig{
		Scope:         sc,
		RepoDir:       root,
		TargetBranch:  in.TargetBranch,
		CommitMessage: commitMsg,
		SuccessStatus: "published",
		Rule:          rule,
	}, func(baseRaw []byte, latestDoc *Document, _ *string, conflictReason *string) ([]byte, bool, error) {
		for _, existing := range latestDoc.Rules {
			if existing.RuleKey == rule.RuleKey {
				return nil, false, errAbortAlreadyCovered
			}
		}
		if err := checkConflictEvaluator(ctx, in.ConflictCheck, rule, latestDoc, conflictReason); err != nil {
			return nil, false, err
		}

		updated, err := InsertRule(baseRaw, sc, prop)
		if err != nil {
			if errors.Is(err, ErrRuleKeyExists) {
				return nil, false, errAbortAlreadyCovered
			}
			return nil, false, err
		}
		return updated, true, nil
	})
}

// UpdateRule replaces an existing Team Rule cleanly inside an isolated Git worktree with CAS conflict retry.
func (s *Service) UpdateRule(ctx context.Context, in UpdateInput) (PublishResult, error) {
	sc, root, err := s.resolveMutationTarget(ctx, in.Scope, in.RepoDir)
	if err != nil {
		return PublishResult{}, err
	}
	targetKey := domain.NormalizeRuleKey(in.TargetKey)
	if targetKey == "" {
		return PublishResult{}, fmt.Errorf("%w: empty target rule", ErrRuleNotFound)
	}

	upd := in.Updated
	upd.Scope = sc
	upd.Category = domain.NormalizeCategoryForScope(sc, upd.Category)
	upd.SubCategory = domain.NormalizeSubCategory(upd.SubCategory)
	upd.Title = strings.Join(strings.Fields(strings.TrimSpace(upd.Title)), " ")
	upd.RuleKey = domain.ComputeRuleKey(upd.Category, upd.Title)
	upd.Pinned = true
	upd.AddedBy, upd.AddedAt = s.normalizeAttribution(ctx, root, upd.AddedBy, upd.AddedAt)
	if err := domain.ValidateTeamRule(upd); err != nil {
		return PublishResult{}, err
	}

	commitMsg := strings.TrimSpace(in.CommitMessage)
	if commitMsg == "" {
		commitMsg = fmt.Sprintf("praxis(rulepack): update %s team rule", upd.Category)
	}

	return s.runCASMutation(ctx, casMutationConfig{
		Scope:         sc,
		RepoDir:       root,
		TargetBranch:  in.TargetBranch,
		CommitMessage: commitMsg,
		SuccessStatus: "updated",
		Rule:          upd,
	}, func(baseRaw []byte, latestDoc *Document, _ *string, conflictReason *string) ([]byte, bool, error) {
		if in.ConflictCheck != nil {
			filtered := &Document{Exists: latestDoc.Exists, Hash: latestDoc.Hash}
			for _, r := range latestDoc.Rules {
				if r.RuleKey != targetKey {
					filtered.Rules = append(filtered.Rules, r)
				}
			}
			if err := checkConflictEvaluator(ctx, in.ConflictCheck, upd, filtered, conflictReason); err != nil {
				return nil, false, err
			}
		}
		written, err := ReplaceRuleByKey(baseRaw, sc, targetKey, upd)
		if err != nil {
			return nil, false, err
		}
		return written, true, nil
	})
}

// RemoveRuleRemote removes an existing Team Rule cleanly inside an isolated Git worktree with CAS retry.
func (s *Service) RemoveRuleRemote(ctx context.Context, in RemoveInput) (PublishResult, error) {
	sc, root, err := s.resolveMutationTarget(ctx, in.Scope, in.RepoDir)
	if err != nil {
		return PublishResult{}, err
	}
	targetKey := domain.NormalizeRuleKey(in.TargetKey)
	if targetKey == "" {
		return PublishResult{}, fmt.Errorf("%w: empty target rule", ErrRuleNotFound)
	}

	commitMsg := strings.TrimSpace(in.CommitMessage)
	if commitMsg == "" {
		commitMsg = "praxis(rulepack): remove team rule"
	}

	var removedRule domain.TeamRule
	res, err := s.runCASMutation(ctx, casMutationConfig{
		Scope:         sc,
		RepoDir:       root,
		TargetBranch:  in.TargetBranch,
		CommitMessage: commitMsg,
		SuccessStatus: "removed",
	}, func(baseRaw []byte, latestDoc *Document, _ *string, _ *string) ([]byte, bool, error) {
		for _, r := range latestDoc.Rules {
			if r.RuleKey == targetKey {
				removedRule = r
				break
			}
		}
		written, err := RemoveRule(baseRaw, sc, targetKey)
		if err != nil {
			return nil, false, err
		}
		return written, true, nil
	})
	if err == nil && res.Rule.RuleKey == "" {
		res.Rule = removedRule
	}
	return res, err
}
