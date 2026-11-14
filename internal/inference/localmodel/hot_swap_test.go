package localmodel_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
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
	"memorize/internal/inference/localmodel"
	"memorize/internal/inference/providers"
)

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
	if !localmodel.IsSupportedPlatform() {
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
	mgr := localmodel.NewManager(modelsDir)
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

	// Stop server and verify lazy auto-wake via EnsureRunning hook
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

	// Verify bootstrap preset selection
	preset := localmodel.SelectBootstrapPreset()
	if preset.ID == "" || preset.Tier != "speed" || !strings.HasSuffix(preset.FileName, ".gguf") {
		t.Fatalf("expected valid speed-tier bootstrap preset, got %+v", preset)
	}

	// Verify 8-way concurrent EnsureRunning singleflight + 0600 llama-server.json
	if err := os.WriteFile(filepath.Join(modelsDir, "race.gguf"), []byte("GGUF-MOCK"), 0o644); err != nil {
		t.Fatalf("write race.gguf: %v", err)
	}
	ln2, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("alloc race port: %v", err)
	}
	racePort := ln2.Addr().(*net.TCPAddr).Port
	_ = ln2.Close()

	// Point managed runtime to an in-repo python mock server would need PATH; instead reuse the compiled mock llama-server binary already in runtimeDir!
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
