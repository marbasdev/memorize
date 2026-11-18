package dashboard

import (
	"fmt"
	"net/http"
	"strings"
	"time"

	"memorize/internal/domain"
	"memorize/internal/infra/sqlite"
	thttp "memorize/internal/transport/http"
)

type ssrLoginData struct {
	Lang       string
	Error      string
	HasReferer bool
}

type ssrUsersModalData struct {
	Lang           string
	CurrentAccount string
	Users          []domain.UserAccount
	IssuedAccount  string
	IssuedRole     string
	IssuedToken    string
	ConnectHintURL string
}

func dashboardAuthMiddleware(d Deps, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !thttp.IsAllowedOrigin(r, d.ServerMode) {
			thttp.WriteJSON(w, http.StatusForbidden, map[string]any{"error": "cross-origin request blocked"})
			return
		}
		if !d.ServerMode {
			next.ServeHTTP(w, r)
			return
		}

		// Public static assets & login page remain accessible without session cookie.
		if (r.URL.Path == "/login" && (r.Method == http.MethodGet || r.Method == http.MethodPost)) ||
			(strings.HasPrefix(r.URL.Path, "/assets/") && r.Method == http.MethodGet) {
			next.ServeHTTP(w, r)
			return
		}

		db := d.DB
		if db == nil && d.Memory != nil {
			db = d.Memory.DB()
		}
		p, ok := thttp.AuthenticateHTTPRequest(db, r)
		if !ok {
			if r.Header.Get("HX-Request") == "true" {
				w.Header().Set("HX-Redirect", "/login")
				thttp.WriteJSON(w, http.StatusUnauthorized, map[string]any{"error": "authentication required"})
				return
			}
			if strings.HasPrefix(r.URL.Path, "/api/") {
				thttp.WriteJSON(w, http.StatusUnauthorized, map[string]any{"error": "authentication required"})
				return
			}
			if r.Method == http.MethodGet && (r.URL.Path == "/" || !strings.HasPrefix(r.URL.Path, "/ui/")) {
				http.Redirect(w, r, "/login", http.StatusFound)
				return
			}
			thttp.WriteJSON(w, http.StatusUnauthorized, map[string]any{"error": "authentication required"})
			return
		}

		ctx := domain.WithAuthPrincipal(r.Context(), p)
		if p.Account != "" {
			ctx = domain.WithAccount(ctx, p.Account)
		}
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func requireAdminRoute(fn http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := domain.RequireAdmin(r.Context()); err != nil {
			thttp.WriteJSON(w, http.StatusForbidden, map[string]any{"error": err.Error()})
			return
		}
		fn(w, r)
	}
}

func resolveRequestBaseURL(r *http.Request) string {
	scheme := "http"
	if thttp.IsSecureRequest(r) {
		scheme = "https"
	}
	host := strings.TrimSpace(r.Header.Get("X-Forwarded-Host"))
	if host != "" {
		host = strings.TrimSpace(strings.Split(host, ",")[0])
	}
	if host == "" {
		host = strings.TrimSpace(r.Host)
	}
	if host == "" {
		host = "127.0.0.1:18095"
	}
	return scheme + "://" + host
}

func buildUsersModalData(r *http.Request, d Deps, lang string, issued *domain.UserTokenIssue) (ssrUsersModalData, error) {
	db := d.DB
	if db == nil && d.Memory != nil {
		db = d.Memory.DB()
	}
	users, err := sqlite.ListUsers(r.Context(), db)
	if err != nil {
		return ssrUsersModalData{}, err
	}
	data := ssrUsersModalData{
		Lang:           lang,
		CurrentAccount: domain.EffectivePrincipalAccount(r.Context()),
		Users:          users,
		ConnectHintURL: resolveRequestBaseURL(r),
	}
	if issued != nil && issued.PlainToken != "" {
		data.IssuedAccount = issued.User.Account
		data.IssuedRole = issued.User.Role
		data.IssuedToken = issued.PlainToken
	}
	return data, nil
}

func renderUsersModalWithToast(w http.ResponseWriter, r *http.Request, d Deps, lang, toast string, issued *domain.UserTokenIssue) {
	data, err := buildUsersModalData(r, d, lang, issued)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if strings.TrimSpace(toast) != "" {
		setHXTriggers(w, toast, false, false)
	}
	writeHTMLTemplate(w, "modal_users", data)
}

