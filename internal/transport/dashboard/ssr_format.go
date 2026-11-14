package dashboard

import (
	"bytes"
	"fmt"
	"html"
	"html/template"
	"regexp"
	"sort"
	"strings"
	"time"

	"memorize/internal/domain"
	"memorize/internal/modules/progress"
)

func fmtProgressStamp(lang, rawTS string) string {
	t, ok := progress.ParseProgressTimestamp(rawTS)
	if !ok {
		s := strings.TrimSpace(rawTS)
		if len(s) > 16 {
			return s[:16]
		}
		return s
	}
	now := time.Now()
	diff := now.Sub(t)
	if diff < -time.Minute {
		return t.Format("01-02 15:04")
	}
	sameDay := t.Year() == now.Year() && t.YearDay() == now.YearDay()
	yest := now.AddDate(0, 0, -1)
	isYesterday := t.Year() == yest.Year() && t.YearDay() == yest.YearDay()

	if diff < time.Minute {
		if lang == "ko" {
			return "방금 전 · " + t.Format("15:04")
		}
		return "just now · " + t.Format("15:04")
	}
	if diff < time.Hour {
		mins := int(diff.Minutes())
		if mins < 1 {
			mins = 1
		}
		if lang == "ko" {
			return fmt.Sprintf("%d분 전 · %s", mins, t.Format("15:04"))
		}
		return fmt.Sprintf("%dm ago · %s", mins, t.Format("15:04"))
	}
	if sameDay {
		hrs := int(diff.Hours())
		if hrs < 1 {
			hrs = 1
		}
		if lang == "ko" {
			return fmt.Sprintf("%d시간 전 · %s", hrs, t.Format("15:04"))
		}
		return fmt.Sprintf("%dh ago · %s", hrs, t.Format("15:04"))
	}
	if isYesterday {
		if lang == "ko" {
			return "어제 · " + t.Format("15:04")
		}
		return "yesterday · " + t.Format("15:04")
	}
	if diff < 7*24*time.Hour {
		days := int(diff.Hours() / 24)
		if days < 1 {
			days = 1
		}
		if lang == "ko" {
			return fmt.Sprintf("%d일 전 · %s", days, t.Format("01-02 15:04"))
		}
		return fmt.Sprintf("%dd ago · %s", days, t.Format("01-02 15:04"))
	}
	if t.Year() == now.Year() {
		return t.Format("01-02 15:04")
	}
	return t.Format("2006-01-02 15:04")
}

func fmtDoneItemStamp(lang string, r domain.ProgressRecord) string {
	ts := strings.TrimSpace(r.UpdatedAt)
	if ts == "" {
		ts = strings.TrimSpace(r.CreatedAt)
	}
	stamp := fmtProgressStamp(lang, ts)
	if r.Source == "git_sync" && strings.TrimSpace(r.Date) != "" {
		if t, ok := progress.ParseProgressTimestamp(ts); ok && r.Date != t.Format("2006-01-02") {
			commitLabel := "commit"
			if lang == "ko" {
				commitLabel = "커밋"
			}
			return fmt.Sprintf("%s (%s %s)", stamp, commitLabel, r.Date)
		}
	}
	return stamp
}

func fmtDoneWindowDesc(lang string, ov domain.ProgressOverview) string {
	win := domain.NormalizeDoneWindow(ov.DoneWindow)
	var base string
	switch win {
	case "24h":
		base = uiText(lang, "prog_win_24h")
	case "30d":
		base = uiText(lang, "prog_win_30d")
	case "all":
		base = uiText(lang, "prog_win_all")
	default:
		base = uiText(lang, "prog_win_7d")
	}
	rangeText := ""
	if win == "24h" {
		if len(ov.DoneWindowStart) >= 16 && len(ov.DoneWindowEnd) >= 16 {
			rangeText = ov.DoneWindowStart[5:16] + " ~ " + ov.DoneWindowEnd[5:16]
		}
	} else if win == "7d" || win == "30d" {
		if len(ov.DoneWindowStart) >= 10 && len(ov.DoneWindowEnd) >= 10 {
			rangeText = ov.DoneWindowStart[5:10] + " ~ " + ov.DoneWindowEnd[5:10]
		}
	}
	if rangeText != "" {
		base += " (" + rangeText + ")"
	}
	shown := len(ov.RecentDone)
	total := ov.DoneWindowTotal
	if total <= 0 {
		if lang == "ko" {
			return base + " · 완료 기록 없음"
		}
		return base + " · 0 completed"
	}
	if total > shown {
		if lang == "ko" {
			return fmt.Sprintf("%s · %d건 중 최신 %d건 (완료 시각순)", base, total, shown)
		}
		return fmt.Sprintf("%s · latest %d of %d (by completion time)", base, shown, total)
	}
	if lang == "ko" {
		return fmt.Sprintf("%s · 총 %d건 (완료 시각순)", base, total)
	}
	return fmt.Sprintf("%s · %d completed (by completion time)", base, total)
}

