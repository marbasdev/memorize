package main

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"memorize/internal/memory"
)

func TestSingleBinaryInstallersAndEmbeddedMCPResource(t *testing.T) {
	if !strings.Contains(memory.PlaybookSkillMD, "name: memory-curator") {
		t.Fatalf("expected embedded PlaybookSkillMD frontmatter")
	}
	body := memory.PlaybookBodyMD()
	if strings.HasPrefix(body, "---") || !strings.Contains(body, "Golden Filter") {
		t.Fatalf("expected stripped PlaybookBodyMD, got:\n%s", body)
	}

	tmp := t.TempDir()
	binPath := filepath.Join(tmp, "memorize")
	if runtime.GOOS == "windows" {
		binPath += ".exe"
	}
	buildCmd := exec.Command("go", "build", "-ldflags=-X main.version=1.5.0-test", "-o", binPath, ".")
	if out, err := buildCmd.CombinedOutput(); err != nil {
		t.Fatalf("build memorize failed: %v\n%s", err, string(out))
	}

	// 1. -version flag
	verOut, err := exec.Command(binPath, "-version").CombinedOutput()
	if err != nil || strings.TrimSpace(string(verOut)) != "memorize 1.5.0-test" {
		t.Fatalf("unexpected -version output: %q (%v)", string(verOut), err)
	}

	// 2. -install git-hook
	dataDir := filepath.Join(tmp, "data")
	repoDir := filepath.Join(tmp, "repo")
	if err := os.MkdirAll(filepath.Join(repoDir, ".git", "hooks"), 0o755); err != nil {
		t.Fatal(err)
	}
	instGit := exec.Command(binPath, "-install", "git-hook")
	instGit.Dir = repoDir
	instGit.Env = append(os.Environ(), "MEMORY_DATA_DIR="+dataDir)
	if out, err := instGit.CombinedOutput(); err != nil {
		t.Fatalf("-install git-hook failed: %v\n%s", err, string(out))
	}
	hookPath := filepath.Join(repoDir, ".git", "hooks", "pre-commit")
	hookBytes, err := os.ReadFile(hookPath)
	if err != nil {
		t.Fatalf("missing pre-commit hook: %v", err)
	}
	realBinPath, err := filepath.EvalSymlinks(binPath)
	if err != nil {
		realBinPath = binPath
	}
	if !strings.Contains(string(hookBytes), "'"+realBinPath+"'") && !strings.Contains(string(hookBytes), "'"+binPath+"'") {
		t.Fatalf("expected single-quoted binary path in pre-commit hook, got:\n%s", string(hookBytes))
	}
	if runtime.GOOS != "windows" {
		symRepo := filepath.Join(tmp, "symrepo")
		if err := os.MkdirAll(filepath.Join(symRepo, ".git", "hooks"), 0o755); err != nil {
			t.Fatal(err)
		}
		targetVictim := filepath.Join(tmp, "victim.txt")
		if err := os.WriteFile(targetVictim, []byte("untouched"), 0o600); err != nil {
			t.Fatal(err)
		}
		symHook := filepath.Join(symRepo, ".git", "hooks", "pre-commit")
		if err := os.Symlink(targetVictim, symHook); err != nil {
			t.Fatal(err)
		}
		symCmd := exec.Command(binPath, "-install", "git-hook")
		symCmd.Dir = symRepo
		symCmd.Env = append(os.Environ(), "MEMORY_DATA_DIR="+dataDir)
		if out, err := symCmd.CombinedOutput(); err == nil {
			t.Fatalf("expected -install git-hook to refuse symlink pre-commit hook, output:\n%s", string(out))
		}
		if got, _ := os.ReadFile(targetVictim); string(got) != "untouched" {
			t.Fatalf("symlink target was modified by -install git-hook")
		}
	}

	// 3. Pure MCP Stdio Resource & Prompt verification (zero external SKILL.md needed)
	srvCmd := exec.Command(binPath)
	srvCmd.Env = append(os.Environ(),
		"MEMORY_DB_PATH="+filepath.Join(tmp, "stdio.db"),
		"MEMORY_DISABLE_BG_DASHBOARD=1",
	)
	stdin, err := srvCmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := srvCmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := srvCmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = stdin.Close()
		_ = srvCmd.Wait()
	}()

	reader := bufio.NewReader(stdout)
	sendRPC := func(msg string) string {
		t.Helper()
		_, _ = stdin.Write([]byte(msg + "\n"))
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatalf("read rpc line: %v", err)
		}
		return line
	}

	initLine := sendRPC(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"test","version":"1.0"}}}`)
	if !strings.Contains(initLine, memory.PlaybookURI) {
		t.Fatalf("expected serverInstructions in initialize response to reference %s, got: %s", memory.PlaybookURI, initLine)
	}
	for _, wantToken := range []string{"planned", "blocked"} {
		if !strings.Contains(initLine, wantToken) {
			t.Fatalf("expected serverInstructions in initialize response to describe %q progress state, got: %s", wantToken, initLine)
		}
	}

	resLine := sendRPC(`{"jsonrpc":"2.0","id":2,"method":"resources/read","params":{"uri":"memorize://playbook/memory-curator"}}`)
	var resPayload struct {
		Result struct {
			Contents []struct {
				URI  string `json:"uri"`
				Text string `json:"text"`
			} `json:"contents"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(resLine), &resPayload); err != nil {
		t.Fatalf("unmarshal resources/read: %v (%s)", err, resLine)
	}
	if len(resPayload.Result.Contents) != 1 || !strings.Contains(resPayload.Result.Contents[0].Text, "Golden Filter") {
		t.Fatalf("unexpected embedded MCP resource contents: %+v", resPayload)
	}

	promptLine := sendRPC(`{"jsonrpc":"2.0","id":3,"method":"prompts/get","params":{"name":"memory-curator"}}`)
	if !strings.Contains(promptLine, "Golden Filter") {
		t.Fatalf("expected prompts/get memory-curator to return playbook text, got: %s", promptLine)
	}
}

