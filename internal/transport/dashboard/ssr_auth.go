package dashboard

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"memorize/internal/domain"
	"memorize/internal/infra/config"
	"memorize/internal/infra/git"
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
	IssuedMCPJSON  string
	ConnectHintURL string
}

type ssrRemoteRepoRow struct {
	RepoPath  string
	URL       string
	Account   string
	Role      string
	TokenHint string
}

type ssrRemotesModalData struct {
	Lang            string
	RemoteConnected bool
	RemoteCount     int
	RemoteOOB       bool
	HasDefault      bool
	DefaultURL      string
	DefaultAccount  string
	DefaultRole     string
	DefaultHint     string
	Repos           []ssrRemoteRepoRow
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
	host := thttp.TrustedForwardedHost(r)
	if host == "" {
		host = strings.TrimSpace(r.Host)
	}
	if host == "" {
		host = domain.DefaultDaemonAddr
	}
	return scheme + "://" + host
}

const (
	loginRateWindow      = 5 * time.Minute
	loginMaxFailures     = 10
	loginMaxTrackedPeers = 512
)

type loginAttemptBucket struct {
	failures    int
	windowStart time.Time
	lastSeen    time.Time
}

type loginRateLimiter struct {
	mu      sync.Mutex
	buckets map[string]*loginAttemptBucket
}

func (l *loginRateLimiter) isBlocked(ip string, now time.Time) bool {
	if l == nil || ip == "" {
		return false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	b, ok := l.buckets[ip]
	if !ok {
		return false
	}
	if now.Sub(b.windowStart) > loginRateWindow {
		delete(l.buckets, ip)
		return false
	}
	b.lastSeen = now
	return b.failures >= loginMaxFailures
}

func (l *loginRateLimiter) recordFailure(ip string, now time.Time) bool {
	if l == nil || ip == "" {
		return false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	b, ok := l.buckets[ip]
	if !ok || now.Sub(b.windowStart) > loginRateWindow {
		b = &loginAttemptBucket{windowStart: now}
		l.buckets[ip] = b
	}
	b.failures++
	b.lastSeen = now
	if len(l.buckets) > loginMaxTrackedPeers {
		for k, entry := range l.buckets {
			if k != ip && now.Sub(entry.windowStart) > loginRateWindow {
				delete(l.buckets, k)
			}
		}
		for len(l.buckets) > loginMaxTrackedPeers {
			oldestKey := ""
			var oldestTime time.Time
			for k, entry := range l.buckets {
				if k == ip {
					continue
				}
				if oldestKey == "" || entry.lastSeen.Before(oldestTime) {
					oldestKey = k
					oldestTime = entry.lastSeen
				}
			}
			if oldestKey == "" {
				break
			}
			delete(l.buckets, oldestKey)
		}
	}
	return b.failures >= loginMaxFailures
}

func (l *loginRateLimiter) reset(ip string) {
	if l == nil || ip == "" {
		return
	}
	l.mu.Lock()
	delete(l.buckets, ip)
	l.mu.Unlock()
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
		mcpCfg := map[string]any{
			"mcpServers": map[string]any{
				"memorize": map[string]any{
					"url": strings.TrimRight(data.ConnectHintURL, "/") + "/mcp",
					"headers": map[string]string{
						"Authorization": "Bearer " + issued.PlainToken,
					},
				},
			},
		}
		if b, merr := json.MarshalIndent(mcpCfg, "", "  "); merr == nil {
			data.IssuedMCPJSON = string(b)
		}
	}
	return data, nil
}

func safeRemoteHTTPURL(raw string) string {
	norm := config.NormalizeRemoteURL(raw)
	u, err := url.Parse(norm)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return ""
	}
	return strings.TrimRight(u.String(), "/")
}

func remoteBindingsDataDir(d Deps) string {
	if envDir := strings.TrimSpace(os.Getenv("MEMORY_DATA_DIR")); envDir != "" {
		return envDir
	}
	if d.Memory != nil {
		dbPath := d.Memory.DBPath()
		if dbPath != "" && dbPath != ":memory:" && !strings.HasPrefix(dbPath, "file::memory:") {
			return config.DataDirForDBPath(dbPath)
		}
	}
	return config.DefaultDataDir()
}

