package localmodel

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
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
		// Exceeds apiClient.Timeout (40ms) during body streaming
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

	// Verify pickAvailableGGUFPort skips both busy port and exclude port (daemon port).
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

	// Mock a healthy llama-server on an ephemeral fallback port and verify Status + EnsureRunningEndpoint reuse it.
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