func packageBinaryTarGz(t *testing.T, binBytes []byte) ([]byte, string) {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	hdr := &tar.Header{
		Name: "memorize",
		Mode: 0o755,
		Size: int64(len(binBytes)),
	}
	if err := tw.WriteHeader(hdr); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(binBytes); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	archive := buf.Bytes()
	sum := sha256.Sum256(archive)
	return archive, hex.EncodeToString(sum[:])
}

func TestPluginManifestsAndAutoUpdateBootstrapper(t *testing.T) {
	repoRoot, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}

	// 1. Verify all repository plugin manifests parse cleanly and share the same version.
	readManifestVersion := func(relPath string) string {
		t.Helper()
		raw, err := os.ReadFile(filepath.Join(repoRoot, relPath))
		if err != nil {
			t.Fatalf("read %s: %v", relPath, err)
		}
		var doc map[string]any
		if err := json.Unmarshal(raw, &doc); err != nil {
			t.Fatalf("invalid JSON in %s: %v", relPath, err)
		}
		if v, ok := doc["version"].(string); ok && v != "" {
			return v
		}
		if plugins, ok := doc["plugins"].([]any); ok && len(plugins) > 0 {
			if first, ok := plugins[0].(map[string]any); ok {
				if v, ok := first["version"].(string); ok {
					return v
				}
			}
		}
		return ""
	}

	agyVer := readManifestVersion("plugins/memorize/plugin.json")
	claudeVer := readManifestVersion("plugins/memorize/.claude-plugin/plugin.json")
	mpVer := readManifestVersion(".claude-plugin/marketplace.json")
	if agyVer == "" || agyVer != claudeVer || agyVer != mpVer {
		t.Fatalf("version mismatch across manifests: agy=%q claude=%q marketplace=%q", agyVer, claudeVer, mpVer)
	}

	for _, rel := range []string{
		"plugins/memorize/mcp_config.json",
		"plugins/memorize/hooks.json",
		"plugins/memorize/.claude-plugin/plugin.json",
	} {
		raw, err := os.ReadFile(filepath.Join(repoRoot, rel))
		if err != nil {
			t.Fatalf("read %s: %v", rel, err)
		}
		var doc map[string]any
		if err := json.Unmarshal(raw, &doc); err != nil {
			t.Fatalf("invalid JSON in %s: %v", rel, err)
		}
	}

	if _, err := os.Stat(filepath.Join(repoRoot, "plugins/memorize/rules/AGENTS.md")); !os.IsNotExist(err) {
		t.Fatalf("plugins/memorize/rules/AGENTS.md must not exist (memorize replaces static AGENTS.md rule files via MCP instructions & DB rules); stat err=%v", err)
	}

	skillBytes, err := os.ReadFile(filepath.Join(repoRoot, "plugins/memorize/skills/memory-curator/SKILL.md"))
	if err != nil {
		t.Fatalf("read plugin SKILL.md: %v", err)
	}
	if string(skillBytes) != memory.PlaybookSkillMD {
		t.Fatalf("plugins/memorize/skills/memory-curator/SKILL.md is out of sync with internal/memory/playbook/SKILL.md")
	}

	// 2. Test run-memorize.sh GitLab release bootstrap + SHA256 verification + automatic version upgrade (Unix).
	if runtime.GOOS == "windows" {
		return
	}
	tmp := t.TempDir()
	bin100Path := filepath.Join(tmp, "memorize-1.0.0-bin")
	bin110Path := filepath.Join(tmp, "memorize-1.1.0-bin")
	for ver, outPath := range map[string]string{"1.0.0": bin100Path, "1.1.0": bin110Path} {
		cmd := exec.Command("go", "build", "-ldflags=-X main.version="+ver, "-o", outPath, ".")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("build %s failed: %v\n%s", ver, err, string(out))
		}
	}

	b100, err := os.ReadFile(bin100Path)
	if err != nil {
		t.Fatal(err)
	}
	b110, err := os.ReadFile(bin110Path)
	if err != nil {
		t.Fatal(err)
	}
	archiveName := fmt.Sprintf("memorize-%s-%s.tar.gz", runtime.GOOS, runtime.GOARCH)
	tgz100, sum100 := packageBinaryTarGz(t, b100)
	tgz110, sum110 := packageBinaryTarGz(t, b110)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/org/memorize/-/releases/v1.0.0/downloads/" + archiveName,
			"/github.com/org/memorize/releases/download/v1.0.0/" + archiveName:
			_, _ = w.Write(tgz100)
		case "/org/memorize/-/releases/v1.0.0/downloads/SHA256SUMS",
			"/github.com/org/memorize/releases/download/v1.0.0/SHA256SUMS":
			_, _ = fmt.Fprintf(w, "%s  ./%s\n", sum100, archiveName)
		case "/org/memorize/-/releases/v1.1.0/downloads/" + archiveName,
			"/github.com/org/memorize/releases/download/v1.1.0/" + archiveName:
			_, _ = w.Write(tgz110)
		case "/org/memorize/-/releases/v1.1.0/downloads/SHA256SUMS",
			"/github.com/org/memorize/releases/download/v1.1.0/SHA256SUMS":
			_, _ = fmt.Fprintf(w, "%s  ./%s\n", sum110, archiveName)
		case "/org/nosums/-/releases/v1.0.0/downloads/" + archiveName:
			_, _ = w.Write(tgz100)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	// Stage an isolated copy of the plugin outside the Go repository so local repo fallback cannot trigger.
	isolatedPlugin := filepath.Join(tmp, "isolated-plugin", "memorize")
	if err := os.MkdirAll(filepath.Join(isolatedPlugin, "scripts"), 0o755); err != nil {
		t.Fatal(err)
	}
	runnerBytes, err := os.ReadFile(filepath.Join(repoRoot, "plugins/memorize/scripts/run-memorize.sh"))
	if err != nil {
		t.Fatal(err)
	}
	runnerPath := filepath.Join(isolatedPlugin, "scripts", "run-memorize.sh")
	if err := os.WriteFile(runnerPath, runnerBytes, 0o755); err != nil {
		t.Fatal(err)
	}

	dataDir := filepath.Join(tmp, "user-data")
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		t.Fatal(err)
	}
	dbMarkerPath := filepath.Join(dataDir, "memories.db")
	if err := os.WriteFile(dbMarkerPath, []byte("sqlite-state-preserved"), 0o600); err != nil {
		t.Fatal(err)
	}

	writePluginManifest := func(ver string) {
		t.Helper()
		manifest := fmt.Sprintf("{\n  \"name\": \"memorize\",\n  \"version\": %q,\n  \"repository\": %q\n}\n", ver, srv.URL+"/org/memorize")
		if err := os.WriteFile(filepath.Join(isolatedPlugin, "plugin.json"), []byte(manifest), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	// Step A: Initial install at v1.0.0 downloads and caches memorize-1.0.0 with zero stdout pollution.
	writePluginManifest("1.0.0")
	cmd100 := exec.Command("/bin/sh", runnerPath, "-version")
	cmd100.Env = append(os.Environ(), "MEMORY_DATA_DIR="+dataDir)
	var stdout100, stderr100 bytes.Buffer
	cmd100.Stdout = &stdout100
	cmd100.Stderr = &stderr100
	if err := cmd100.Run(); err != nil {
		t.Fatalf("run-memorize.sh v1.0.0 failed: %v\nstderr: %s", err, stderr100.String())
	}
	if strings.TrimSpace(stdout100.String()) != "memorize 1.0.0" {
		t.Fatalf("expected clean stdout 'memorize 1.0.0', got %q (stderr: %s)", stdout100.String(), stderr100.String())
	}
	if _, err := os.Stat(filepath.Join(dataDir, "bin", "memorize-1.0.0")); err != nil {
		t.Fatalf("expected cached memorize-1.0.0 binary: %v", err)
	}

	// Step B: Plugin manifest updates to v1.1.0 -> automatically downloads v1.1.0, prunes v1.0.0, preserves memories.db.
	writePluginManifest("1.1.0")
	cmd110 := exec.Command("/bin/sh", runnerPath, "-version")
	cmd110.Env = append(os.Environ(), "MEMORY_DATA_DIR="+dataDir)
	var stdout110, stderr110 bytes.Buffer
	cmd110.Stdout = &stdout110
	cmd110.Stderr = &stderr110
	if err := cmd110.Run(); err != nil {
		t.Fatalf("run-memorize.sh v1.1.0 upgrade failed: %v\nstderr: %s", err, stderr110.String())
	}
	if strings.TrimSpace(stdout110.String()) != "memorize 1.1.0" {
		t.Fatalf("expected clean stdout 'memorize 1.1.0', got %q (stderr: %s)", stdout110.String(), stderr110.String())
	}
	if _, err := os.Stat(filepath.Join(dataDir, "bin", "memorize-1.1.0")); err != nil {
		t.Fatalf("expected cached memorize-1.1.0 binary: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dataDir, "bin", "memorize-1.0.0")); !os.IsNotExist(err) {
		t.Fatalf("expected outdated memorize-1.0.0 binary to be pruned, stat err=%v", err)
	}
	preservedDB, err := os.ReadFile(dbMarkerPath)
	if err != nil || string(preservedDB) != "sqlite-state-preserved" {
		t.Fatalf("expected memories.db to be preserved across binary upgrade")
	}

	// Step C: Verify GitHub Releases URL pattern (*github.com/* -> /releases/download/v<VER>/...) in a fresh data dir.
	ghDataDir := filepath.Join(tmp, "gh-user-data")
	ghManifest := fmt.Sprintf("{\n  \"name\": \"memorize\",\n  \"version\": \"1.1.0\",\n  \"repository\": %q\n}\n", srv.URL+"/github.com/org/memorize")
	if err := os.WriteFile(filepath.Join(isolatedPlugin, "plugin.json"), []byte(ghManifest), 0o644); err != nil {
		t.Fatal(err)
	}
	cmdGH := exec.Command("/bin/sh", runnerPath, "-version")
	cmdGH.Env = append(os.Environ(), "MEMORY_DATA_DIR="+ghDataDir)
	var stdoutGH, stderrGH bytes.Buffer
	cmdGH.Stdout = &stdoutGH
	cmdGH.Stderr = &stderrGH
	if err := cmdGH.Run(); err != nil {
		t.Fatalf("run-memorize.sh GitHub release bootstrap failed: %v\nstderr: %s", err, stderrGH.String())
	}
	if strings.TrimSpace(stdoutGH.String()) != "memorize 1.1.0" {
		t.Fatalf("expected clean stdout 'memorize 1.1.0' from GitHub release bootstrap, got %q", stdoutGH.String())
	}

	// Step D: Fail closed when SHA256SUMS is missing or release base URL uses insecure remote HTTP.
	noSumsDataDir := filepath.Join(tmp, "nosums-data")
	noSumsManifest := fmt.Sprintf("{\n  \"name\": \"memorize\",\n  \"version\": \"1.0.0\",\n  \"repository\": %q\n}\n", srv.URL+"/org/nosums")
	if err := os.WriteFile(filepath.Join(isolatedPlugin, "plugin.json"), []byte(noSumsManifest), 0o644); err != nil {
		t.Fatal(err)
	}
	cmdNoSums := exec.Command("/bin/sh", runnerPath, "-version")
	cmdNoSums.Env = append(os.Environ(), "MEMORY_DATA_DIR="+noSumsDataDir)
	if out, err := cmdNoSums.CombinedOutput(); err == nil {
		t.Fatalf("expected run-memorize.sh to fail closed when SHA256SUMS is missing, output:\n%s", string(out))
	}
	if _, err := os.Stat(filepath.Join(noSumsDataDir, "bin", "memorize-1.0.0")); !os.IsNotExist(err) {
		t.Fatalf("expected no binary installed when SHA256SUMS is missing, stat err=%v", err)
	}

	httpDataDir := filepath.Join(tmp, "insecure-http-data")
	cmdInsecure := exec.Command("/bin/sh", runnerPath, "-version")
	cmdInsecure.Env = append(os.Environ(),
		"MEMORY_DATA_DIR="+httpDataDir,
		"MEMORIZE_RELEASE_BASE_URL=http://evil.example.com/v{VERSION}",
	)
	if out, err := cmdInsecure.CombinedOutput(); err == nil {
		t.Fatalf("expected run-memorize.sh to reject non-HTTPS MEMORIZE_RELEASE_BASE_URL, output:\n%s", string(out))
	}
}
