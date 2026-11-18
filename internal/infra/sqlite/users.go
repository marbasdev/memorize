package sqlite

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"memorize/internal/domain"
)

// HashAuthToken returns the lowercase SHA-256 hex digest of a plaintext bearer token.
func HashAuthToken(plain string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(plain)))
	return hex.EncodeToString(sum[:])
}

func generateRandomToken() (plain, hash, hint string, err error) {
	buf := make([]byte, 24)
	if _, err = rand.Read(buf); err != nil {
		return "", "", "", err
	}
	plain = "mem_" + hex.EncodeToString(buf)
	hash = HashAuthToken(plain)
	hint = domain.FormatTokenHint(plain)
	return plain, hash, hint, nil
}

func activeAdminCountTx(ctx context.Context, tx *sql.Tx, excludeAccount string) (int, error) {
	var cnt int
	err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM users WHERE active = 1 AND role = ? AND account != ?`,
		domain.RoleAdmin, excludeAccount).Scan(&cnt)
	return cnt, err
}

// CreateUser registers a new team server account and returns a once-shown plaintext token.
func CreateUser(ctx context.Context, db *sql.DB, rawAccount, rawRole string) (domain.UserTokenIssue, error) {
	if db == nil {
		return domain.UserTokenIssue{}, sql.ErrConnDone
	}
	acc, err := domain.ValidateAccountName(rawAccount)
	if err != nil {
		return domain.UserTokenIssue{}, err
	}
	role := domain.NormalizeRole(rawRole)
	plain, hash, hint, err := generateRandomToken()
	if err != nil {
		return domain.UserTokenIssue{}, err
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO users (account, role, token_hash, token_hint, active, created_at)
		VALUES (?, ?, ?, ?, 1, CURRENT_TIMESTAMP)
	`, acc, role, hash, hint); err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "unique") || strings.Contains(strings.ToLower(err.Error()), "constraint") {
			return domain.UserTokenIssue{}, fmt.Errorf("account %q already exists", acc)
		}
		return domain.UserTokenIssue{}, err
	}
	u, err := GetUser(ctx, db, acc)
	if err != nil {
		return domain.UserTokenIssue{}, err
	}
	return domain.UserTokenIssue{User: u, PlainToken: plain}, nil
}

// GetUser fetches one registered user account without exposing its token hash.
func GetUser(ctx context.Context, db *sql.DB, rawAccount string) (domain.UserAccount, error) {
	acc := domain.NormalizeAccount(rawAccount)
	var u domain.UserAccount
	var act int
	err := db.QueryRowContext(ctx, `
		SELECT account, role, COALESCE(token_hint, ''), active, COALESCE(created_at, ''), COALESCE(last_seen_at, '')
		FROM users WHERE account = ?
	`, acc).Scan(&u.Account, &u.Role, &u.TokenHint, &act, &u.CreatedAt, &u.LastSeenAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return domain.UserAccount{}, fmt.Errorf("account %q not found", acc)
		}
		return domain.UserAccount{}, err
	}
	u.Active = act == 1
	return u, nil
}

