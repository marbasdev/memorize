package domain

import "strings"

type ChangeEvent struct {
	Seq    uint64
	Domain string
}

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
