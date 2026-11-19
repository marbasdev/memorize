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

func FirstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if trimmed := strings.TrimSpace(v); trimmed != "" {
			return trimmed
		}
	}
	return ""
}

func Clamp(v, lo, hi float64) float64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}
