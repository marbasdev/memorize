package dashboard

import (
	"context"
	"encoding/json"
	"fmt"
	"html/template"
	"math"
	"net/url"
	"sort"
	"strconv"
	"strings"

	"memorize/internal/domain"
	"memorize/internal/inference/discovery"
	"memorize/internal/modules/progress"
)

type ssrTabOption struct {
	ID    string
	Label string
}

type ssrHistBar struct {
	Label    string
	Count    int
	HeightPx int
}

type ssrHistogramData struct {
	OOB  bool
	Bars []ssrHistBar
}

type ssrProgressChipData struct {
	Lang        string
	CurScope    string
	CurSession  string
	CurKind     string
	Scope       string
	ProjectName string
	SessionID   string
	Kind        string
}

func toggleFilterJSON(key, cur, target string, extra map[string]string) string {
	next := target
	if strings.EqualFold(strings.TrimSpace(cur), strings.TrimSpace(target)) {
		next = ""
	}
	payload := map[string]string{key: next, "page": "1"}
	for k, v := range extra {
		payload[k] = v
	}
	b, _ := json.Marshal(payload)
	return string(b)
}

type ssrMemoriesData struct {
	Lang             string
	OOB              bool
	Stats            domain.DashboardStats
	GeneralCount     int
	Histogram        ssrHistogramData
	Distribution     domain.ScopeDistribution
	Query            domain.RecordQuery
	HasCustomFilter  bool
	IncludePersonal  bool
	CurrentAccount   string
	Scopes           []string
	Sessions         []domain.SessionSummary
	Accounts         []string
	Categories       []string
	SubCategories    []string
	AllSubCategories map[string][]string
	Page             domain.RecordPage
	PageActiveIDs    []int64
	PageNums         []string
	PageSizes        []int
	PageInfo         string
	HighlightTerms   []string
}

type ssrGuardrailsData struct {
	Lang          string
	Scopes        []string
	GuardrailLogs ssrGuardrailLogsData
}

type ssrCalibrateData struct {
	Lang            string
	Thresholds      domain.Thresholds
	HistScope       string
	HistActiveCount int
	Histogram       ssrHistogramData
	Scopes          []string
}

type ssrPageData struct {
	Lang       string
	Memories   ssrMemoriesData
	Progress   ssrProgressData
	Guardrails ssrGuardrailsData
	Engines    ssrEnginesData
	Calibrate  ssrCalibrateData
}

type ssrDetailModalData struct {
	Lang             string
	Record           domain.MemoryRecord
	Scopes           []string
	Categories       []string
	AllSubCategories map[string][]string
}

type ssrMergeBucket struct {
	Key   string
	Label string
}

type ssrMergeCatSummary struct {
	Category    string
	GroupCount  int
	SourceCount int
}

type ssrRebalanceCatSummary struct {
	Category string
	Count    int
}

type ssrConsolidateModalData struct {
	Lang                  string
	AIMode                bool
	RebalanceMode         bool
	ManualMode            bool
	Scope                 string
	Scopes                []string
	Buckets               []ssrMergeBucket
	SelectedBucket        string
	Count                 int
	MaxCount              int
	Category              string
	Pinned                bool
	Candidates            []domain.MemoryRecord
	PickedIDs             map[int64]bool
	PickedCount           int
	DraftText             string
	Proposals             []domain.ConsolidationProposal
	CatSummaries          []ssrMergeCatSummary
	RebalanceItems        []domain.TaxonomyRebalanceItem
	RebalanceCatSummaries []ssrRebalanceCatSummary
	TotalActionCount      int
	SourceLabel           string
}

type ssrCategoriesModalData struct {
	Lang        string
	PresetsJSON string
}

type ssrPortsModalData struct {
	Lang             string
	ConfiguredDaemon int
	ActiveDaemon     int
	ListenAddr       string
	DaemonRunning    bool
	DaemonPID        int
	DaemonFallback   bool
	ConfiguredGGUF   int
	ActiveGGUF       int
	GGUFEndpoint     string
	GGUFRunning      bool
	GGUFFallback     bool
	GGUFSupported    bool
}

type ssrScoredProbeItem struct {
	ID         int64
	Content    string
	Score      float64
	Confidence float64
	Pass       bool
	Pct        float64
}

type ssrProbeResultsData struct {
	Lang       string
	ElapsedMs  float64
	PassCount  int
	TotalCount int
	Pinned     []domain.Fact
	Scored     []ssrScoredProbeItem
}