func buildRemotesModalData(d Deps, lang string) ssrRemotesModalData {
	if d.ServerMode {
		return ssrRemotesModalData{Lang: lang}
	}
	dataDir := remoteBindingsDataDir(d)
	f := config.LoadRemoteBindings(dataDir)
	out := ssrRemotesModalData{Lang: lang}
	if f.Default != nil {
		if safeURL := safeRemoteHTTPURL(f.Default.URL); safeURL != "" {
			out.HasDefault = true
			out.DefaultURL = safeURL
			out.DefaultAccount = f.Default.Account
			out.DefaultRole = f.Default.Role
			out.DefaultHint = domain.FormatTokenHint(f.Default.Token)
			out.RemoteCount++
		}
	}
	var paths []string
	for rp := range f.Repos {
		paths = append(paths, rp)
	}
	sort.Strings(paths)
	for _, rp := range paths {
		t := f.Repos[rp]
		safeURL := safeRemoteHTTPURL(t.URL)
		if safeURL == "" {
			continue
		}
		out.Repos = append(out.Repos, ssrRemoteRepoRow{
			RepoPath:  rp,
			URL:       safeURL,
			Account:   t.Account,
			Role:      t.Role,
			TokenHint: domain.FormatTokenHint(t.Token),
		})
		out.RemoteCount++
	}
	out.RemoteConnected = out.RemoteCount > 0
	return out
}