func renderSSRTemplate(name string, data any) ([]byte, error) {
	tmpl := template.New("root").Funcs(ssrFuncMap())
	for _, rel := range ssrTemplateFiles {
		raw, ok := resolveLiveAsset(rel)
		if !ok {
			return nil, fmt.Errorf("missing template %s", rel)
		}
		if _, err := tmpl.Parse(string(raw)); err != nil {
			return nil, fmt.Errorf("parse %s: %w", rel, err)
		}
	}
	var buf bytes.Buffer
	if err := tmpl.ExecuteTemplate(&buf, name, data); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func extractHighlightTerms(rawQuery string) []string {
	prefixes := []string{"id:", "scope:", "session:", "account:", "user:", "category:", "cat:", "subcat:", "sub:", "tag:", "#", "@"}
	seen := map[string]bool{}
	var out []string
	for _, tok := range strings.Fields(strings.ToLower(strings.TrimSpace(rawQuery))) {
		cleaned := tok
		for _, p := range prefixes {
			if strings.HasPrefix(cleaned, p) {
				cleaned = strings.TrimPrefix(cleaned, p)
				break
			}
		}
		cleaned = strings.TrimSpace(cleaned)
		if cleaned != "" && !seen[cleaned] {
			seen[cleaned] = true
			out = append(out, cleaned)
		}
	}
	sort.Slice(out, func(i, j int) bool { return len(out[i]) > len(out[j]) })
	return out
}

func highlightHTML(rawStr string, terms []string) template.HTML {
	if rawStr == "" || len(terms) == 0 {
		return template.HTML(html.EscapeString(rawStr))
	}
	escaped := make([]string, 0, len(terms))
	for _, tm := range terms {
		if tm != "" {
			escaped = append(escaped, regexp.QuoteMeta(tm))
		}
	}
	if len(escaped) == 0 {
		return template.HTML(html.EscapeString(rawStr))
	}
	re, err := regexp.Compile("(?i)(" + strings.Join(escaped, "|") + ")")
	if err != nil {
		return template.HTML(html.EscapeString(rawStr))
	}
	locs := re.FindAllStringIndex(rawStr, -1)
	if len(locs) == 0 {
		return template.HTML(html.EscapeString(rawStr))
	}
	var sb strings.Builder
	last := 0
	for _, loc := range locs {
		if loc[0] > last {
			sb.WriteString(html.EscapeString(rawStr[last:loc[0]]))
		}
		sb.WriteString(`<mark class="search-hl">`)
		sb.WriteString(html.EscapeString(rawStr[loc[0]:loc[1]]))
		sb.WriteString(`</mark>`)
		last = loc[1]
	}
	if last < len(rawStr) {
		sb.WriteString(html.EscapeString(rawStr[last:]))
	}
	return template.HTML(sb.String())
}

func slicePriorityTags(tags []string, terms []string) []string {
	if len(tags) == 0 {
		return nil
	}
	if len(terms) == 0 {
		if len(tags) > 2 {
			return tags[:2]
		}
		return tags
	}
	cp := append([]string(nil), tags...)
	sort.SliceStable(cp, func(i, j int) bool {
		return hasLineMatch([]string{cp[i]}, terms) && !hasLineMatch([]string{cp[j]}, terms)
	})
	if len(cp) > 3 {
		return cp[:3]
	}
	return cp
}

func hasLineMatch(lines []string, terms []string) bool {
	if len(lines) == 0 || len(terms) == 0 {
		return false
	}
	for _, line := range lines {
		low := strings.ToLower(line)
		for _, tm := range terms {
			if strings.Contains(low, tm) {
				return true
			}
		}
	}
	return false
}

func fmtLatencyMs(ms float64) string {
	if ms <= 0 {
		return "0ms"
	}
	if ms < 1 {
		return "<1ms"
	}
	if ms < 1000 {
		return fmt.Sprintf("%.0fms", ms)
	}
	return fmt.Sprintf("%.2fs", ms/1000.0)
}