type ssrVerifyResultsData struct {
	Passed        bool
	Output        string
	GuardrailLogs ssrGuardrailLogsData
}

var ssrTemplateFiles = []string{
	"templates/layout.html",
	"templates/memories.html",
	"templates/progress.html",
	"templates/guardrails.html",
	"templates/engines.html",
	"templates/telemetry.html",
	"templates/calibrate.html",
	"templates/modals.html",
}

func ssrFuncMap() template.FuncMap {
	return template.FuncMap{
		"t": uiText,
		"jsonAttr": func(v any) string {
			b, err := json.Marshal(v)
			if err != nil {
				return "null"
			}
			return string(b)
		},
		"shortSession": func(id string) string {
			if len(id) > 12 {
				return id[:8]
			}
			return id
		},
		"highlight":  highlightHTML,
		"sliceTags":  slicePriorityTags,
		"hasLineHit": hasLineMatch,
		"derefInt64": func(p *int64) int64 {
			if p != nil {
				return *p
			}
			return 0
		},
		"sub":   func(a, b int) int { return a - b },
		"add":   func(a, b int) int { return a + b },
		"upper": strings.ToUpper,
		"joinIDs": func(ids []int64) string {
			parts := make([]string, len(ids))
			for i, id := range ids {
				parts[i] = strconv.FormatInt(id, 10)
			}
			return strings.Join(parts, ",")
		},
		"fmtBytes": func(n int64) string {
			if n <= 0 {
				return "0 MB"
			}
			gb := float64(n) / (1024 * 1024 * 1024)
			if gb >= 1.0 {
				return fmt.Sprintf("%.2f GB", gb)
			}
			return fmt.Sprintf("%.1f MB", float64(n)/(1024*1024))
		},
		"fmtFloat": func(v float64, prec int) string {
			return strconv.FormatFloat(v, 'f', prec, 64)
		},
		"fmtLatency": fmtLatencyMs,
		"clampPct": func(v float64) float64 {
			if v <= 0 {
				return 5
			}
			return math.Min(100, math.Max(2, v))
		},
		"fitBadgeClass": func(status string) string {
			switch status {
			case "optimal":
				return "b-ok"
			case "tight":
				return "b-warn"
			default:
				return "b-danger"
			}
		},
		"presetIDOrEmpty": func(id string) string {
			if strings.HasPrefix(id, "hf:") {
				return ""
			}
			return id
		},
		"joinCSV": func(items []string) string {
			return strings.Join(items, ", ")
		},
		"pctOf": func(part, total int) float64 {
			if total <= 0 {
				return 0
			}
			return float64(part*100) / float64(total)
		},
		"progStatusBadge": func(status string) string {
			switch status {
			case domain.ProgressStatusDone:
				return "b-ok"
			case domain.ProgressStatusInProgress:
				return "b-accent"
			case domain.ProgressStatusNeedsSync:
				return "b-warn"
			case domain.ProgressStatusBlocked:
				return "b-danger"
			default:
				return "b-muted"
			}
		},
		"progStatusLabel": func(lang, status string) string {
			return uiText(lang, "prog_st_"+status)
		},
		"progKindBadge": func(kind string) string {
			switch kind {
			case domain.ProgressKindMemory:
				return "b-warn"
			case domain.ProgressKindGit:
				return "b-ok"
			default:
				return "b-accent"
			}
		},
		"progKindLabel": func(lang, kind string) string {
			switch kind {
			case domain.ProgressKindMemory:
				return uiText(lang, "prog_kind_memory")
			case domain.ProgressKindGit:
				return uiText(lang, "prog_kind_git")
			default:
				return uiText(lang, "prog_kind_work")
			}
		},
		"progChips": func(root ssrProgressData, scope, projectName, sessionID, kind string) ssrProgressChipData {
			return ssrProgressChipData{
				Lang:        root.Lang,
				CurScope:    root.Scope,
				CurSession:  root.SessionFilter,
				CurKind:     root.KindFilter,
				Scope:       scope,
				ProjectName: projectName,
				SessionID:   sessionID,
				Kind:        kind,
			}
		},
		"scopeToggleJSON": func(cur, target string) string {
			return toggleFilterJSON("scope", cur, target, nil)
		},
		"sessionToggleJSON": func(cur, target string) string {
			return toggleFilterJSON("session_id", cur, target, nil)
		},
		"kindToggleJSON": func(cur, target string) string {
			return toggleFilterJSON("kind", cur, target, nil)
		},
		"trackToggleJSON": func(cur, target string) string {
			return toggleFilterJSON("track", cur, target, map[string]string{"category": ""})
		},
		"guardrailBadge": func(verdict string) string {
			switch verdict {
			case domain.GuardrailVerdictRuleViolation:
				return "b-danger"
			case domain.GuardrailVerdictPreMutateGate, domain.GuardrailVerdictPlanViolation:
				return "b-warn"
			default:
				return "b-ok"
			}
		},
		"guardrailVerdictLabel": func(lang, verdict string) string {
			return uiText(lang, "gr_verdict_"+verdict)
		},
		"guardrailSourceLabel": func(lang, source string) string {
			switch source {
			case "antigravity-pretool":
				return "PreToolUse (Antigravity)"
			case "claude-pretool":
				return "PreToolUse (Claude)"
			case "git-precommit":
				return "Git Pre-Commit"
			case "mcp:verify_plan":
				return "MCP verify_plan"
			case "ui:verify":
				return uiText(lang, "gr_src_ui")
			default:
				return source
			}
		},
		"splitLines": func(raw string) []string {
			var out []string
			for _, l := range strings.Split(raw, "\n") {
				if t := strings.TrimSpace(l); t != "" {
					out = append(out, t)
				}
			}
			return out
		},
		"fmtLocalTimestamp": func(raw string) string {
			if t, ok := progress.ParseProgressTimestamp(raw); ok {
				return t.Format("2006-01-02 15:04:05 MST")
			}
			return strings.TrimSpace(raw)
		},
		"fmtProgressStamp":  fmtProgressStamp,
		"fmtDoneItemStamp":  fmtDoneItemStamp,
		"fmtDoneWindowDesc": fmtDoneWindowDesc,
	}
}

