package sqlite_test

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"

	"memorize/internal/domain"
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

	// Legacy empty-account personal memories should be claimed by initial bootstrap admin cleanly.
	if _, err := db.ExecContext(ctx, `INSERT INTO memories (scope, account, category, content, confidence, pinned, active) VALUES ('personal', '', 'convention', 'Legacy note', 0.9, 0, 1)`); err != nil {
		t.Fatal(err)
	}

	// Bootstrap admin created once only when table is empty.
	adminIssue, created, err := sqlite.EnsureBootstrapAdmin(ctx, db, "hostadmin", "")
	if err != nil || !created {
		t.Fatalf("EnsureBootstrapAdmin first: created=%v err=%v", created, err)
	}
	var claimedAcc string
	if err := db.QueryRowContext(ctx, `SELECT account FROM memories WHERE content = 'Legacy note'`).Scan(&claimedAcc); err != nil || claimedAcc != "hostadmin" {
		t.Fatalf("expected legacy blank-account personal memory claimed by hostadmin, got %q (err=%v)", claimedAcc, err)
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
