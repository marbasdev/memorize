package http

import (
	"encoding/json"
	"net"
	"net/http"
	"net/url"
	"strings"
)

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

// IsTrustedProxyPeer reports whether remoteAddr belongs to a loopback, private RFC1918/ULA,
// or link-local reverse proxy allowed to supply X-Forwarded-* headers.
func IsTrustedProxyPeer(remoteAddr string) bool {
	raw := strings.TrimSpace(remoteAddr)
	if raw == "" {
		return false
	}
	host := raw
	if h, _, err := net.SplitHostPort(raw); err == nil {
		host = h
	}
	host = strings.Trim(host, "[]")
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast()
}

func TrustedForwardedHost(r *http.Request) string {
	if r == nil || !IsTrustedProxyPeer(r.RemoteAddr) {
		return ""
	}
	raw := strings.TrimSpace(r.Header.Get("X-Forwarded-Host"))
	if raw == "" {
		return ""
	}
	first := strings.TrimSpace(strings.Split(raw, ",")[0])
	if first == "" {
		return ""
	}
	for i := 0; i < len(first); i++ {
		c := first[i]
		if c <= 0x20 || c >= 0x7f || c == '/' || c == '\\' || c == '@' || c == '?' || c == '#' {
			return ""
		}
	}
	return first
}

func ClientPeerIP(r *http.Request) string {
	if r == nil {
		return ""
	}
	if IsTrustedProxyPeer(r.RemoteAddr) {
		if xff := strings.TrimSpace(r.Header.Get("X-Forwarded-For")); xff != "" {
			first := strings.TrimSpace(strings.Split(xff, ",")[0])
			if ip := net.ParseIP(strings.Trim(first, "[]")); ip != nil {
				return ip.String()
			}
		}
	}
	raw := strings.TrimSpace(r.RemoteAddr)
	if h, _, err := net.SplitHostPort(raw); err == nil {
		raw = h
	}
	if ip := net.ParseIP(strings.Trim(raw, "[]")); ip != nil {
		return ip.String()
	}
	return raw
}

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

func IsAllowedOrigin(r *http.Request, serverMode bool) bool {
	if r == nil {
		return false
	}
	if strings.EqualFold(strings.TrimSpace(r.Header.Get("Sec-Fetch-Site")), "cross-site") {
		return false
	}
	if !serverMode {
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
	hostHeader := strings.TrimSpace(r.Host)
	if hostHeader == "" {
		return false
	}
	orig := strings.TrimSpace(r.Header.Get("Origin"))
	if orig == "" {
		return true
	}
	u, err := url.Parse(orig)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return false
	}
	if strings.EqualFold(u.Host, hostHeader) {
		return true
	}
	if xfh := TrustedForwardedHost(r); xfh != "" && strings.EqualFold(u.Host, xfh) {
		return true
	}
	return false
}

func IsJSONPost(r *http.Request, serverMode bool) bool {
	if r == nil || r.Method != http.MethodPost || !IsAllowedOrigin(r, serverMode) {
		return false
	}
	ct := strings.ToLower(strings.TrimSpace(r.Header.Get("Content-Type")))
	return strings.HasPrefix(ct, "application/json")
}

func PostJSONMode(serverMode bool, fn http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if !IsAllowedOrigin(r, serverMode) {
			http.Error(w, "forbidden origin", http.StatusForbidden)
			return
		}
		if !IsJSONPost(r, serverMode) {
			http.Error(w, "unsupported media type", http.StatusUnsupportedMediaType)
			return
		}
		fn(w, r)
	}
}

func WriteJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func WriteErr(w http.ResponseWriter, code int, err error) {
	WriteJSON(w, code, map[string]any{"error": err.Error()})
}

func DecodeJSON(w http.ResponseWriter, r *http.Request, dst any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	if err := json.NewDecoder(r.Body).Decode(dst); err != nil {
		WriteErr(w, http.StatusBadRequest, err)
		return false
	}
	return true
}
