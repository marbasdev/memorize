package localmodel

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"memorize/internal/domain"
	"memorize/internal/inference/providers"
)

func TestStreamingDownloadNotAbortedByMetadataTimeout(t *testing.T) {
	origTimeout := apiClient.Timeout
	apiClient.Timeout = 40 * time.Millisecond
	defer func() { apiClient.Timeout = origTimeout }()

	payload := strings.Repeat("GGUF_MODEL_STREAM_CHUNK_", 1024)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		flusher, _ := w.(http.Flusher)
		w.WriteHeader(http.StatusOK)
		half := len(payload) / 2
		_, _ = w.Write([]byte(payload[:half]))
		if flusher != nil {
			flusher.Flush()
		}
		time.Sleep(110 * time.Millisecond)
		_, _ = w.Write([]byte(payload[half:]))
	}))
	defer srv.Close()

	dir := t.TempDir()
	mgr := NewManager(dir)
	dest := filepath.Join(dir, "streamed-test.gguf")
	if err := mgr.downloadFileWithProgress(context.Background(), srv.URL+"/streamed-test.gguf", dest); err != nil {
		t.Fatalf("expected streaming download to succeed beyond apiClient timeout, got: %v", err)
	}
	got, err := os.ReadFile(dest)
	if err != nil || string(got) != payload {
		t.Fatalf("downloaded file mismatch: len=%d err=%v", len(got), err)
	}
}

func TestDownloadContextCancellationAndPathSafety(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("head"))
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		<-r.Context().Done()
	}))
	defer srv.Close()

	dir := t.TempDir()
	mgr := NewManager(dir)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	dest := filepath.Join(dir, "cancelled.gguf")
	if err := mgr.downloadFileWithProgress(ctx, srv.URL+"/cancelled.gguf", dest); err == nil {
		t.Fatalf("expected cancelled download to return error")
	}

	if err := mgr.DeleteModel("../evil.gguf"); err == nil {
		t.Fatalf("expected path traversal model name to be rejected")
	}
}

func TestPlatformSupportAndEmptyDirGuidance(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen ephemeral port: %v", err)
	}
	unusedPort := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()

	emptyDir := t.TempDir()
	mgr := NewManager(emptyDir)
	st := mgr.Status(unusedPort)
	wantSupported := runtime.GOOS == "darwin" || runtime.GOOS == "linux"
	if st.Supported != wantSupported || st.OS != runtime.GOOS {
		t.Fatalf("unexpected Status platform metadata: supported=%v os=%q", st.Supported, st.OS)
	}

	werr := mgr.EnsureRunning(context.Background(), "", unusedPort)
	if !wantSupported {
		if werr == nil || !strings.Contains(werr.Error(), "Custom Local URL") {
			t.Fatalf("expected unsupported platform guidance recommending Custom Local URL, got: %v", werr)
		}
		return
	}
	if werr == nil || !strings.Contains(werr.Error(), "http://127.0.0.1:18095") {
		t.Fatalf("expected actionable Day-0 guidance pointing to http://127.0.0.1:18095, got: %v", werr)
	}
}

