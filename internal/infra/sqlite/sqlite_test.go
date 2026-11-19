package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"memorize/internal/domain"
	"memorize/internal/infra/config"
)

func TestInitSchemaDropsLegacyProgressStatusColumn(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "legacy.db")
	rawDB, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}

	legacyDDL := `
	CREATE TABLE project_progress (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		scope TEXT NOT NULL,
		category TEXT NOT NULL DEFAULT 'general',
		sub_category TEXT NOT NULL DEFAULT '',
		track TEXT NOT NULL DEFAULT '',
		status TEXT NOT NULL DEFAULT 'done',
		summary TEXT NOT NULL,
		details TEXT NOT NULL DEFAULT '',
		next_step TEXT NOT NULL DEFAULT '',
		source TEXT NOT NULL DEFAULT 'agent',
		commit_hash TEXT NOT NULL DEFAULT '',
		memory_ids TEXT NOT NULL DEFAULT '',
		created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now')),
		updated_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now'))
	);
	CREATE INDEX idx_progress_scope_cat ON project_progress(scope, category, status, id DESC);
	CREATE INDEX idx_progress_scope_track ON project_progress(scope, track, status, id DESC);
	INSERT INTO project_progress (scope, category, track, status, summary, source)
	VALUES
		('project:demo', 'backend', 'auth', 'done', 'Initial auth work', 'agent'),
		('project:demo', 'backend', 'api', 'in_progress', 'Legacy in-progress row', 'agent');
	`
	if _, err := rawDB.Exec(legacyDDL); err != nil {
		_ = rawDB.Close()
		t.Fatalf("seed legacy schema: %v", err)
	}
	_ = rawDB.Close()

	db, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open (with InitSchema migration): %v", err)
	}
	defer db.Close()

	if err := InitSchema(db); err != nil {
		t.Fatalf("InitSchema (idempotent 2nd): %v", err)
	}

	rows, err := db.Query(`PRAGMA table_info(project_progress)`)
	if err != nil {
		t.Fatalf("PRAGMA table_info: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var cid int
		var name, colType string
		var notNull, pk int
		var dflt any
		if err := rows.Scan(&cid, &name, &colType, &notNull, &dflt, &pk); err != nil {
			t.Fatalf("scan table_info: %v", err)
		}
		if name == "status" {
			t.Fatalf("expected status column to be dropped from project_progress")
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("table_info rows: %v", err)
	}

	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM project_progress WHERE scope = 'project:demo'`).Scan(&count); err != nil {
		t.Fatalf("count surviving rows: %v", err)
	}
	if count != 2 {
		t.Fatalf("expected 2 pre-existing rows to survive migration, got %d", count)
	}

	if _, err := db.Exec(`INSERT INTO project_progress (scope, category, track, summary, source) VALUES ('project:demo', 'tests', 'sqlite', 'Post-migration insert', 'agent')`); err != nil {
		t.Fatalf("insert without status column: %v", err)
	}

	var idxCount int
	if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'index' AND tbl_name = 'project_progress' AND name IN ('idx_progress_scope_cat', 'idx_progress_scope_track')`).Scan(&idxCount); err != nil {
		t.Fatalf("check recreated indexes: %v", err)
	}
	if idxCount != 2 {
		t.Fatalf("expected 2 recreated progress indexes, got %d", idxCount)
	}
}