func buildHistogramData(bins []int, oob bool) ssrHistogramData {
	labels := [5]string{"<.80", ".80-.85", ".85-.90", ".90-.95", ">=.95"}
	maxC := 1
	for _, c := range bins {
		if c > maxC {
			maxC = c
		}
	}
	bars := make([]ssrHistBar, 5)
	for i := 0; i < 5; i++ {
		cnt := 0
		if i < len(bins) {
			cnt = bins[i]
		}
		h := int(math.Round((float64(cnt) / float64(maxC)) * 38.0))
		if h < 4 {
			h = 4
		}
		bars[i] = ssrHistBar{Label: labels[i], Count: cnt, HeightPx: h}
	}
	return ssrHistogramData{OOB: oob, Bars: bars}
}

func buildPageNums(cur, total int) []string {
	if total <= 7 {
		out := make([]string, total)
		for i := 0; i < total; i++ {
			out[i] = strconv.Itoa(i + 1)
		}
		return out
	}
	out := []string{"1"}
	left := max(2, cur-1)
	right := min(total-1, cur+1)
	if left > 2 {
		out = append(out, "...")
	}
	for p := left; p <= right; p++ {
		out = append(out, strconv.Itoa(p))
	}
	if right < total-1 {
		out = append(out, "...")
	}
	out = append(out, strconv.Itoa(total))
	return out
}

func formatPageInfo(lang string, page domain.RecordPage, isFiltered bool) string {
	if page.FilteredRecords == 0 {
		if lang == "ko" {
			if isFiltered {
				return fmt.Sprintf("조건에 맞는 기억 0건 (전체 %d건)", page.TotalRecords)
			}
			return "기억 0건"
		}
		if isFiltered {
			return fmt.Sprintf("0 matching memories (filtered from %d)", page.TotalRecords)
		}
		return "0 memories"
	}
	start := (page.Page-1)*page.PageSize + 1
	end := min(page.FilteredRecords, page.Page*page.PageSize)
	if lang == "ko" {
		if isFiltered {
			return fmt.Sprintf("검색 결과 %d건 중 %d–%d 표시 (전체 %d건)", page.FilteredRecords, start, end, page.TotalRecords)
		}
		return fmt.Sprintf("총 %d건 중 %d–%d 표시", page.TotalRecords, start, end)
	}
	if isFiltered {
		return fmt.Sprintf("Showing %d–%d of %d (filtered from %d)", start, end, page.FilteredRecords, page.TotalRecords)
	}
	return fmt.Sprintf("Showing %d–%d of %d", start, end, page.TotalRecords)
}

