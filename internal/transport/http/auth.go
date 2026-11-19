package http

import (
	"database/sql"
	"net/http"
	"strings"
	"time"

	"memorize/internal/domain"
	"memorize/internal/infra/sqlite"
)

func ExtractBearerToken(r *http.Request) (string, bool) {
	if r == nil {
		return "", false
	}
	authz := strings.TrimSpace(r.Header.Get("Authorization"))
	if authz == "" {
		return "", false
	}
	parts := strings.SplitN(authz, " ", 2)
	if len(parts) != 2 || !strings.EqualFold(strings.TrimSpace(parts[0]), "Bearer") {
		return "", true
	}
	return strings.TrimSpace(parts[1]), true
}

func IsSecureRequest(r *http.Request) bool {
	if r == nil {
		return false
	}
	if r.TLS != nil {
		return true
	}
	if !IsTrustedProxyPeer(r.RemoteAddr) {
		return false
	}
	xfp := strings.TrimSpace(strings.Split(r.Header.Get("X-Forwarded-Proto"), ",")[0])
	return strings.EqualFold(xfp, "https")
}

func SetSessionCookie(w http.ResponseWriter, r *http.Request, value string, maxAgeSec int) {
	http.SetCookie(w, &http.Cookie{
		Name:     domain.SessionCookieName,
		Value:    value,
		Path:     "/",
		MaxAge:   maxAgeSec,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   IsSecureRequest(r),
	})
}

func ClearSessionCookie(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name:     domain.SessionCookieName,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		Expires:  time.Unix(0, 0),
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   IsSecureRequest(r),
	})
}

func SetAccountsCookie(w http.ResponseWriter, r *http.Request, value string, maxAgeSec int) {
	if strings.TrimSpace(value) == "" {
		ClearAccountsCookie(w, r)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     domain.AccountsCookieName,
		Value:    value,
		Path:     "/",
		MaxAge:   maxAgeSec,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   IsSecureRequest(r),
	})
}

func ClearAccountsCookie(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name:     domain.AccountsCookieName,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		Expires:  time.Unix(0, 0),
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   IsSecureRequest(r),
	})
}

func AuthenticateHTTPRequest(db *sql.DB, r *http.Request) (domain.AuthPrincipal, bool) {
	if db == nil || r == nil {
		return domain.AuthPrincipal{}, false
	}
	if tok, hasAuthz := ExtractBearerToken(r); hasAuthz {
		if tok == "" {
			return domain.AuthPrincipal{}, false
		}
		return sqlite.AuthenticateToken(r.Context(), db, tok)
	}
	c, err := r.Cookie(domain.SessionCookieName)
	if err != nil || strings.TrimSpace(c.Value) == "" {
		return domain.AuthPrincipal{}, false
	}
	return sqlite.AuthenticateSessionCookie(r.Context(), db, c.Value)
}