func renderRemotesModalWithToast(w http.ResponseWriter, d Deps, lang, toast string) {
	data := buildRemotesModalData(d, lang)
	body, err := renderSSRTemplate("modal_remotes", data)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	oobData := data
	oobData.RemoteOOB = true
	if navBytes, nErr := renderSSRTemplate("remotes_nav_btn", oobData); nErr == nil {
		body = append(body, '\n')
		body = append(body, navBytes...)
	}
	if strings.TrimSpace(toast) != "" {
		setHXTriggers(w, toast, false, false)
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store, no-cache, must-revalidate")
	_, _ = w.Write(body)
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
	limiter := &loginRateLimiter{buckets: make(map[string]*loginAttemptBucket)}
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
				Name:     domain.LangCookieName,
				Value:    lang,
				Path:     "/",
				MaxAge:   int(domain.LangCookieMaxAge.Seconds()),
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
		now := time.Now()
		peerIP := thttp.ClientPeerIP(r)
		if limiter.isBlocked(peerIP, now) {
			body, err := renderSSRTemplate("login", ssrLoginData{
				Lang:  lang,
				Error: langText(lang, "Too many failed login attempts. Please wait a few minutes and try again.", "로그인 실패 횟수가 너무 많습니다. 잠시 후 다시 시도해 주세요."),
			})
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.Header().Set("Cache-Control", "no-store, no-cache, must-revalidate")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write(body)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, 1<<16)
		_ = r.ParseForm()
		tok := strings.TrimSpace(r.FormValue("token"))
		p, ok := sqlite.AuthenticateToken(r.Context(), db, tok)
		if !ok {
			throttled := limiter.recordFailure(peerIP, now)
			status := http.StatusUnauthorized
			errMsg := langText(lang, "Invalid or disabled access token.", "액세스 토큰이 유효하지 않거나 비활성화되었습니다.")
			if throttled {
				status = http.StatusTooManyRequests
				errMsg = langText(lang, "Too many failed login attempts. Please wait a few minutes and try again.", "로그인 실패 횟수가 너무 많습니다. 잠시 후 다시 시도해 주세요.")
			}
			body, err := renderSSRTemplate("login", ssrLoginData{
				Lang:  lang,
				Error: errMsg,
			})
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.Header().Set("Cache-Control", "no-store, no-cache, must-revalidate")
			w.WriteHeader(status)
			_, _ = w.Write(body)
			return
		}
		limiter.reset(peerIP)
		cookieVal, err := sqlite.IssueSessionCookieValue(r.Context(), db, p, domain.DefaultSessionTTL)
		if err != nil {
			http.Error(w, "failed to create session", http.StatusInternalServerError)
			return
		}
		thttp.SetSessionCookie(w, r, cookieVal, int(domain.DefaultSessionTTL.Seconds()))
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

	mux.HandleFunc("/ui/modal/remotes", getHX(requireAdminRoute(func(w http.ResponseWriter, r *http.Request) {
		if d.ServerMode {
			http.Error(w, "remote client connections are configured on client machines", http.StatusNotFound)
			return
		}
		renderRemotesModalWithToast(w, d, requestLang(r), "")
	})))

	mux.HandleFunc("/ui/remotes/connect", postHX(requireAdminRoute(func(w http.ResponseWriter, r *http.Request) {
		if d.ServerMode {
			http.Error(w, "remote client connections are configured on client machines", http.StatusNotFound)
			return
		}
		lang := requestLang(r)
		mode := strings.ToLower(strings.TrimSpace(r.FormValue("mode")))
		isGlobal := mode == "global"
		repoPath := strings.TrimSpace(r.FormValue("repo_path"))
		if !isGlobal && repoPath == "" {
			http.Error(w, langText(lang, "enter a repository directory path or select Global Default", "저장소 디렉토리 경로를 입력하거나 전역 기본값을 선택하세요"), http.StatusBadRequest)
			return
		}
		safeURL := safeRemoteHTTPURL(r.FormValue("url"))
		tok := strings.TrimSpace(r.FormValue("token"))
		if safeURL == "" || tok == "" {
			http.Error(w, langText(lang, "valid http/https server URL and token are required", "유효한 http/https 서버 URL과 토큰을 입력하세요"), http.StatusBadRequest)
			return
		}
		st, err := thttp.QueryDaemonStatusWithAuth(safeURL, tok)
		if err != nil {
			http.Error(w, langText(lang, fmt.Sprintf("server verification failed: %v", err), fmt.Sprintf("팀 서버 검증 실패: %v", err)), http.StatusBadRequest)
			return
		}
		repoRoot := ""
		if !isGlobal {
			if _, root := git.CanonicalScope(r.Context(), repoPath); root != "" {
				repoRoot = root
			} else if abs, aerr := filepath.Abs(repoPath); aerr == nil {
				repoRoot = abs
			} else {
				repoRoot = filepath.Clean(repoPath)
			}
		}
		target := domain.RemoteTargetConfig{
			URL:     safeURL,
			Token:   tok,
			Account: st.Account,
			Role:    st.Role,
		}
		if err := config.SaveRemoteBinding(remoteBindingsDataDir(d), repoRoot, isGlobal, target); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		msg := langText(lang, fmt.Sprintf("Connected to %s", safeURL), fmt.Sprintf("팀 서버 연결 완료: %s", safeURL))
		renderRemotesModalWithToast(w, d, lang, msg)
	})))

	mux.HandleFunc("/ui/remotes/disconnect", postHX(requireAdminRoute(func(w http.ResponseWriter, r *http.Request) {
		if d.ServerMode {
			http.Error(w, "remote client connections are configured on client machines", http.StatusNotFound)
			return
		}
		lang := requestLang(r)
		isGlobal := strings.EqualFold(strings.TrimSpace(r.FormValue("mode")), "global")
		repoPath := strings.TrimSpace(r.FormValue("repo_path"))
		removed, err := config.RemoveRemoteBinding(remoteBindingsDataDir(d), repoPath, isGlobal)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		toast := langText(lang, "No matching connection removed", "제거할 연결 정보가 없습니다")
		if removed {
			toast = langText(lang, "Disconnected from remote team server", "원격 팀 서버 연결을 해제했습니다")
		}
		renderRemotesModalWithToast(w, d, lang, toast)
	})))
}