func TestOpenConcurrentWALReadWrite(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "concurrent.db")
	db, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	if stats := db.Stats(); stats.MaxOpenConnections != maxDiskOpenConns {
		t.Fatalf("expected MaxOpenConnections=%d for disk DB, got %d", maxDiskOpenConns, stats.MaxOpenConnections)
	}

	ctx := context.Background()
	var wg sync.WaitGroup
	errCh := make(chan error, 64)

	for w := 0; w < 4; w++ {
		w := w
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 20; i++ {
				err := WithTx(ctx, db, func(tx *sql.Tx) error {
					var cnt int
					if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM memories`).Scan(&cnt); err != nil {
						return err
					}
					_, err := tx.ExecContext(ctx,
						`INSERT INTO memories (scope, account, category, content, confidence, pinned, active) VALUES ('project:conc', 'u', 'architecture', ?, 0.9, 0, 1)`,
						fmt.Sprintf("rule-%d-%d", w, i))
					return err
				})
				if err != nil {
					errCh <- err
					return
				}
			}
		}()
	}
	for r := 0; r < 8; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 40; i++ {
				rows, err := db.QueryContext(ctx, `SELECT id, content FROM memories WHERE scope = 'project:conc' LIMIT 10`)
				if err != nil {
					errCh <- err
					return
				}
				for rows.Next() {
					var id int64
					var c string
					_ = rows.Scan(&id, &c)
				}
				_ = rows.Close()
			}
		}()
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Fatalf("concurrent WAL read/write error: %v", err)
	}

	var total int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM memories WHERE scope = 'project:conc'`).Scan(&total); err != nil || total != 80 {
		t.Fatalf("expected 80 committed rows, got %d (err=%v)", total, err)
	}
}

func openTestUsersDB(t *testing.T) (*sql.DB, context.Context) {
	t.Helper()
	db, err := Open(":memory:")
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	if err := InitSchema(db); err != nil {
		_ = db.Close()
		t.Fatalf("sqlite.InitSchema: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db, context.Background()
}

func TestUsersCRUDTokenRotationCookiesAndLastAdminGuard(t *testing.T) {
	db, ctx := openTestUsersDB(t)

	if _, err := db.ExecContext(ctx, `INSERT INTO memories (scope, account, category, content, confidence, pinned, active) VALUES ('personal', '', 'convention', 'Legacy note', 0.9, 0, 1)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO memories (scope, account, category, content, confidence, pinned, active) VALUES ('global', '', 'architecture', 'Legacy global dev rule', 0.9, 1, 1)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO project_progress (scope, account, category, summary) VALUES ('global', '', 'dev', 'Legacy global prog')`); err != nil {
		t.Fatal(err)
	}

	adminIssue, created, err := EnsureBootstrapAdmin(ctx, db, "hostadmin", "")
	if err != nil || !created {
		t.Fatalf("EnsureBootstrapAdmin first: created=%v err=%v", created, err)
	}
	var claimedAcc, claimedGlobAcc, claimedProgAcc string
	if err := db.QueryRowContext(ctx, `SELECT account FROM memories WHERE content = 'Legacy note'`).Scan(&claimedAcc); err != nil || claimedAcc != "hostadmin" {
		t.Fatalf("expected legacy blank-account personal memory claimed by hostadmin, got %q (err=%v)", claimedAcc, err)
	}
	if err := db.QueryRowContext(ctx, `SELECT account FROM memories WHERE content = 'Legacy global dev rule'`).Scan(&claimedGlobAcc); err != nil || claimedGlobAcc != "hostadmin" {
		t.Fatalf("expected legacy blank-account global memory claimed by hostadmin, got %q (err=%v)", claimedGlobAcc, err)
	}
	if err := db.QueryRowContext(ctx, `SELECT account FROM project_progress WHERE summary = 'Legacy global prog'`).Scan(&claimedProgAcc); err != nil || claimedProgAcc != "hostadmin" {
		t.Fatalf("expected legacy blank-account global progress claimed by hostadmin, got %q (err=%v)", claimedProgAcc, err)
	}

	SetStoredRepoPath(ctx, db, "project:memorize", "/tmp/local-repo")
	srvCtx := domain.WithAuthPrincipal(ctx, domain.AuthPrincipal{
		Account:       "hostadmin",
		Role:          domain.RoleAdmin,
		ServerMode:    true,
		Authenticated: true,
	})
	SetStoredRepoPath(srvCtx, db, "project:other", "/tmp/other-repo")
	if resolved := GetStoredRepoPath(srvCtx, db, "project:memorize"); resolved != "" {
		t.Fatalf("expected empty GetStoredRepoPath in ServerMode, got %q", resolved)
	}
	if other := GetStoredRepoPath(ctx, db, "project:other"); other != "" {
		t.Fatalf("expected SetStoredRepoPath in ServerMode to be ignored, got %q", other)
	}
	if adminIssue.User.Account != "hostadmin" || adminIssue.User.Role != domain.RoleAdmin || !strings.HasPrefix(adminIssue.PlainToken, "mem_") {
		t.Fatalf("unexpected bootstrap admin: %+v", adminIssue)
	}
	if _, created2, err := EnsureBootstrapAdmin(ctx, db, "other", "mem_should_ignore"); err != nil || created2 {
		t.Fatalf("expected second EnsureBootstrapAdmin to be no-op, got created=%v err=%v", created2, err)
	}

	pAdmin, ok := AuthenticateToken(ctx, db, adminIssue.PlainToken)
	if !ok || !pAdmin.Authenticated || !pAdmin.ServerMode || pAdmin.Account != "hostadmin" || pAdmin.Role != domain.RoleAdmin {
		t.Fatalf("AuthenticateToken admin failed: ok=%v p=%+v", ok, pAdmin)
	}
	cookieVal, err := IssueSessionCookieValue(ctx, db, pAdmin, time.Hour)
	if err != nil {
		t.Fatalf("IssueSessionCookieValue: %v", err)
	}
	pCookie, ok := AuthenticateSessionCookie(ctx, db, cookieVal)
	if !ok || pCookie.Account != "hostadmin" || pCookie.Role != domain.RoleAdmin {
		t.Fatalf("AuthenticateSessionCookie failed: ok=%v p=%+v", ok, pCookie)
	}
	if _, ok := AuthenticateSessionCookie(ctx, db, cookieVal+"tamper"); ok {
		t.Fatalf("expected tampered cookie to fail")
	}

	if err := DeleteUser(ctx, db, "hostadmin"); err == nil {
		t.Fatalf("expected DeleteUser on last active admin to fail")
	}
	if err := SetUserActive(ctx, db, "hostadmin", false); err == nil {
		t.Fatalf("expected SetUserActive(false) on last active admin to fail")
	}
	if err := UpdateUserRole(ctx, db, "hostadmin", domain.RoleMember); err == nil {
		t.Fatalf("expected UpdateUserRole(member) on last active admin to fail")
	}

	aliceIssue, err := CreateUser(ctx, db, "alice", domain.RoleMember)
	if err != nil {
		t.Fatalf("CreateUser alice: %v", err)
	}
	if aliceIssue.User.Role != domain.RoleMember {
		t.Fatalf("expected role member, got %s", aliceIssue.User.Role)
	}
	pAlice, ok := AuthenticateToken(ctx, db, aliceIssue.PlainToken)
	if !ok || pAlice.Account != "alice" {
		t.Fatalf("AuthenticateToken alice failed: ok=%v", ok)
	}
	aliceCookie, err := IssueSessionCookieValue(ctx, db, pAlice, time.Hour)
	if err != nil {
		t.Fatal(err)
	}

	aliceRot, err := RotateUserToken(ctx, db, "alice")
	if err != nil || aliceRot.PlainToken == aliceIssue.PlainToken {
		t.Fatalf("RotateUserToken failed: err=%v", err)
	}
	if _, ok := AuthenticateToken(ctx, db, aliceIssue.PlainToken); ok {
		t.Fatalf("expected old token invalidated immediately after rotation")
	}
	if _, ok := AuthenticateSessionCookie(ctx, db, aliceCookie); ok {
		t.Fatalf("expected old session cookie invalidated via token_hint mismatch after rotation")
	}
	if pNew, ok := AuthenticateToken(ctx, db, aliceRot.PlainToken); !ok || pNew.Account != "alice" {
		t.Fatalf("expected rotated token to authenticate, ok=%v", ok)
	}

	if err := SetUserActive(ctx, db, "alice", false); err != nil {
		t.Fatalf("SetUserActive false: %v", err)
	}
	if _, ok := AuthenticateToken(ctx, db, aliceRot.PlainToken); ok {
		t.Fatalf("expected suspended user token rejected")
	}
	if err := SetUserActive(ctx, db, "alice", true); err != nil {
		t.Fatalf("SetUserActive true: %v", err)
	}
	if _, ok := AuthenticateToken(ctx, db, aliceRot.PlainToken); !ok {
		t.Fatalf("expected re-enabled user token accepted")
	}

	if err := UpdateUserRole(ctx, db, "alice", domain.RoleAdmin); err != nil {
		t.Fatalf("UpdateUserRole alice admin: %v", err)
	}
	if err := DeleteUser(ctx, db, "hostadmin"); err != nil {
		t.Fatalf("DeleteUser hostadmin after second admin added: %v", err)
	}
	users, err := ListUsers(ctx, db)
	if err != nil || len(users) != 1 || users[0].Account != "alice" {
		t.Fatalf("unexpected users list after delete: %+v (err=%v)", users, err)
	}
}

func TestConfigMapExcludesSessionSecretAndAuthLastSeenThrottle(t *testing.T) {
	db, ctx := openTestUsersDB(t)
	adminIss, _, err := EnsureBootstrapAdmin(ctx, db, "hostadmin", "")
	if err != nil {
		t.Fatalf("EnsureBootstrapAdmin: %v", err)
	}
	iss, err := CreateUser(ctx, db, "alice", domain.RoleMember)
	if err != nil {
		t.Fatalf("CreateUser alice: %v", err)
	}
	pAdm := domain.AuthPrincipal{
		Account:       adminIss.User.Account,
		Role:          adminIss.User.Role,
		TokenHint:     adminIss.User.TokenHint,
		ServerMode:    true,
		Authenticated: true,
	}
	if _, err := IssueSessionCookieValue(ctx, db, pAdm, time.Hour); err != nil {
		t.Fatalf("IssueSessionCookieValue: %v", err)
	}
	cfg := config.GetConfigMap(ctx, db)
	if _, leaked := cfg["auth_session_secret"]; leaked {
		t.Fatalf("config.GetConfigMap leaked auth_session_secret")
	}

	if _, ok := AuthenticateToken(ctx, db, iss.PlainToken); !ok {
		t.Fatalf("expected initial AuthenticateToken to succeed")
	}
	var firstSeen string
	if err := db.QueryRowContext(ctx, `SELECT COALESCE(last_seen_at, '') FROM users WHERE account = 'alice'`).Scan(&firstSeen); err != nil || firstSeen == "" {
		t.Fatalf("expected first last_seen_at populated, got %q (err=%v)", firstSeen, err)
	}

	freshStamp := time.Now().UTC().Add(-10 * time.Second).Format("2006-01-02 15:04:05")
	if _, err := db.ExecContext(ctx, `UPDATE users SET last_seen_at = ? WHERE account = 'alice'`, freshStamp); err != nil {
		t.Fatal(err)
	}
	if _, ok := AuthenticateToken(ctx, db, iss.PlainToken); !ok {
		t.Fatalf("expected fresh-window AuthenticateToken to succeed")
	}
	var gotAfterFresh string
	if err := db.QueryRowContext(ctx, `SELECT COALESCE(last_seen_at, '') FROM users WHERE account = 'alice'`).Scan(&gotAfterFresh); err != nil {
		t.Fatal(err)
	}
	if gotAfterFresh != freshStamp {
		t.Fatalf("expected last_seen_at throttled within 60s window: want %q, got %q", freshStamp, gotAfterFresh)
	}

	staleStamp := time.Now().UTC().Add(-120 * time.Second).Format("2006-01-02 15:04:05")
	if _, err := db.ExecContext(ctx, `UPDATE users SET last_seen_at = ? WHERE account = 'alice'`, staleStamp); err != nil {
		t.Fatal(err)
	}
	if _, ok := AuthenticateToken(ctx, db, iss.PlainToken); !ok {
		t.Fatalf("expected stale-window AuthenticateToken to succeed")
	}
	var gotAfterStale string
	if err := db.QueryRowContext(ctx, `SELECT COALESCE(last_seen_at, '') FROM users WHERE account = 'alice'`).Scan(&gotAfterStale); err != nil {
		t.Fatal(err)
	}
	if gotAfterStale == staleStamp || gotAfterStale == "" {
		t.Fatalf("expected stale last_seen_at refreshed, still got %q", gotAfterStale)
	}
}

func TestLegacyConfigSessionSecretMigratesToInternalSecrets(t *testing.T) {
	db, ctx := openTestUsersDB(t)
	admIss, _, err := EnsureBootstrapAdmin(ctx, db, "hostadmin", "")
	if err != nil {
		t.Fatalf("EnsureBootstrapAdmin: %v", err)
	}
	cookieVal, err := IssueSessionCookieValue(ctx, db, domain.AuthPrincipal{
		Account:       admIss.User.Account,
		Role:          admIss.User.Role,
		TokenHint:     admIss.User.TokenHint,
		ServerMode:    true,
		Authenticated: true,
	}, domain.DefaultSessionTTL)
	if err != nil {
		t.Fatalf("IssueSessionCookieValue: %v", err)
	}

	if _, err := db.ExecContext(ctx, `INSERT INTO config (key, val) SELECT key, val FROM internal_secrets WHERE key = 'auth_session_secret'`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `DELETE FROM internal_secrets`); err != nil {
		t.Fatal(err)
	}
	if err := InitSchema(db); err != nil {
		t.Fatalf("InitSchema migration: %v", err)
	}
	if _, existsInCfg := config.GetConfigMap(ctx, db)["auth_session_secret"]; existsInCfg {
		t.Fatalf("expected auth_session_secret removed from config after migration")
	}
	var cnt int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM internal_secrets WHERE key = 'auth_session_secret'`).Scan(&cnt); err != nil || cnt != 1 {
		t.Fatalf("expected migrated secret in internal_secrets, count=%d err=%v", cnt, err)
	}
	if p, ok := AuthenticateSessionCookie(ctx, db, cookieVal); !ok || p.Account != "hostadmin" {
		t.Fatalf("expected pre-migration session cookie valid after migration, ok=%v p=%+v", ok, p)
	}
}
