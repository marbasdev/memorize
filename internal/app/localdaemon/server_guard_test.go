package localdaemon_test

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"memorize/internal/app/localdaemon"
	"memorize/internal/domain"
	"memorize/internal/infra/config"
)

func TestEnsureRunningSkipsDuplicateDaemonWhenTeamServerActive(t *testing.T) {
	tmp := t.TempDir()
	dbPath := filepath.Join(tmp, "memories.db")
	modelsDir := filepath.Join(tmp, "models")

	// Simulate an active authenticated `memorize serve` team server returning 401 without Bearer token.
	authSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/daemon/status" {
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
			return
		}
		http.NotFound(w, r)
	}))
	defer authSrv.Close()

	listenAddr := strings.TrimPrefix(authSrv.URL, "http://")
	if err := config.WriteDaemonState(dbPath, domain.DaemonRuntimeState{
		PID:        99999,
		ListenAddr: listenAddr,
		ServerMode: true,
		DBPath:     dbPath,
	}); err != nil {
		t.Fatal(err)
	}

	st, ok := localdaemon.EnsureRunning(listenAddr, dbPath, modelsDir)
	if ok {
		t.Fatalf("expected EnsureRunning not to treat authenticated team server as unauthenticated solo daemon")
	}
	if st == nil || !st.ServerMode || !st.Running {
		t.Fatalf("expected authRequired team server status hint, got %+v", st)
	}
}
