package domain

import "strings"

// ChangeEvent describes a state mutation with its domain ("progress", "memories", "guardrails", "live", or "all").
type ChangeEvent struct {
	Seq    uint64
	Domain string
}

// MergeEventDomains combines coalesced SSE domains into either a shared domain or "all".
func MergeEventDomains(a, b string) string {
	da := strings.TrimSpace(a)
	db := strings.TrimSpace(b)
	if da == "" || da == "live" {
		return db
	}
	if db == "" || db == "live" || da == db {
		return da
	}
	return "all"
}