func registerSSRAuthRoutes(mux *http.ServeMux, d Deps) {
	mux.HandleFunc("/login", func(w http.ResponseWriter, r *http.Request) {
		if !thttp.IsAllowedOrigin(r, d.ServerMode) {
			thttp.WriteJSON(w, http.StatusForbidden, map[string]any{"error": "cross-origin request blocked"})
			return
		}
		if !d.ServerMode {
			http.Redirect(w, r, "/", http.StatusFound)
			return
		}
		db := d.DB
		if db == nil && d.Memory != nil {
			db = d.Memory.DB()
		}
		lang := requestLang(r)
		if qLang := strings.TrimSpace(r.URL.Query().Get("lang")); qLang != "" {
			lang = normalizeLang(qLang)
			http.SetCookie(w, &http.Cookie{
				Name:     "memorize_lang",
				Value:    lang,
				Path:     "/",
				MaxAge:   365 * 24 * 3600,
				HttpOnly: true,
				SameSite: http.SameSiteLaxMode,
				Secure:   thttp.IsSecureRequest(r),
			})
		}

		if r.Method == http.MethodGet {
			if _, ok := thttp.AuthenticateHTTPRequest(db, r); ok {
				http.Redirect(w, r, "/", http.StatusFound)
				return
			}
			writeHTMLTemplate(w, "login", ssrLoginData{Lang: lang})
			return
		}
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, 1<<16)
		_ = r.ParseForm()
		tok := strings.TrimSpace(r.FormValue("token"))
		p, ok := sqlite.AuthenticateToken(r.Context(), db, tok)
		if !ok {
			body, err := renderSSRTemplate("login", ssrLoginData{
				Lang:  lang,
				Error: langText(lang, "Invalid or disabled access token.", "액세스 토큰이 유효하지 않거나 비활성화되었습니다."),
			})
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.Header().Set("Cache-Control", "no-store, no-cache, must-revalidate")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write(body)
			return
		}
		cookieVal, err := sqlite.IssueSessionCookieValue(r.Context(), db, p, 7*24*time.Hour)
		if err != nil {
			http.Error(w, "failed to create session", http.StatusInternalServerError)
			return
		}
		thttp.SetSessionCookie(w, r, cookieVal, 7*24*3600)
		http.Redirect(w, r, "/", http.StatusSeeOther)
	})

	mux.HandleFunc("/logout", func(w http.ResponseWriter, r *http.Request) {
		if !thttp.IsAllowedOrigin(r, d.ServerMode) {
			thttp.WriteJSON(w, http.StatusForbidden, map[string]any{"error": "cross-origin request blocked"})
			return
		}
		if r.Method != http.MethodPost {
			http.Error(w, "POST required", http.StatusMethodNotAllowed)
			return
		}
		thttp.ClearSessionCookie(w, r)
		if r.Header.Get("HX-Request") == "true" {
			w.Header().Set("HX-Redirect", "/login")
			w.WriteHeader(http.StatusOK)
			return
		}
		http.Redirect(w, r, "/login", http.StatusSeeOther)
	})

	mux.HandleFunc("/ui/modal/users", getHX(requireAdminRoute(func(w http.ResponseWriter, r *http.Request) {
		if !d.ServerMode {
			http.Error(w, "user management is available in server mode only", http.StatusNotFound)
			return
		}
		renderUsersModalWithToast(w, r, d, requestLang(r), "", nil)
	})))

	mux.HandleFunc("/ui/users/create", postHX(requireAdminRoute(func(w http.ResponseWriter, r *http.Request) {
		lang := requestLang(r)
		db := d.DB
		if db == nil && d.Memory != nil {
			db = d.Memory.DB()
		}
		acc := strings.TrimSpace(r.FormValue("account"))
		role := strings.TrimSpace(r.FormValue("role"))
		issued, err := sqlite.CreateUser(r.Context(), db, acc, role)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		msg := langText(lang, fmt.Sprintf("Created account @%s", issued.User.Account), fmt.Sprintf("@%s 계정을 생성했습니다", issued.User.Account))
		renderUsersModalWithToast(w, r, d, lang, msg, &issued)
	})))

	mux.HandleFunc("/ui/users/rotate", postHX(requireAdminRoute(func(w http.ResponseWriter, r *http.Request) {
		lang := requestLang(r)
		db := d.DB
		if db == nil && d.Memory != nil {
			db = d.Memory.DB()
		}
		acc := strings.TrimSpace(r.FormValue("account"))
		issued, err := sqlite.RotateUserToken(r.Context(), db, acc)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		msg := langText(lang, fmt.Sprintf("Rotated token for @%s", issued.User.Account), fmt.Sprintf("@%s 토큰을 재발급했습니다", issued.User.Account))
		renderUsersModalWithToast(w, r, d, lang, msg, &issued)
	})))

	mux.HandleFunc("/ui/users/toggle", postHX(requireAdminRoute(func(w http.ResponseWriter, r *http.Request) {
		lang := requestLang(r)
		db := d.DB
		if db == nil && d.Memory != nil {
			db = d.Memory.DB()
		}
		acc := strings.TrimSpace(r.FormValue("account"))
		active := strings.TrimSpace(r.FormValue("active")) == "1"
		if err := sqlite.SetUserActive(r.Context(), db, acc, active); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		msg := langText(lang, fmt.Sprintf("Updated status for @%s", domain.NormalizeAccount(acc)), fmt.Sprintf("@%s 상태를 변경했습니다", domain.NormalizeAccount(acc)))
		renderUsersModalWithToast(w, r, d, lang, msg, nil)
	})))

	mux.HandleFunc("/ui/users/role", postHX(requireAdminRoute(func(w http.ResponseWriter, r *http.Request) {
		lang := requestLang(r)
		db := d.DB
		if db == nil && d.Memory != nil {
			db = d.Memory.DB()
		}
		acc := strings.TrimSpace(r.FormValue("account"))
		role := strings.TrimSpace(r.FormValue("role"))
		if err := sqlite.UpdateUserRole(r.Context(), db, acc, role); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		msg := langText(lang, fmt.Sprintf("Updated role for @%s", domain.NormalizeAccount(acc)), fmt.Sprintf("@%s 역할을 변경했습니다", domain.NormalizeAccount(acc)))
		renderUsersModalWithToast(w, r, d, lang, msg, nil)
	})))

	mux.HandleFunc("/ui/users/delete", postHX(requireAdminRoute(func(w http.ResponseWriter, r *http.Request) {
		lang := requestLang(r)
		db := d.DB
		if db == nil && d.Memory != nil {
			db = d.Memory.DB()
		}
		acc := strings.TrimSpace(r.FormValue("account"))
		if err := sqlite.DeleteUser(r.Context(), db, acc); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		msg := langText(lang, fmt.Sprintf("Removed account @%s", domain.NormalizeAccount(acc)), fmt.Sprintf("@%s 계정을 삭제했습니다", domain.NormalizeAccount(acc)))
		renderUsersModalWithToast(w, r, d, lang, msg, nil)
	})))
}
