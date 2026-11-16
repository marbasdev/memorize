package domain

import "testing"

func TestAgentSpecsAndHookParsing(t *testing.T) {
	if len(AllAgentSpecs()) < 3 {
		t.Fatalf("expected at least 3 built-in agent specs")
	}
	ag, ph, ok := ParseHookMode("antigravity-pretool")
	if !ok || ag != "antigravity" || ph != HookPhasePreTool {
		t.Fatalf("unexpected parse for antigravity-pretool: %q %q %v", ag, ph, ok)
	}
	if ag, ph, ok := ParseHookMode("claude-compact"); !ok || ag != "claude" || ph != HookPhaseCompact {
		t.Fatalf("unexpected parse for claude-compact: %q %q %v", ag, ph, ok)
	}
	if ag, ph, ok := ParseHookMode("git-precommit"); !ok || ag != "git" || ph != HookPhasePreCommit {
		t.Fatalf("unexpected parse for git-precommit: %q %q %v", ag, ph, ok)
	}
	if _, _, ok := ParseHookMode("antigravity-compact"); ok {
		t.Fatalf("expected unsupported phase to fail parse")
	}
	if _, _, ok := ParseHookMode("unknown-start"); ok {
		t.Fatalf("expected unknown agent to fail parse")
	}
	if got := FormatHookSourceLabel("claude-pretool"); got != "PreToolUse (Claude)" {
		t.Fatalf("unexpected source label: %q", got)
	}
	if got := FormatHookSourceLabel("antigravity-stop"); got != "Stop Batch (Antigravity)" {
		t.Fatalf("unexpected source label: %q", got)
	}
	if got := FormatHookSourceLabel("git-precommit"); got != "Git Pre-Commit" {
		t.Fatalf("unexpected git source label: %q", got)
	}
}
