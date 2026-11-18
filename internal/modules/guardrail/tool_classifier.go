package guardrail

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"

	"memorize/internal/domain"
)

func isGuardrailExemptTool(name string) bool {
	lower := strings.ToLower(strings.TrimSpace(name))
	switch lower {
	case "read", "read_file", "view", "view_file", "glob", "find_by_name",
		"grep", "grep_search", "ls", "list_dir", "find", "search_web",
		"query_project_memory", "verify_plan_against_memory",
		"list_resources", "read_resource",
		"manage_task", "send_message", "ask_question", "list_plugin_accounts", "search_marketplace",
		"commit_memory", "manage_memory", "project_progress", "invoke_subagent", "define_subagent", "manage_subagents":
		return true
	}
	for _, memTool := range []string{
		"_query_project_memory", "_verify_plan_against_memory",
		"_commit_memory", "_manage_memory", "_project_progress",
	} {
		if strings.HasSuffix(lower, memTool) {
			return true
		}
	}
	return false
}

func isFileEditTool(name string) bool {
	return domain.IsFileEditTool(name)
}

var (
	readOnlyShellBins  = map[string]bool{"ls": true, "pwd": true, "cat": true, "head": true, "tail": true, "wc": true, "ps": true, "lsof": true, "which": true, "type": true, "uname": true, "date": true, "stat": true, "file": true, "shasum": true, "grep": true, "rg": true, "echo": true, "true": true}
	safeGoEnvKeys      = map[string]map[string]bool{"CGO_ENABLED": {"0": true}, "GOOS": {"darwin": true, "linux": true, "windows": true, "freebsd": true, "openbsd": true, "netbsd": true}, "GOARCH": {"amd64": true, "arm64": true, "386": true, "arm": true, "riscv64": true, "ppc64le": true, "s390x": true}}
	readOnlyGitSubcmds = map[string]bool{"status": true, "diff": true, "log": true, "show": true, "rev-parse": true, "ls-files": true}
	readOnlyGitBrFlags = map[string]bool{"-a": true, "--all": true, "-r": true, "--remotes": true, "-v": true, "-vv": true, "--list": true, "--show-current": true}
)

func hasFlagPrefix(args []string, prefixes ...string) bool {
	for _, a := range args {
		for _, p := range prefixes {
			if a == p || strings.HasPrefix(a, p+"=") {
				return true
			}
		}
	}
	return false
}

func isReadOnlyShellBin(bin string, fields []string) bool {
	if readOnlyShellBins[bin] {
		return true
	}
	switch bin {
	case "gofmt":
		for _, f := range fields[1:] {
			if strings.HasPrefix(f, "-") && f != "-" && strings.Trim(f[1:], "ldes") != "" {
				return false
			}
		}
		return true
	case "go":
		if len(fields) < 2 {
			return false
		}
		switch fields[1] {
		case "vet", "list", "version":
			return true
		case "env":
			return !hasFlagPrefix(fields[2:], "-w", "-u")
		case "test":
			return !hasFlagPrefix(fields[2:], "-c", "-o", "-exec")
		}
	case "git":
		i := 1
		if len(fields) >= 3 && fields[1] == "-C" {
			i = 3
		}
		if len(fields) <= i || hasFlagPrefix(fields[i+1:], "-o") {
			return false
		}
		for _, f := range fields[i+1:] {
			if strings.HasPrefix(f, "--output") {
				return false
			}
		}
		sub, rest := fields[i], fields[i+1:]
		if readOnlyGitSubcmds[sub] {
			return true
		}
		if sub == "branch" {
			for _, f := range rest {
				if !readOnlyGitBrFlags[f] {
					return false
				}
			}
			return true
		}
		return sub == "remote" && (len(rest) == 0 || (len(rest) == 1 && (rest[0] == "-v" || rest[0] == "--verbose")) || (len(rest) == 2 && rest[0] == "get-url"))
	}
	return false
}

// isReadOnlyShellCommand deterministically checks if a command line consists solely of
// non-mutating inspection/test/format-check commands without unsafe env overrides or file redirections.
func isReadOnlyShellCommand(raw string) bool {
	cmdStr := strings.TrimSpace(raw)
	if cmdStr == "" {
		return true
	}
	cleaned := strings.NewReplacer("2>&1", "", ">/dev/null", "", "> /dev/null", "").Replace(cmdStr)
	if strings.ContainsAny(cleaned, ">`\n") || strings.Contains(cleaned, "$(") {
		return false
	}
	for _, seg := range strings.Split(strings.NewReplacer("&&", "\n", "||", "\n", ";", "\n", "|", "\n").Replace(cleaned), "\n") {
		fields := strings.Fields(strings.TrimSpace(seg))
		if len(fields) == 0 {
			continue
		}
		hasGoEnv := false
		for len(fields) > 0 && strings.Contains(fields[0], "=") {
			k, v, ok := strings.Cut(fields[0], "=")
			if !ok || !safeGoEnvKeys[k][v] {
				return false
			}
			hasGoEnv = true
			fields = fields[1:]
		}
		if len(fields) == 0 {
			return false
		}
		bin := filepath.Base(fields[0])
		if (hasGoEnv && bin != "go") || !isReadOnlyShellBin(bin, fields) {
			return false
		}
	}
	return true
}

func extractPlanFromEvent(ev NormalizedHookEvent) string {
	if s := strings.TrimSpace(ev.Raw.ProposedPlan); s != "" {
		return s
	}
	tName := ev.ToolName
	if isGuardrailExemptTool(tName) {
		return ""
	}
	in := ev.ToolInput
	if len(in) == 0 {
		return ""
	}
	if strings.EqualFold(tName, "call_mcp_tool") {
		if subTool, ok := in["ToolName"].(string); ok && isGuardrailExemptTool(subTool) {
			return ""
		}
	}
	for _, ck := range []string{"command", "CommandLine"} {
		if v, ok := in[ck]; ok {
			if cmdStr, ok := v.(string); ok && isReadOnlyShellCommand(cmdStr) {
				return ""
			}
		}
	}
	var parts []string
	if tName != "" {
		parts = append(parts, "Tool: "+tName)
	}
	for _, k := range []string{"file_path", "Path", "path", "command", "CommandLine", "content", "Content", "new_string", "Replacement", "description"} {
		if v, ok := in[k]; ok {
			if s, ok := v.(string); ok {
				if k == "Replacement" {
					if oldStr, okOld := in["Target"].(string); okOld {
						s = domain.DiffEditSnippet(oldStr, s)
					}
				} else if k == "new_string" {
					if oldStr, okOld := in["old_string"].(string); okOld {
						s = domain.DiffEditSnippet(oldStr, s)
					}
				}
				s = strings.TrimSpace(s)
				if s != "" {
					s = domain.TruncateUTF8Bytes(s, 1000)
					parts = append(parts, fmt.Sprintf("%s: %s", k, s))
				}
			}
		}
	}
	if len(parts) <= 1 && tName != "" {
		if raw, err := json.Marshal(in); err == nil && len(raw) > 2 {
			parts = append(parts, "Input: "+domain.TruncateUTF8Bytes(string(raw), 1000))
		}
	}
	if len(parts) == 0 {
		return ""
	}
	return strings.Join(parts, "\n")
}