// ListUsers returns all team server user accounts sorted with admins first, then by account name.
func ListUsers(ctx context.Context, db *sql.DB) ([]domain.UserAccount, error) {
	if db == nil {
		return nil, sql.ErrConnDone
	}
	rows, err := db.QueryContext(ctx, `
		SELECT account, role, COALESCE(token_hint, ''), active, COALESCE(created_at, ''), COALESCE(last_seen_at, '')
		FROM users
		ORDER BY CASE WHEN role = 'admin' THEN 0 ELSE 1 END, account ASC
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []domain.UserAccount{}
	for rows.Next() {
		var u domain.UserAccount
		var act int
		if err := rows.Scan(&u.Account, &u.Role, &u.TokenHint, &act, &u.CreatedAt, &u.LastSeenAt); err != nil {
			return nil, err
		}
		u.Active = act == 1
		out = append(out, u)
	}
	return out, rows.Err()
}

// RotateUserToken replaces the user's token hash with a newly generated token and returns the plaintext once.
func RotateUserToken(ctx context.Context, db *sql.DB, rawAccount string) (domain.UserTokenIssue, error) {
	if db == nil {
		return domain.UserTokenIssue{}, sql.ErrConnDone
	}
	acc := domain.NormalizeAccount(rawAccount)
	plain, hash, hint, err := generateRandomToken()
	if err != nil {
		return domain.UserTokenIssue{}, err
	}
	res, err := db.ExecContext(ctx, `UPDATE users SET token_hash = ?, token_hint = ?, active = 1 WHERE account = ?`, hash, hint, acc)
	if err != nil {
		return domain.UserTokenIssue{}, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return domain.UserTokenIssue{}, fmt.Errorf("account %q not found", acc)
	}
	u, err := GetUser(ctx, db, acc)
	if err != nil {
		return domain.UserTokenIssue{}, err
	}
	return domain.UserTokenIssue{User: u, PlainToken: plain}, nil
}

// SetUserActive enables or disables a user account while preventing lockout of the last active admin.
func SetUserActive(ctx context.Context, db *sql.DB, rawAccount string, active bool) error {
	if db == nil {
		return sql.ErrConnDone
	}
	acc := domain.NormalizeAccount(rawAccount)
	return WithTx(ctx, db, func(tx *sql.Tx) error {
		var role string
		if err := tx.QueryRowContext(ctx, `SELECT role FROM users WHERE account = ?`, acc).Scan(&role); err != nil {
			return fmt.Errorf("account %q not found", acc)
		}
		if !active && role == domain.RoleAdmin {
			otherAdmins, err := activeAdminCountTx(ctx, tx, acc)
			if err != nil {
				return err
			}
			if otherAdmins == 0 {
				return fmt.Errorf("cannot disable the last active admin account (%s)", acc)
			}
		}
		act := 0
		if active {
			act = 1
		}
		_, err := tx.ExecContext(ctx, `UPDATE users SET active = ? WHERE account = ?`, act, acc)
		return err
	})
}

// UpdateUserRole promotes or demotes a user while preserving at least one active admin.
func UpdateUserRole(ctx context.Context, db *sql.DB, rawAccount, rawRole string) error {
	if db == nil {
		return sql.ErrConnDone
	}
	acc := domain.NormalizeAccount(rawAccount)
	newRole := domain.NormalizeRole(rawRole)
	return WithTx(ctx, db, func(tx *sql.Tx) error {
		var curRole string
		if err := tx.QueryRowContext(ctx, `SELECT role FROM users WHERE account = ?`, acc).Scan(&curRole); err != nil {
			return fmt.Errorf("account %q not found", acc)
		}
		if curRole == domain.RoleAdmin && newRole != domain.RoleAdmin {
			otherAdmins, err := activeAdminCountTx(ctx, tx, acc)
			if err != nil {
				return err
			}
			if otherAdmins == 0 {
				return fmt.Errorf("cannot demote the last active admin account (%s)", acc)
			}
		}
		_, err := tx.ExecContext(ctx, `UPDATE users SET role = ? WHERE account = ?`, newRole, acc)
		return err
	})
}

// DeleteUser removes a user account while preventing deletion of the last active admin.
func DeleteUser(ctx context.Context, db *sql.DB, rawAccount string) error {
	if db == nil {
		return sql.ErrConnDone
	}
	acc := domain.NormalizeAccount(rawAccount)
	return WithTx(ctx, db, func(tx *sql.Tx) error {
		var role string
		if err := tx.QueryRowContext(ctx, `SELECT role FROM users WHERE account = ?`, acc).Scan(&role); err != nil {
			return fmt.Errorf("account %q not found", acc)
		}
		if role == domain.RoleAdmin {
			otherAdmins, err := activeAdminCountTx(ctx, tx, acc)
			if err != nil {
				return err
			}
			if otherAdmins == 0 {
				return fmt.Errorf("cannot delete the last active admin account (%s)", acc)
			}
		}
		_, err := tx.ExecContext(ctx, `DELETE FROM users WHERE account = ?`, acc)
		return err
	})
}

// EnsureBootstrapAdmin creates an initial admin user if the users table is empty.
func EnsureBootstrapAdmin(ctx context.Context, db *sql.DB, rawAccount, explicitToken string) (domain.UserTokenIssue, bool, error) {
	if db == nil {
		return domain.UserTokenIssue{}, false, sql.ErrConnDone
	}
	var cnt int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM users`).Scan(&cnt); err != nil {
		return domain.UserTokenIssue{}, false, err
	}
	if cnt > 0 {
		return domain.UserTokenIssue{}, false, nil
	}
	acc, err := domain.ValidateAccountName(rawAccount)
	if err != nil {
		acc = "admin"
	}
	plain := strings.TrimSpace(explicitToken)
	var hash, hint string
	if plain != "" {
		hash = HashAuthToken(plain)
		hint = domain.FormatTokenHint(plain)
	} else {
		plain, hash, hint, err = generateRandomToken()
		if err != nil {
			return domain.UserTokenIssue{}, false, err
		}
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO users (account, role, token_hash, token_hint, active, created_at)
		VALUES (?, ?, ?, ?, 1, CURRENT_TIMESTAMP)
	`, acc, domain.RoleAdmin, hash, hint); err != nil {
		return domain.UserTokenIssue{}, false, err
	}
	_, _ = db.ExecContext(ctx, `UPDATE memories SET account = ? WHERE scope = 'personal' AND COALESCE(account, '') = ''`, acc)
	u, err := GetUser(ctx, db, acc)
	if err != nil {
		return domain.UserTokenIssue{}, false, err
	}
	return domain.UserTokenIssue{User: u, PlainToken: plain}, true, nil
}

// AuthenticateToken validates a bearer token against active users and updates last_seen_at.
func AuthenticateToken(ctx context.Context, db *sql.DB, plainToken string) (domain.AuthPrincipal, bool) {
	if db == nil {
		return domain.AuthPrincipal{}, false
	}
	trimmed := strings.TrimSpace(plainToken)
	if trimmed == "" {
		return domain.AuthPrincipal{}, false
	}
	digest := HashAuthToken(trimmed)
	var acc, role, hint, lastSeen string
	var act int
	err := db.QueryRowContext(ctx, `
		SELECT account, role, COALESCE(token_hint, ''), COALESCE(last_seen_at, ''), active
		FROM users WHERE token_hash = ?
	`, digest).Scan(&acc, &role, &hint, &lastSeen, &act)
	if err != nil || act != 1 {
		return domain.AuthPrincipal{}, false
	}
	if shouldTouchLastSeen(lastSeen, time.Now().UTC()) {
		_, _ = db.ExecContext(ctx, `UPDATE users SET last_seen_at = CURRENT_TIMESTAMP WHERE account = ?`, acc)
	}
	return domain.AuthPrincipal{
		Account:       acc,
		Role:          domain.NormalizeRole(role),
		TokenHint:     hint,
		ServerMode:    true,
		Authenticated: true,
	}, true
}

func shouldTouchLastSeen(lastSeen string, now time.Time) bool {
	s := strings.TrimSpace(lastSeen)
	if s == "" {
		return true
	}
	for _, layout := range []string{"2006-01-02 15:04:05", time.RFC3339, "2006-01-02T15:04:05Z"} {
		if ts, err := time.Parse(layout, s); err == nil {
			return now.Sub(ts.UTC()) >= 60*time.Second
		}
	}
	return true
}

func ensureSessionSecret(ctx context.Context, db *sql.DB) ([]byte, error) {
	if db == nil {
		return nil, sql.ErrConnDone
	}
	var val string
	if err := db.QueryRowContext(ctx, `SELECT val FROM config WHERE key = 'auth_session_secret'`).Scan(&val); err == nil && len(strings.TrimSpace(val)) >= 32 {
		if b, derr := hex.DecodeString(strings.TrimSpace(val)); derr == nil && len(b) >= 16 {
			return b, nil
		}
	}
	sec := make([]byte, 32)
	if _, err := rand.Read(sec); err != nil {
		return nil, err
	}
	hexSec := hex.EncodeToString(sec)
	if _, err := db.ExecContext(ctx, `
		INSERT INTO config (key, val) VALUES ('auth_session_secret', ?)
		ON CONFLICT(key) DO NOTHING
	`, hexSec); err != nil {
		return nil, err
	}
	if err := db.QueryRowContext(ctx, `SELECT val FROM config WHERE key = 'auth_session_secret'`).Scan(&val); err == nil {
		if b, derr := hex.DecodeString(strings.TrimSpace(val)); derr == nil {
			return b, nil
		}
	}
	return sec, nil
}

func signCookiePayload(secret []byte, body string) string {
	mac := hmac.New(sha256.New, secret)
	_, _ = mac.Write([]byte(body))
	return hex.EncodeToString(mac.Sum(nil))
}

// IssueSessionCookieValue builds an HMAC-signed cookie value bound to account + current token hint.
// Rotating or revoking the user's token immediately invalidates any previously issued browser cookies.
func IssueSessionCookieValue(ctx context.Context, db *sql.DB, p domain.AuthPrincipal, ttl time.Duration) (string, error) {
	secret, err := ensureSessionSecret(ctx, db)
	if err != nil {
		return "", err
	}
	if ttl <= 0 {
		ttl = 7 * 24 * time.Hour
	}
	exp := time.Now().Add(ttl).Unix()
	body := fmt.Sprintf("%s|%s|%d", p.Account, p.TokenHint, exp)
	return body + "|" + signCookiePayload(secret, body), nil
}

// AuthenticateSessionCookie validates cookieVal's HMAC signature, expiration, and live user token state.
func AuthenticateSessionCookie(ctx context.Context, db *sql.DB, cookieVal string) (domain.AuthPrincipal, bool) {
	if db == nil || strings.TrimSpace(cookieVal) == "" {
		return domain.AuthPrincipal{}, false
	}
	parts := strings.Split(strings.TrimSpace(cookieVal), "|")
	if len(parts) != 4 {
		return domain.AuthPrincipal{}, false
	}
	acc, hint, expStr, sigHex := parts[0], parts[1], parts[2], parts[3]
	expUnix, err := strconv.ParseInt(expStr, 10, 64)
	if err != nil || time.Now().Unix() > expUnix {
		return domain.AuthPrincipal{}, false
	}
	secret, err := ensureSessionSecret(ctx, db)
	if err != nil {
		return domain.AuthPrincipal{}, false
	}
	sigBytes, err := hex.DecodeString(sigHex)
	if err != nil {
		return domain.AuthPrincipal{}, false
	}
	expectedBytes, _ := hex.DecodeString(signCookiePayload(secret, acc+"|"+hint+"|"+expStr))
	if len(expectedBytes) == 0 || !hmac.Equal(sigBytes, expectedBytes) {
		return domain.AuthPrincipal{}, false
	}

	var dbRole, dbHint string
	var act int
	err = db.QueryRowContext(ctx, `
		SELECT role, COALESCE(token_hint, ''), active
		FROM users WHERE account = ?
	`, acc).Scan(&dbRole, &dbHint, &act)
	if err != nil || act != 1 || dbHint != hint {
		return domain.AuthPrincipal{}, false
	}
	return domain.AuthPrincipal{
		Account:       acc,
		Role:          domain.NormalizeRole(dbRole),
		TokenHint:     dbHint,
		ServerMode:    true,
		Authenticated: true,
	}, true
}
