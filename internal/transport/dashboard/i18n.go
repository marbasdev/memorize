package dashboard

import (
	"net/http"
	"strings"

	"praxis/internal/domain"
)

func normalizeLang(raw string) string {
	if strings.EqualFold(strings.TrimSpace(raw), "ko") {
		return "ko"
	}
	return "en"
}

func requestLang(r *http.Request) string {
	if q := strings.TrimSpace(r.URL.Query().Get("lang")); q != "" {
		return normalizeLang(q)
	}
	if c, err := r.Cookie(domain.LangCookieName); err == nil && c != nil {
		return normalizeLang(c.Value)
	}
	return "en"
}

var uiTranslations = map[string]map[string]string{
	"en": enTranslations,
	"ko": koTranslations,
}

func uiText(lang, key string) string {
	l := normalizeLang(lang)
	if m := uiTranslations[l]; m != nil {
		if v, ok := m[key]; ok {
			return v
		}
	}
	if v, ok := uiTranslations["en"][key]; ok {
		return v
	}
	return key
}
