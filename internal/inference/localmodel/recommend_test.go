package localmodel

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

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

	// Installed GGUF listing skips directories & non-gguf files
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

	// DeleteModel rejects traversal and deletes valid model
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

	// StartModelDownload validation
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
	// Build a minimal tar.gz containing a fake llama-server binary
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
	// Cached lookup returns identical results
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
