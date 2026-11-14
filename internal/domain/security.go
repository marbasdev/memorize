package domain

import (
	"regexp"
	"strings"
)

var secretPatterns = []*regexp.Regexp{
	regexp.MustCompile(`AIza[0-9A-Za-z_-]{20,}`),
	regexp.MustCompile(`sk-[A-Za-z0-9_-]{20,}`),
	regexp.MustCompile(`gh[pousr]_[A-Za-z0-9]{20,}`),
	regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----`),
}

var injectionPhrases = []string{
	"ignore previous instructions",
	"ignore all prior instructions",
	"disregard all previous rules",
	"system prompt override",
	"이전 지시를 모두 무시",
}

// CheckSecurityGuard rejects strings containing raw API/private keys or prompt-injection signatures.
func CheckSecurityGuard(text string) string {
	for _, re := range secretPatterns {
		if re.MatchString(text) {
			return "raw API key or private key detected"
		}
	}
	lower := strings.ToLower(text)
	for _, p := range injectionPhrases {
		if strings.Contains(lower, p) {
			return "prompt injection pattern detected"
		}
	}
	return ""
}

// RedactSecrets masks embedded API keys and private key headers in s.
func RedactSecrets(raw string) string {
	s := raw
	for _, re := range secretPatterns {
		s = re.ReplaceAllString(s, "[REDACTED]")
	}
	return s
}
