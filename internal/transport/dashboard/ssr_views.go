package dashboard

import (
	"encoding/json"
	"fmt"
	"html/template"
	"math"
	"strconv"
	"strings"

	"memorize/internal/domain"
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
	CurKind     string
	Scope       string
	ProjectName string
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
	Lang                 string
	OOB                  bool
	Stats                domain.DashboardStats
	GeneralCount         int
	Histogram            ssrHistogramData
	Distribution         domain.ScopeDistribution
	Query                domain.RecordQuery
	HasCustomFilter      bool
	IncludePersonal      bool
	CurrentAccount       string
	Scopes               []string
	Sessions             []domain.SessionSummary
	Accounts             []string
	Categories           []string
	SubCategories        []string
	AllSubCategories     map[string][]string
	Page                 domain.RecordPage
	PageActiveIDs        []int64
	PageNums             []string
	PageSizes            []int
	PageInfo             string
	HighlightTerms       []string
	SyncEnabled          bool
	PendingSyncConflicts int
	SyncConflicts        []domain.SyncConflict
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
	Team       ssrTeamData
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
	"templates/team.html",
	"templates/team_modals.html",
	"templates/progress.html",
	"templates/guardrails.html",
	"templates/engines.html",
	"templates/telemetry.html",
	"templates/calibrate.html",
	"templates/modals.html",
	"templates/sync_modals.html",
}

func ssrFuncMap() template.FuncMap {
	return template.FuncMap{
		"t":         uiText,
		"hasPrefix": strings.HasPrefix,
		"jsonAttr": func(v any) string {
			b, err := json.Marshal(v)
			if err != nil {
				return "null"
			}
			return string(b)
		},
		"jsonObj": func(kv ...string) string {
			m := make(map[string]string, len(kv)/2)
			for i := 0; i+1 < len(kv); i += 2 {
				m[kv[i]] = kv[i+1]
			}
			b, err := json.Marshal(m)
			if err != nil {
				return "{}"
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
		"progChips": func(root ssrProgressData, scope, projectName, kind string) ssrProgressChipData {
			return ssrProgressChipData{
				Lang:        root.Lang,
				CurScope:    root.Scope,
				CurKind:     root.KindFilter,
				Scope:       scope,
				ProjectName: projectName,
				Kind:        kind,
			}
		},
		"scopeToggleJSON": func(cur, target string) string {
			return toggleFilterJSON("scope", cur, target, nil)
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
			case domain.GuardrailVerdictPreMutateGate, domain.GuardrailVerdictPlanViolation, domain.GuardrailVerdictPartial, domain.GuardrailVerdictError:
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
			case "mcp:verify_plan":
				return "MCP verify_plan"
			case "ui:verify":
				return uiText(lang, "gr_src_ui")
			default:
				return domain.FormatHookSourceLabel(source)
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