func TestPortFallbackDiscoveryAndManagedState(t *testing.T) {
	dataDir := t.TempDir()
	modelsDir := filepath.Join(dataDir, "models")
	if err := os.MkdirAll(modelsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dataDir, "ports.json"), []byte(`{"daemon_port":19195,"gguf_port":19188}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dataDir, "daemon.json"), []byte(`{"pid":123,"listen_addr":"127.0.0.1:19195"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	mgr := NewManager(modelsDir)
	if got := mgr.resolveTargetPort(0); got != 19188 {
		t.Fatalf("expected ports.json gguf_port 19188, got %d", got)
	}
	if u := mgr.dashboardURL(); u != "http://127.0.0.1:19195" {
		t.Fatalf("expected dashboardURL from daemon.json http://127.0.0.1:19195, got %q", u)
	}
	if !IsSupportedPlatform() {
		return
	}
	if werr := mgr.EnsureRunning(context.Background(), "", 0); werr == nil || !strings.Contains(werr.Error(), "http://127.0.0.1:19195") {
		t.Fatalf("expected empty-model guidance pointing to active daemon URL http://127.0.0.1:19195, got: %v", werr)
	}

	busyLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer busyLn.Close()
	busyPort := busyLn.Addr().(*net.TCPAddr).Port
	excludePort := busyPort + 1
	chosen, fb, err := pickAvailableGGUFPort(busyPort, excludePort)
	if err != nil || !fb || chosen == busyPort || chosen == excludePort {
		t.Fatalf("expected fallback port distinct from busy=%d exclude=%d, got chosen=%d fb=%v err=%v", busyPort, excludePort, chosen, fb, err)
	}

	healthSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"status":"ok"}`))
			return
		}
		http.NotFound(w, r)
	}))
	defer healthSrv.Close()
	_, hpStr, _ := strings.Cut(strings.TrimPrefix(healthSrv.URL, "http://"), ":")
	var mockPort int
	_, _ = fmt.Sscanf(hpStr, "%d", &mockPort)

	if err := os.MkdirAll(mgr.RuntimeDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	psBytes, _ := json.Marshal(pidState{PID: os.Getpid(), Model: "tiny.gguf", Port: mockPort, DesiredPort: 19188})
	if err := os.WriteFile(mgr.pidFilePath(), psBytes, 0o600); err != nil {
		t.Fatal(err)
	}

	st := mgr.Status(0)
	if !st.ServerRunning || st.Port != mockPort || st.DesiredPort != 19188 || !st.PortFallback {
		t.Fatalf("expected fallback running status (port=%d desired=19188 fb=true), got %+v", mockPort, st)
	}
	ep, err := mgr.EnsureRunningEndpoint(context.Background(), "tiny.gguf", 19188)
	if err != nil || ep != fmt.Sprintf("http://127.0.0.1:%d", mockPort) {
		t.Fatalf("expected EnsureRunningEndpoint to reuse active fallback endpoint :%d, got ep=%q err=%v", mockPort, ep, err)
	}
}

func TestSelectManagedContextSize(t *testing.T) {
	t.Setenv("MEMORY_GGUF_CTX", "")
	if got := SelectManagedContextSize(HardwareSpec{RAMGB: 8}, 2<<30); got != 4096 {
		t.Fatalf("expected 4096 for 8GB RAM, got %d", got)
	}
	if got := SelectManagedContextSize(HardwareSpec{RAMGB: 16}, 2<<30); got != 8192 {
		t.Fatalf("expected 8192 for 16GB RAM + 2GB model, got %d", got)
	}
	if got := SelectManagedContextSize(HardwareSpec{RAMGB: 32}, 4<<30); got != 16384 {
		t.Fatalf("expected 16384 for 32GB RAM + 4GB model, got %d", got)
	}
	t.Setenv("MEMORY_GGUF_CTX", "12288")
	if got := SelectManagedContextSize(HardwareSpec{RAMGB: 8}, 2<<30); got != 12288 {
		t.Fatalf("expected MEMORY_GGUF_CTX=12288 override, got %d", got)
	}
}

func TestValidateDownloadURLAndModelSecurity(t *testing.T) {
	cases := []struct {
		url     string
		wantErr bool
		wantFn  string
	}{
		{"https://huggingface.co/unsloth/Qwen-GGUF/resolve/main/qwen-4b-Q4_K_M.gguf", false, "qwen-4b-Q4_K_M.gguf"},
		{"http://127.0.0.1:8080/local-model.gguf", false, "local-model.gguf"},
		{"http://localhost:9000/model.gguf", false, "model.gguf"},
		{"http://example.com/insecure.gguf", true, ""},
		{"https://example.com/not-gguf.bin", true, ""},
		{"https://example.com/bad%20name.gguf", true, ""},
		{"https://example.com/../evil.gguf", false, "evil.gguf"},
	}
	for _, tc := range cases {
		_, fn, err := validateDownloadURL(tc.url)
		if tc.wantErr {
			if err == nil {
				t.Errorf("validateDownloadURL(%q) expected error, got fn=%q", tc.url, fn)
			}
			continue
		}
		if err != nil || fn != tc.wantFn {
			t.Errorf("validateDownloadURL(%q) = (%q, %v), want fn=%q", tc.url, fn, err, tc.wantFn)
		}
	}

	dir := t.TempDir()
	mgr := NewManager(dir)
	if !strings.HasSuffix(mgr.RuntimeDir(), "runtime") {
		t.Fatalf("unexpected RuntimeDir %q", mgr.RuntimeDir())
	}

	if err := os.WriteFile(filepath.Join(dir, "b-model.gguf"), []byte("1234"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "a-model.gguf"), []byte("123456"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "ignore.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "nested.gguf"), 0o755); err != nil {
		t.Fatal(err)
	}
	list := mgr.installedModels()
	if len(list) != 2 || list[0].FileName != "a-model.gguf" || list[1].FileName != "b-model.gguf" {
		t.Fatalf("unexpected installedModels: %+v", list)
	}

	for _, bad := range []string{"", "../secret.gguf", "sub/a-model.gguf", "not-gguf.bin"} {
		if err := mgr.DeleteModel(bad); err == nil {
			t.Errorf("DeleteModel(%q) expected error", bad)
		}
	}
	if err := mgr.DeleteModel("a-model.gguf"); err != nil {
		t.Fatalf("DeleteModel valid failed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "a-model.gguf")); !os.IsNotExist(err) {
		t.Fatalf("expected a-model.gguf to be deleted")
	}

	if _, err := mgr.StartModelDownload("nonexistent-preset", ""); err == nil {
		t.Fatalf("expected unknown preset error")
	}
	if _, err := mgr.StartModelDownload("", "http://remote.invalid/test.gguf"); err == nil && IsSupportedPlatform() {
		t.Fatalf("expected insecure remote URL error")
	}
}

func TestRecommendQuantRankAndRepoFiltering(t *testing.T) {
	if r := quantRank("Qwen3-4B-Q4_K_M.gguf"); r != 100 {
		t.Fatalf("Q4_K_M rank = %d, want 100", r)
	}
	if r := quantRank("mmproj-model-f16.gguf"); r != -1 {
		t.Fatalf("expected mmproj skipped, got %d", r)
	}
	if r := quantRank("nested/Qwen-Q4_K_M.gguf"); r != -1 {
		t.Fatalf("expected nested path skipped, got %d", r)
	}

	best, sz := pickBestGGUF([]hfSibling{
		{RFilename: "mmproj-f16.gguf", Size: 500},
		{RFilename: "model-Q8_0.gguf", Size: 8000},
		{RFilename: "model-Q4_K_M.gguf", Size: 4200},
	})
	if best != "model-Q4_K_M.gguf" || sz != 4200 {
		t.Fatalf("pickBestGGUF = (%q, %d)", best, sz)
	}

	if !isNonChatRepo("black-forest-labs/FLUX.1-dev-GGUF") || !isNonChatRepo("BAAI/bge-reranker-gguf") {
		t.Fatalf("expected non-chat repos to be filtered")
	}
	if isNonChatRepo("unsloth/Qwen3.5-4B-GGUF") {
		t.Fatalf("chat GGUF repo falsely excluded")
	}
	if !isOversizedRepo("unsloth/Qwen3-32B-GGUF") || isOversizedRepo("unsloth/Qwen3-4B-GGUF") {
		t.Fatalf("isOversizedRepo mismatch")
	}
	if !isTrustedGGUFAuthor("ggml-org/gemma-4-GGUF") || isTrustedGGUFAuthor("random/model-GGUF") {
		t.Fatalf("isTrustedGGUFAuthor mismatch")
	}

	hw := HardwareSpec{Platform: "darwin/arm64", RAMGB: 16, SafeBudgetGB: 6.0}
	small := decorateFit(Preset{ID: "p1", SizeBytes: 2 << 30}, hw)
	heavy := decorateFit(Preset{ID: "p2", SizeBytes: 12 << 30}, hw)
	if small.FitStatus != "optimal" || heavy.FitStatus == "optimal" {
		t.Fatalf("decorateFit mismatch: small=%s heavy=%s", small.FitStatus, heavy.FitStatus)
	}

	if bp := SelectBootstrapPreset(); bp.ID == "" || bp.FileName == "" {
		t.Fatalf("SelectBootstrapPreset returned empty preset: %+v", bp)
	}
	if presets := Presets(); len(presets) == 0 {
		t.Fatalf("expected non-empty curated Presets()")
	}
}

func TestDiscoverHuggingFaceModelsAndRuntimeInstall(t *testing.T) {
	var tarBuf bytes.Buffer
	gzw := gzip.NewWriter(&tarBuf)
	tw := tar.NewWriter(gzw)
	binContent := []byte("#!/bin/sh\necho ok\n")
	if err := tw.WriteHeader(&tar.Header{
		Name: "llama-b9999/" + runtimeBinaryName(),
		Mode: 0o755,
		Size: int64(len(binContent)),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(binContent); err != nil {
		t.Fatal(err)
	}
	_ = tw.Close()
	_ = gzw.Close()
	archiveBytes := tarBuf.Bytes()

	suffix, err := platformAssetSuffix()
	wantSupported := runtime.GOOS == "darwin" || runtime.GOOS == "linux"
	if wantSupported && err != nil {
		t.Fatalf("platformAssetSuffix failed: %v", err)
	}

	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/models":
			_ = json.NewEncoder(w).Encode([]map[string]any{
				{"id": "ggml-org/Qwen-Test-GGUF", "downloads": 900, "lastModified": "2026-11-10T00:00:00Z"},
				{"id": "random/whisper-gguf", "downloads": 5000, "lastModified": "2026-11-12T00:00:00Z"},
			})
		case r.URL.Path == "/api/models/ggml-org/Qwen-Test-GGUF":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id":           "ggml-org/Qwen-Test-GGUF",
				"downloads":    900,
				"lastModified": "2026-11-10T00:00:00Z",
				"siblings": []map[string]any{
					{"rfilename": "qwen-test-Q4_K_M.gguf", "size": int64(2 << 30)},
					{"rfilename": "mmproj-f16.gguf", "size": int64(300 << 20)},
				},
			})
		case r.URL.Path == "/releases":
			_ = json.NewEncoder(w).Encode([]map[string]any{
				{
					"tag_name": "b9999",
					"assets": []map[string]any{
						{"name": "cudart-llama-" + suffix, "browser_download_url": srv.URL + "/bad.tar.gz"},
						{"name": "llama-b9999-" + suffix, "browser_download_url": srv.URL + "/runtime.tar.gz"},
					},
				},
			})
		case r.URL.Path == "/runtime.tar.gz":
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(archiveBytes)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	t.Setenv("HF_API_BASE", srv.URL)
	items, err := DiscoverHuggingFaceModels(context.Background(), "popular", "qwen", true)
	if err != nil || len(items) != 1 || items[0].FileName != "qwen-test-Q4_K_M.gguf" || !items[0].Live {
		t.Fatalf("DiscoverHuggingFaceModels mismatch: items=%+v err=%v", items, err)
	}
	cached, err := DiscoverHuggingFaceModels(context.Background(), "popular", "qwen", false)
	if err != nil || len(cached) != 1 {
		t.Fatalf("DiscoverHuggingFaceModels cache mismatch: items=%+v err=%v", cached, err)
	}

	if wantSupported {
		t.Setenv("LLAMA_RELEASE_API_URL", srv.URL+"/releases")
		mgr := NewManager(t.TempDir())
		if err := mgr.InstallRuntimeSync(context.Background()); err != nil {
			t.Fatalf("InstallRuntimeSync failed: %v", err)
		}
		if rt := mgr.RuntimeBinary(); rt == "" {
			t.Fatalf("expected installed RuntimeBinary after InstallRuntimeSync")
		}
		if _, err := mgr.StartServer(context.Background(), "../bad.gguf", 18089); err == nil {
			t.Fatalf("expected StartServer to reject traversal filename or missing path")
		}
		_ = mgr.StopServer()
	}
}

func buildMockLlamaRuntimeTarGz(t *testing.T) []byte {
	t.Helper()
	srcDir := t.TempDir()
	srcFile := filepath.Join(srcDir, "main.go")
	binFile := filepath.Join(srcDir, "llama-server")
	goSrc := `package main

import (
	"io"
	"net/http"
	"os"
)

func main() {
	port := "18099"
	for i := 1; i < len(os.Args)-1; i++ {
		if os.Args[i] == "--port" {
			port = os.Args[i+1]
		}
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte("{\"status\":\"ok\"}"))
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte("{\"choices\":[{\"message\":{\"content\":\"<think></think>{\\\"items\\\":[{\\\"id\\\":1,\\\"score\\\":4.9}]}\"},\"logprobs\":{\"content\":[{\"token\":\"{\",\"logprob\":-0.0001},{\"token\":\"\\\"items\\\"\",\"logprob\":-0.0001},{\"token\":\"4.9\",\"logprob\":-0.05}]}}]}"))
	})
	_ = http.ListenAndServe("127.0.0.1:"+port, mux)
}
`
	if err := os.WriteFile(srcFile, []byte(goSrc), 0o600); err != nil {
		t.Fatalf("write mock llama-server src: %v", err)
	}
	if out, err := exec.Command("go", "build", "-o", binFile, srcFile).CombinedOutput(); err != nil {
		t.Fatalf("build mock llama-server binary: %v\n%s", err, string(out))
	}
	binBytes, err := os.ReadFile(binFile)
	if err != nil {
		t.Fatalf("read mock llama-server binary: %v", err)
	}

	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)

	dylibBody := []byte("dummy-metal-dylib")
	_ = tw.WriteHeader(&tar.Header{Name: "llama-b99999/libggml-metal.0.dylib", Typeflag: tar.TypeReg, Mode: 0o755, Size: int64(len(dylibBody))})
	_, _ = tw.Write(dylibBody)

	_ = tw.WriteHeader(&tar.Header{Name: "llama-b99999/libggml-metal.dylib", Typeflag: tar.TypeSymlink, Linkname: "libggml-metal.0.dylib"})

	_ = tw.WriteHeader(&tar.Header{Name: "llama-b99999/llama-server", Typeflag: tar.TypeReg, Mode: 0o755, Size: int64(len(binBytes))})
	_, _ = tw.Write(binBytes)

	_ = tw.Close()
	_ = gz.Close()
	return buf.Bytes()
}

func TestLocalModelDownloadSpawnLazyWakeAndConcurrentSingleflight(t *testing.T) {
	if !IsSupportedPlatform() {
		t.Skipf("managed llama-server runtime requires macOS/Linux, got %s/%s", runtime.GOOS, runtime.GOARCH)
	}
	assetSuffix := "bin-macos-arm64.tar.gz"
	switch runtime.GOOS + "/" + runtime.GOARCH {
	case "darwin/amd64":
		assetSuffix = "bin-macos-x64.tar.gz"
	case "linux/arm64":
		assetSuffix = "bin-ubuntu-arm64.tar.gz"
	case "linux/amd64":
		assetSuffix = "bin-ubuntu-x64.tar.gz"
	}
	tarBytes := buildMockLlamaRuntimeTarGz(t)

	var hubURL string
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/releases":
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprintf(w, `[{"tag_name":"b99999","assets":[{"name":"llama-b99999-%s","browser_download_url":"%s/runtime.tar.gz"}]}]`, assetSuffix, hubURL)
		case "/runtime.tar.gz":
			w.Header().Set("Content-Type", "application/gzip")
			w.Header().Set("Content-Length", fmt.Sprintf("%d", len(tarBytes)))
			_, _ = w.Write(tarBytes)
		case "/tiny-test.gguf":
			payload := bytes.Repeat([]byte("GGUF"), 1024)
			w.Header().Set("Content-Length", fmt.Sprintf("%d", len(payload)))
			_, _ = w.Write(payload)
		default:
			http.NotFound(w, r)
		}
	}))
	defer hub.Close()
	hubURL = hub.URL

	t.Setenv("LLAMA_RELEASE_API_URL", hub.URL+"/releases")
	t.Setenv("PATH", "/usr/bin:/bin")

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("alloc free port: %v", err)
	}
	testPort := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()

	modelsDir := t.TempDir()
	mgr := NewManager(modelsDir)
	defer mgr.StopServer()

	ctx := context.Background()
	if _, err := mgr.StartModelDownload("", "http://example.com/evil.gguf"); err == nil {
		t.Fatalf("expected non-https external URL to be rejected")
	}
	if _, err := mgr.StartModelDownload("", hub.URL+"/tiny-test.gguf"); err != nil {
		t.Fatalf("StartModelDownload failed: %v", err)
	}
	for i := 0; i < 40; i++ {
		st := mgr.Status(testPort)
		if !st.Download.Active && len(st.Installed) == 1 {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	if len(mgr.Status(testPort).Installed) != 1 {
		t.Fatalf("expected tiny-test.gguf installed, got %+v", mgr.Status(testPort))
	}

	ep, err := mgr.StartServer(ctx, "tiny-test.gguf", testPort)
	if err != nil || ep == "" {
		t.Fatalf("StartServer failed: ep=%q err=%v", ep, err)
	}
	if target, err := os.Readlink(filepath.Join(modelsDir, "runtime", "libggml-metal.dylib")); err != nil || target != "libggml-metal.0.dylib" {
		t.Fatalf("expected extracted dylib symlink, got %q (%v)", target, err)
	}

	ev := providers.NewGGUFAdapter(ep, "tiny-test.gguf", func(wctx context.Context) error {
		return mgr.EnsureRunning(wctx, "tiny-test.gguf", testPort)
	})
	facts := []domain.Fact{{ID: 1, Scope: "project:hotswap", Category: "architecture", Content: "GGUF test"}}
	scored, err := ev.ScoreRelevance(ctx, "로컬 모델 테스트", facts)
	if err != nil || len(scored) != 1 || scored[0].Score != 4.9 {
		t.Fatalf("expected GGUF ScoreRelevance 4.9, got %+v (%v)", scored, err)
	}

	_ = mgr.StopServer()
	wokenScored, err := ev.ScoreRelevance(ctx, "재기동 자동 웨이크업 테스트", facts)
	if err != nil || len(wokenScored) != 1 || wokenScored[0].Score != 4.9 {
		t.Fatalf("expected lazy auto-wake to score 4.9, got %+v (%v)", wokenScored, err)
	}

	if err := mgr.DeleteModel("tiny-test.gguf"); err != nil {
		t.Fatalf("DeleteModel failed: %v", err)
	}
	if len(mgr.Status(testPort).Installed) != 0 {
		t.Fatalf("expected 0 installed models after delete")
	}

	preset := SelectBootstrapPreset()
	if preset.ID == "" || preset.Tier != "speed" || !strings.HasSuffix(preset.FileName, ".gguf") {
		t.Fatalf("expected valid speed-tier bootstrap preset, got %+v", preset)
	}

	if err := os.WriteFile(filepath.Join(modelsDir, "race.gguf"), []byte("GGUF-MOCK"), 0o644); err != nil {
		t.Fatalf("write race.gguf: %v", err)
	}
	ln2, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("alloc race port: %v", err)
	}
	racePort := ln2.Addr().(*net.TCPAddr).Port
	_ = ln2.Close()

	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := mgr.EnsureRunning(ctx, "race.gguf", racePort); err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Fatalf("concurrent EnsureRunning error: %v", e)
	}
	st := mgr.Status(racePort)
	if !st.ServerRunning || st.ServerPID <= 0 {
		t.Fatalf("expected single running server after concurrent EnsureRunning, got running=%v pid=%d", st.ServerRunning, st.ServerPID)
	}
	stateInfo, err := os.Stat(filepath.Join(modelsDir, "runtime", "llama-server.json"))
	if err != nil || stateInfo.Mode().Perm() != 0o600 {
		t.Fatalf("expected llama-server.json mode 0600, got info=%v err=%v", stateInfo, err)
	}
}
