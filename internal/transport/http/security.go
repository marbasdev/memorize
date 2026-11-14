package http

import (
	"encoding/json"
	"net"
	"net/http"
	"net/url"
	"strings"
)

// SecurityHeadersMiddleware attaches defensive HTTP security headers on local console responses.
func SecurityHeadersMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Content-Security-Policy",
			"default-src 'self'; script-src 'self' 'unsafe-eval'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'")
		next.ServeHTTP(w, r)
	})
}

// IsLoopbackHost reports whether rawHost resolves strictly to a loopback hostname or IP.
func IsLoopbackHost(rawHost string) bool {
	h := strings.TrimSpace(rawHost)
	if h == "" {
		return false
	}
	if hostOnly, _, err := net.SplitHostPort(h); err == nil {
		h = hostOnly
	}
	h = strings.Trim(strings.ToLower(h), "[]")
	return h == "127.0.0.1" || h == "localhost" || h == "::1"
}

// IsAllowedLocalOrigin blocks cross-site browser requests and non-loopback Host headers (DNS rebinding).
func IsAllowedLocalOrigin(r *http.Request) bool {
	if strings.EqualFold(strings.TrimSpace(r.Header.Get("Sec-Fetch-Site")), "cross-site") {
		return false
	}
	if !IsLoopbackHost(r.Host) {
		return false
	}
	orig := strings.TrimSpace(r.Header.Get("Origin"))
	if orig == "" {
		return true
	}
	u, err := url.Parse(orig)
	if err != nil {
		return false
	}
	host := strings.ToLower(u.Hostname())
	return host == "127.0.0.1" || host == "localhost" || host == "::1"
}

// IsLocalJSONPost verifies loopback Host/Origin, POST method, and application/json Content-Type.
func IsLocalJSONPost(r *http.Request) bool {
	if !IsAllowedLocalOrigin(r) || r.Method != http.MethodPost {
		return false
	}
	ct := strings.ToLower(strings.TrimSpace(r.Header.Get("Content-Type")))
	return strings.HasPrefix(ct, "application/json")
}

// PostJSON enforces POST + application/json + local Origin checks to prevent drive-by CSRF against localhost.
func PostJSON(fn http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !IsAllowedLocalOrigin(r) {
			WriteJSON(w, http.StatusForbidden, map[string]any{"error": "cross-origin request blocked"})
			return
		}
		if r.Method != http.MethodPost {
			WriteJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "POST required"})
			return
		}
		ct := strings.ToLower(strings.TrimSpace(r.Header.Get("Content-Type")))
		if !strings.HasPrefix(ct, "application/json") {
			WriteJSON(w, http.StatusUnsupportedMediaType, map[string]any{"error": "Content-Type application/json required"})
			return
		}
		fn(w, r)
	}
}

// WriteJSON writes a JSON response with the given HTTP status code.
func WriteJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// WriteErr writes an error JSON response with the given HTTP status code.
func WriteErr(w http.ResponseWriter, code int, err error) {
	WriteJSON(w, code, map[string]any{"error": err.Error()})
}

// DecodeJSON reads up to 1 MiB from the request body and decodes JSON into dst.
func DecodeJSON(w http.ResponseWriter, r *http.Request, dst any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	if err := json.NewDecoder(r.Body).Decode(dst); err != nil {
		WriteErr(w, http.StatusBadRequest, err)
		return false
	}
	return true
}
