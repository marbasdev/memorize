package sqlite_test

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"

	"memorize/internal/domain"
	"memorize/internal/infra/config"
	"memorize/internal/infra/sqlite"
)

func openTestUsersDB(t *testing.T) (*sql.DB, context.Context) {
	t.Helper()
	db, err := sqlite.Open(":memory:")
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	if err := sqlite.InitSchema(db); err != nil {
		_ = db.Close()
		t.Fatalf("sqlite.InitSchema: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db, context.Background()
}

func TestUsersCRUDTokenRotationCookiesAndLastAdminGuard(t *testing.T) {
	db, ctx := openTestUsersDB(t)

	// Legacy empty-account personal & global rows should be claimed by initial bootstrap admin cleanly.
	if _, err := db.ExecContext(ctx, `INSERT INTO memories (scope, account, category, content, confidence, pinned, active) VALUES ('personal', '', 'convention', 'Legacy note', 0.9, 0, 1)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO memories (scope, account, category, content, confidence, pinned, active) VALUES ('global', '', 'architecture', 'Legacy global dev rule', 0.9, 1, 1)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO project_progress (scope, account, category, summary) VALUES ('global', '', 'dev', 'Legacy global prog')`); err != nil {
		t.Fatal(err)
	}

	// Bootstrap admin created once only when table is empty.
	adminIssue, created, err := sqlite.EnsureBootstrapAdmin(ctx, db, "hostadmin", "")
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

	// ServerMode never probes host filesystem for client repository paths.
	srvCtx := domain.WithAuthPrincipal(ctx, domain.AuthPrincipal{
		Account:       "hostadmin",
		Role:          domain.RoleAdmin,
		ServerMode:    true,
		Authenticated: true,
	})
	if resolved := sqlite.ResolveScopeRepoPath(srvCtx, db, "project:memorize", "."); resolved != "" {
		t.Fatalf("expected empty ResolveScopeRepoPath in ServerMode, got %q", resolved)
	}
	if adminIssue.User.Account != "hostadmin" || adminIssue.User.Role != domain.RoleAdmin || !strings.HasPrefix(adminIssue.PlainToken, "mem_") {
		t.Fatalf("unexpected bootstrap admin: %+v", adminIssue)
	}
	if _, created2, err := sqlite.EnsureBootstrapAdmin(ctx, db, "other", "mem_should_ignore"); err != nil || created2 {
		t.Fatalf("expected second EnsureBootstrapAdmin to be no-op, got created=%v err=%v", created2, err)
	}

	// Authenticate via plaintext token & signed session cookie.
	pAdmin, ok := sqlite.AuthenticateToken(ctx, db, adminIssue.PlainToken)
	if !ok || !pAdmin.Authenticated || !pAdmin.ServerMode || pAdmin.Account != "hostadmin" || pAdmin.Role != domain.RoleAdmin {
		t.Fatalf("AuthenticateToken admin failed: ok=%v p=%+v", ok, pAdmin)
	}
	cookieVal, err := sqlite.IssueSessionCookieValue(ctx, db, pAdmin, time.Hour)
	if err != nil {
		t.Fatalf("IssueSessionCookieValue: %v", err)
	}
	pCookie, ok := sqlite.AuthenticateSessionCookie(ctx, db, cookieVal)
	if !ok || pCookie.Account != "hostadmin" || pCookie.Role != domain.RoleAdmin {
		t.Fatalf("AuthenticateSessionCookie failed: ok=%v p=%+v", ok, pCookie)
	}
	if _, ok := sqlite.AuthenticateSessionCookie(ctx, db, cookieVal+"tamper"); ok {
		t.Fatalf("expected tampered cookie to fail")
	}

	// Cannot delete, disable, or demote the sole active admin.
	if err := sqlite.DeleteUser(ctx, db, "hostadmin"); err == nil {
		t.Fatalf("expected DeleteUser on last active admin to fail")
	}
	if err := sqlite.SetUserActive(ctx, db, "hostadmin", false); err == nil {
		t.Fatalf("expected SetUserActive(false) on last active admin to fail")
	}
	if err := sqlite.UpdateUserRole(ctx, db, "hostadmin", domain.RoleMember); err == nil {
		t.Fatalf("expected UpdateUserRole(member) on last active admin to fail")
	}

	// Create member Alice; test token rotation invalidates previous token and cookie immediately.
	aliceIssue, err := sqlite.CreateUser(ctx, db, "alice", domain.RoleMember)
	if err != nil {
		t.Fatalf("CreateUser alice: %v", err)
	}
	if aliceIssue.User.Role != domain.RoleMember {
		t.Fatalf("expected role member, got %s", aliceIssue.User.Role)
	}
	pAlice, ok := sqlite.AuthenticateToken(ctx, db, aliceIssue.PlainToken)
	if !ok || pAlice.Account != "alice" {
		t.Fatalf("AuthenticateToken alice failed: ok=%v", ok)
	}
	aliceCookie, err := sqlite.IssueSessionCookieValue(ctx, db, pAlice, time.Hour)
	if err != nil {
		t.Fatal(err)
	}

	aliceRot, err := sqlite.RotateUserToken(ctx, db, "alice")
	if err != nil || aliceRot.PlainToken == aliceIssue.PlainToken {
		t.Fatalf("RotateUserToken failed: err=%v", err)
	}
	if _, ok := sqlite.AuthenticateToken(ctx, db, aliceIssue.PlainToken); ok {
		t.Fatalf("expected old token invalidated immediately after rotation")
	}
	if _, ok := sqlite.AuthenticateSessionCookie(ctx, db, aliceCookie); ok {
		t.Fatalf("expected old session cookie invalidated via token_hint mismatch after rotation")
	}
	if pNew, ok := sqlite.AuthenticateToken(ctx, db, aliceRot.PlainToken); !ok || pNew.Account != "alice" {
		t.Fatalf("expected rotated token to authenticate, ok=%v", ok)
	}

	// Suspending disables both token & cookie immediately; re-enabling restores rotated token.
	if err := sqlite.SetUserActive(ctx, db, "alice", false); err != nil {
		t.Fatalf("SetUserActive false: %v", err)
	}
	if _, ok := sqlite.AuthenticateToken(ctx, db, aliceRot.PlainToken); ok {
		t.Fatalf("expected suspended user token rejected")
	}
	if err := sqlite.SetUserActive(ctx, db, "alice", true); err != nil {
		t.Fatalf("SetUserActive true: %v", err)
	}
	if _, ok := sqlite.AuthenticateToken(ctx, db, aliceRot.PlainToken); !ok {
		t.Fatalf("expected re-enabled user token accepted")
	}

	// Promoting Alice to admin allows demoting/deleting original hostadmin cleanly.
	if err := sqlite.UpdateUserRole(ctx, db, "alice", domain.RoleAdmin); err != nil {
		t.Fatalf("UpdateUserRole alice admin: %v", err)
	}
	if err := sqlite.DeleteUser(ctx, db, "hostadmin"); err != nil {
		t.Fatalf("DeleteUser hostadmin after second admin added: %v", err)
	}
	users, err := sqlite.ListUsers(ctx, db)
	if err != nil || len(users) != 1 || users[0].Account != "alice" {
		t.Fatalf("unexpected users list after delete: %+v (err=%v)", users, err)
	}
}

func TestConfigMapExcludesSessionSecretAndAuthLastSeenThrottle(t *testing.T) {
	db, ctx := openTestUsersDB(t)
	adminIss, _, err := sqlite.EnsureBootstrapAdmin(ctx, db, "hostadmin", "")
	if err != nil {
		t.Fatalf("EnsureBootstrapAdmin: %v", err)
	}
	iss, err := sqlite.CreateUser(ctx, db, "alice", domain.RoleMember)
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
	if _, err := sqlite.IssueSessionCookieValue(ctx, db, pAdm, time.Hour); err != nil {
		t.Fatalf("IssueSessionCookieValue: %v", err)
	}
	cfg := config.GetConfigMap(ctx, db)
	if _, leaked := cfg["auth_session_secret"]; leaked {
		t.Fatalf("config.GetConfigMap leaked auth_session_secret")
	}

	// First AuthenticateToken populates empty last_seen_at.
	if _, ok := sqlite.AuthenticateToken(ctx, db, iss.PlainToken); !ok {
		t.Fatalf("expected initial AuthenticateToken to succeed")
	}
	var firstSeen string
	if err := db.QueryRowContext(ctx, `SELECT COALESCE(last_seen_at, '') FROM users WHERE account = 'alice'`).Scan(&firstSeen); err != nil || firstSeen == "" {
		t.Fatalf("expected first last_seen_at populated, got %q (err=%v)", firstSeen, err)
	}

	// Recent timestamp (<60s old) skips write inside AuthenticateToken.
	freshStamp := time.Now().UTC().Add(-10 * time.Second).Format("2006-01-02 15:04:05")
	if _, err := db.ExecContext(ctx, `UPDATE users SET last_seen_at = ? WHERE account = 'alice'`, freshStamp); err != nil {
		t.Fatal(err)
	}
	if _, ok := sqlite.AuthenticateToken(ctx, db, iss.PlainToken); !ok {
		t.Fatalf("expected fresh-window AuthenticateToken to succeed")
	}
	var gotAfterFresh string
	if err := db.QueryRowContext(ctx, `SELECT COALESCE(last_seen_at, '') FROM users WHERE account = 'alice'`).Scan(&gotAfterFresh); err != nil {
		t.Fatal(err)
	}
	if gotAfterFresh != freshStamp {
		t.Fatalf("expected last_seen_at throttled within 60s window: want %q, got %q", freshStamp, gotAfterFresh)
	}

	// Stale timestamp (>=60s old) triggers last_seen_at refresh.
	staleStamp := time.Now().UTC().Add(-120 * time.Second).Format("2006-01-02 15:04:05")
	if _, err := db.ExecContext(ctx, `UPDATE users SET last_seen_at = ? WHERE account = 'alice'`, staleStamp); err != nil {
		t.Fatal(err)
	}
	if _, ok := sqlite.AuthenticateToken(ctx, db, iss.PlainToken); !ok {
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
	admIss, _, err := sqlite.EnsureBootstrapAdmin(ctx, db, "hostadmin", "")
	if err != nil {
		t.Fatalf("EnsureBootstrapAdmin: %v", err)
	}
	cookieVal, err := sqlite.IssueSessionCookieValue(ctx, db, domain.AuthPrincipal{
		Account:       admIss.User.Account,
		Role:          admIss.User.Role,
		TokenHint:     admIss.User.TokenHint,
		ServerMode:    true,
		Authenticated: true,
	}, domain.DefaultSessionTTL)
	if err != nil {
		t.Fatalf("IssueSessionCookieValue: %v", err)
	}

	// Simulate legacy DB where secret lived inside config and internal_secrets did not have it.
	if _, err := db.ExecContext(ctx, `INSERT INTO config (key, val) SELECT key, val FROM internal_secrets WHERE key = 'auth_session_secret'`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `DELETE FROM internal_secrets`); err != nil {
		t.Fatal(err)
	}
	// Run InitSchema to verify one-time migration out of config -> internal_secrets.
	if err := sqlite.InitSchema(db); err != nil {
		t.Fatalf("InitSchema migration: %v", err)
	}
	if _, existsInCfg := config.GetConfigMap(ctx, db)["auth_session_secret"]; existsInCfg {
		t.Fatalf("expected auth_session_secret removed from config after migration")
	}
	var cnt int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM internal_secrets WHERE key = 'auth_session_secret'`).Scan(&cnt); err != nil || cnt != 1 {
		t.Fatalf("expected migrated secret in internal_secrets, count=%d err=%v", cnt, err)
	}
	if p, ok := sqlite.AuthenticateSessionCookie(ctx, db, cookieVal); !ok || p.Account != "hostadmin" {
		t.Fatalf("expected pre-migration session cookie valid after migration, ok=%v p=%+v", ok, p)
	}
}