func mergeCategoriesAndSubs(presetJSON string, facets domain.DashboardFacets) ([]string, map[string][]string) {
	var presetMap map[string][]string
	_ = json.Unmarshal([]byte(discovery.NormalizePresetCategoriesJSON(presetJSON)), &presetMap)
	catSet := map[string]bool{}
	subMap := map[string]map[string]bool{}
	for c, subs := range presetMap {
		catSet[c] = true
		k := strings.ToLower(c)
		if subMap[k] == nil {
			subMap[k] = map[string]bool{}
		}
		for _, s := range subs {
			subMap[k][s] = true
		}
	}
	for _, c := range facets.Categories {
		catSet[c] = true
	}
	for cKey, subs := range facets.SubCategories {
		if subMap[cKey] == nil {
			subMap[cKey] = map[string]bool{}
		}
		for _, s := range subs {
			subMap[cKey][s] = true
		}
	}
	cats := make([]string, 0, len(catSet))
	for c := range catSet {
		if c != "" {
			cats = append(cats, c)
		}
	}
	sort.Strings(cats)

	outSubs := make(map[string][]string, len(subMap))
	for k, set := range subMap {
		list := make([]string, 0, len(set))
		for s := range set {
			if s != "" {
				list = append(list, s)
			}
		}
		sort.Strings(list)
		outSubs[k] = list
	}
	return cats, outSubs
}

func buildMemoriesView(ctx context.Context, d Deps, lang string, q url.Values, oob bool) (ssrMemoriesData, error) {
	rq := parseRecordQuery(q)
	if rq.State == "" {
		rq.State = "active"
	}
	if rq.PageSize <= 0 {
		rq.PageSize = 20
	}
	stats, err := d.Memory.GetDashboardStatsForScope(ctx, rq.Scope)
	if err != nil {
		return ssrMemoriesData{}, err
	}
	dist, _ := d.Memory.GetCategoryDistribution(ctx, rq.Scope)
	facets, err := d.Memory.GetDashboardFacets(ctx)
	if err != nil {
		return ssrMemoriesData{}, err
	}
	pageRes, err := d.Memory.QueryRecordsPage(ctx, rq)
	if err != nil {
		return ssrMemoriesData{}, err
	}
	cfg := d.Memory.GetConfigMap(ctx)
	cats, allSubMap := mergeCategoriesAndSubs(cfg["preset_categories"], facets)

	var subList []string
	if rq.Category != "" {
		subList = allSubMap[strings.ToLower(rq.Category)]
	} else {
		seen := map[string]bool{}
		for _, list := range allSubMap {
			for _, s := range list {
				if !seen[s] {
					seen[s] = true
					subList = append(subList, s)
				}
			}
		}
		sort.Strings(subList)
	}

	var activeIDs []int64
	for _, r := range pageRes.Records {
		if r.Active {
			activeIDs = append(activeIDs, r.ID)
		}
	}

	hasCustomFilter := rq.Scope != "" || rq.SessionID != "" || rq.Account != "" ||
		rq.Category != "" || rq.SubCategory != "" || strings.TrimSpace(rq.Search) != "" ||
		(rq.State != "" && rq.State != "active")
	isFiltered := hasCustomFilter || rq.State != "all"

	scopes := facets.Scopes
	if len(scopes) == 0 {
		scopes = []string{"global", "personal"}
	}

	return ssrMemoriesData{
		Lang:             lang,
		OOB:              oob,
		Stats:            stats,
		GeneralCount:     max(0, stats.ActiveCount-stats.PinnedCount),
		Histogram:        buildHistogramData(stats.ConfidenceBins, oob),
		Distribution:     dist,
		Query:            rq,
		HasCustomFilter:  hasCustomFilter,
		IncludePersonal:  cfg["include_personal_in_project"] == "1" || (cfg["include_personal_in_project"] == "" && discovery.EnvOrDefault("", "MEMORY_INCLUDE_PERSONAL", "0") == "1"),
		CurrentAccount:   d.Memory.CurrentAccount(ctx),
		Scopes:           scopes,
		Sessions:         facets.Sessions,
		Accounts:         facets.Accounts,
		Categories:       cats,
		SubCategories:    subList,
		AllSubCategories: allSubMap,
		Page:             pageRes,
		PageActiveIDs:    activeIDs,
		PageNums:         buildPageNums(pageRes.Page, pageRes.TotalPages),
		PageSizes:        []int{10, 20, 50, 100},
		PageInfo:         formatPageInfo(lang, pageRes, isFiltered),
		HighlightTerms:   extractHighlightTerms(rq.Search),
	}, nil
}
