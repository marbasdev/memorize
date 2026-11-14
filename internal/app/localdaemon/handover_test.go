package localdaemon

import (
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"memorize/internal/infra/config"
	"memorize/internal/modules/guardrail"
	"memorize/internal/modules/memory"
	"memorize/internal/modules/progress"
	"memorize/internal/transport/dashboard"
)

func TestWatchedBinaryCandidatesAndDevHotReloadGate(t *testing.T) {
	t.Setenv("MEMORY_DAEMON_BIN", "")
	t.Setenv("MEMORIZE_DEV_HOTRELOAD", "")

	repoRoot, err := filepath.Abs(filepath.Join("..", "..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	binName := "memorize"
	if runtime.GOOS == "windows" {
		binName = "memorize.exe"
	}
	repoBin := filepath.Join(repoRoot, "bin", binName)

	// Without MEMORIZE_DEV_HOTRELOAD=1, release safety requires CWD/repo bin/memorize NOT to be watched (#126).
	for _, cand := range WatchedBinaryCandidates() {
		if config.SamePathClean(cand, repoBin) {
			t.Fatalf("did not expect repo binary %s in WatchedBinaryCandidates when MEMORIZE_DEV_HOTRELOAD is unset; got %v", repoBin, WatchedBinaryCandidates())
		}
	}

	// With MEMORIZE_DEV_HOTRELOAD=1, repo-root bin/memorize IS included.
	t.Setenv("MEMORIZE_DEV_HOTRELOAD", "1")
	foundRepoBin := false
	for _, cand := range WatchedBinaryCandidates() {
		if config.SamePathClean(cand, repoBin) {
			foundRepoBin = true
			break
		}
	}
	if !foundRepoBin {
		t.Fatalf("expected %s in WatchedBinaryCandidates when MEMORIZE_DEV_HOTRELOAD=1; got %v", repoBin, WatchedBinaryCandidates())
	}

	// Explicit MEMORY_DAEMON_BIN overrides all discovery candidates.
	custom := filepath.Join(t.TempDir(), "custom-memorize")
	t.Setenv("MEMORY_DAEMON_BIN", custom)
	cands := WatchedBinaryCandidates()
	if len(cands) == 0 || !strings.HasSuffix(cands[0], "custom-memorize") {
		t.Fatalf("expected MEMORY_DAEMON_BIN override, got %v", cands)
	}
}

func TestDaemonLogPathAndStartBackgroundServerHandover(t *testing.T) {
	tmp := t.TempDir()
	customDB := filepath.Join(tmp, "memories.db")
	t.Setenv("MEMORY_DAEMON_LOG", "")
	if got := config.DaemonLogPath(customDB); got != filepath.Join(tmp, "daemon.log") {
		t.Fatalf("DaemonLogPath = %q, want %q", got, filepath.Join(tmp, "daemon.log"))
	}

	mem, err := memory.Open(":memory:", nil)
	if err != nil {
		t.Fatalf("memory.Open: %v", err)
	}
	defer mem.Close()
	prog := progress.New(mem)
	gr := guardrail.New(mem, prog)

	deps := dashboard.Deps{
		Memory:    mem,
		Progress:  prog,
		Guardrail: gr,
	}

	// Occupy an ephemeral port with a yieldable server, then verify StartBackgroundServer takes over cleanly.
	lnProbe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := lnProbe.Addr().String()
	_ = lnProbe.Close()

	bound1, close1, err := StartBackgroundServer(addr, deps, customDB)
	if err != nil || bound1 == "" || close1 == nil {
		t.Fatalf("StartBackgroundServer #1 failed: bound=%q err=%v", bound1, err)
	}
	defer func() { _ = close1() }()

	resp, err := http.Get("http://" + bound1 + "/api/dashboard/meta")
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 on /api/dashboard/meta, got resp=%v err=%v", resp, err)
	}
	_ = resp.Body.Close()

	// ListenWithHandover(onlyIfNewer=false) yields the first server cleanly and rebinds the same port.
	ln2, err := ListenWithHandover(bound1, false)
	if err != nil {
		t.Fatalf("ListenWithHandover takeover failed: %v", err)
	}
	_ = ln2.Close()

	// Brief yield check for server goroutine teardown.
	time.Sleep(20 * time.Millisecond)
	_ = os.RemoveAll(tmp)
}
