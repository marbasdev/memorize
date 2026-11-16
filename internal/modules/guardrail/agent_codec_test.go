package guardrail

import (
	"encoding/json"
	"testing"

	"memorize/internal/domain"
)

func TestDecodeHookEventAndCodecs(t *testing.T) {
	ev := decodeHookEvent("antigravity-pretool", []byte(`{
		"conversationId": "conv-123",
		"workspacePaths": ["/tmp/ws1"],
		"lastUserInput": "always use strict types",
		"toolCall": {"name": "run_command", "args": {"CommandLine": "rm -rf build"}}
	}`))
	if !ev.Valid || ev.Agent != "antigravity" || ev.Phase != domain.HookPhasePreTool {
		t.Fatalf("unexpected parsed meta: %+v", ev)
	}
	if ev.SessionID != "conv-123" || ev.ToolName != "run_command" || ev.TurnPrompt != "always use strict types" {
		t.Fatalf("unexpected normalized fields: %+v", ev)
	}

	out, code, _ := codecForAgent("claude").FormatDenyPreTool("blocked")
	if code != 2 {
		t.Fatalf("expected claude pretool deny exit code 2, got %d", code)
	}
	var payload map[string]string
	if json.Unmarshal([]byte(out), &payload) != nil || payload["decision"] != "block" {
		t.Fatalf("unexpected claude deny body: %s", out)
	}

	outA, codeA, _ := codecForAgent("antigravity").FormatDenyPreTool("blocked")
	if codeA != 0 {
		t.Fatalf("expected antigravity pretool deny exit code 0, got %d", codeA)
	}
	if json.Unmarshal([]byte(outA), &payload) != nil || payload["decision"] != "deny" {
		t.Fatalf("unexpected antigravity deny body: %s", outA)
	}
}
