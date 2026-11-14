package dashboard

import (
	"math"
	"sort"
	"strings"

	"memorize/internal/domain"
)

type ssrTelemetryTaskBar struct {
	Task           string
	Label          string
	LastEngine     string
	Calls          int
	TimeoutCount   int
	ErrorCount     int
	AvgElapsedMs   float64
	P95ElapsedMs   float64
	MaxElapsedMs   float64
	TotalElapsedMs float64
	AvgItems       float64
	SharePct       float64
	AvgWidthPct    float64
	P95WidthPct    float64
}

type ssrTelemetryEngineBar struct {
	Engine         string
	Calls          int
	TimeoutCount   int
	ErrorCount     int
	AvgElapsedMs   float64
	P95ElapsedMs   float64
	MaxElapsedMs   float64
	TotalElapsedMs float64
	AvgItems       float64
	SharePct       float64
	AvgWidthPct    float64
	P95WidthPct    float64
}

type ssrTelemetrySpark struct {
	ShortTask string
	TaskLabel string
	Engine    string
	ElapsedMs float64
	ItemCount int
	Status    string
	Stamp     string
	HeightPx  int
}

type ssrTelemetryViz struct {
	Window      string
	WindowLabel string
	WindowTabs  []ssrTabOption
	ActiveTasks []ssrTelemetryTaskBar
	Engines     []ssrTelemetryEngineBar
	SparkBars   []ssrTelemetrySpark
	MaxSampleMs float64
	OKCount     int
}

func parseTelemetryWindow(raw string) (string, int) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "1h", "1":
		return "1h", 1
	case "7d", "168":
		return "7d", 168
	case "all", "0":
		return "all", 0
	default:
		return "24h", 24
	}
}

func shortTaskCode(t domain.EvalTask) string {
	switch t {
	case domain.TaskRelevance:
		return "REL"
	case domain.TaskMutation:
		return "MUT"
	case domain.TaskVerify:
		return "VER"
	case domain.TaskCalibrate:
		return "CAL"
	case domain.TaskConsolidate:
		return "CON"
	case domain.TaskProgress:
		return "PRG"
	case domain.TaskTaxonomy:
		return "TAX"
	default:
		s := strings.ToUpper(string(t))
		if len(s) > 3 {
			return s[:3]
		}
		return s
	}
}

func scaleBarPct(v, maxVal float64) float64 {
	if maxVal <= 0 || v <= 0 {
		return 0
	}
	return math.Min(100, math.Max(3, v/maxVal*100))
}

func buildTelemetryViz(lang, twRaw string, rep domain.EvalTelemetryReport) ssrTelemetryViz {
	winID, _ := parseTelemetryWindow(twRaw)
	tabs := []ssrTabOption{
		{ID: "1h", Label: uiText(lang, "tel_win_1h")},
		{ID: "24h", Label: uiText(lang, "tel_win_24h")},
		{ID: "7d", Label: uiText(lang, "tel_win_7d")},
		{ID: "all", Label: uiText(lang, "tel_win_all")},
	}
	winLbl := uiText(lang, "tel_win_"+winID)

	viz := ssrTelemetryViz{
		Window:      winID,
		WindowLabel: winLbl,
		WindowTabs:  tabs,
		OKCount:     max(0, rep.TotalCalls-rep.TotalTimeouts-rep.TotalErrors),
	}

	var maxTaskScale float64
	for _, ts := range rep.Tasks {
		if ts.Calls == 0 {
			continue
		}
		scale := max(ts.P95ElapsedMs, ts.AvgElapsedMs)
		if scale > maxTaskScale {
			maxTaskScale = scale
		}
	}
	for _, ts := range rep.Tasks {
		if ts.Calls == 0 {
			continue
		}
		share := 0.0
		if rep.TotalElapsedMs > 0 {
			share = ts.TotalElapsedMs / rep.TotalElapsedMs * 100
		} else if rep.TotalCalls > 0 {
			share = float64(ts.Calls) / float64(rep.TotalCalls) * 100
		}
		viz.ActiveTasks = append(viz.ActiveTasks, ssrTelemetryTaskBar{
			Task:           string(ts.Task),
			Label:          uiText(lang, "task_"+string(ts.Task)+"_lbl"),
			LastEngine:     ts.LastEngine,
			Calls:          ts.Calls,
			TimeoutCount:   ts.TimeoutCount,
			ErrorCount:     ts.ErrorCount,
			AvgElapsedMs:   ts.AvgElapsedMs,
			P95ElapsedMs:   ts.P95ElapsedMs,
			MaxElapsedMs:   ts.MaxElapsedMs,
			TotalElapsedMs: ts.TotalElapsedMs,
			AvgItems:       ts.AvgItems,
			SharePct:       share,
			AvgWidthPct:    scaleBarPct(ts.AvgElapsedMs, maxTaskScale),
			P95WidthPct:    scaleBarPct(ts.P95ElapsedMs, maxTaskScale),
		})
	}
	sort.Slice(viz.ActiveTasks, func(i, j int) bool {
		if viz.ActiveTasks[i].TotalElapsedMs != viz.ActiveTasks[j].TotalElapsedMs {
			return viz.ActiveTasks[i].TotalElapsedMs > viz.ActiveTasks[j].TotalElapsedMs
		}
		return viz.ActiveTasks[i].Calls > viz.ActiveTasks[j].Calls
	})

	var maxEngScale float64
	for _, es := range rep.Engines {
		scale := max(es.P95ElapsedMs, es.AvgElapsedMs)
		if scale > maxEngScale {
			maxEngScale = scale
		}
	}
	for _, es := range rep.Engines {
		share := 0.0
		if rep.TotalElapsedMs > 0 {
			share = es.TotalElapsedMs / rep.TotalElapsedMs * 100
		}
		viz.Engines = append(viz.Engines, ssrTelemetryEngineBar{
			Engine:         es.Engine,
			Calls:          es.Calls,
			TimeoutCount:   es.TimeoutCount,
			ErrorCount:     es.ErrorCount,
			AvgElapsedMs:   es.AvgElapsedMs,
			P95ElapsedMs:   es.P95ElapsedMs,
			MaxElapsedMs:   es.MaxElapsedMs,
			TotalElapsedMs: es.TotalElapsedMs,
			AvgItems:       es.AvgItems,
			SharePct:       share,
			AvgWidthPct:    scaleBarPct(es.AvgElapsedMs, maxEngScale),
			P95WidthPct:    scaleBarPct(es.P95ElapsedMs, maxEngScale),
		})
	}

	var maxSample float64
	for _, sp := range rep.RecentSamples {
		if sp.ElapsedMs > maxSample {
			maxSample = sp.ElapsedMs
		}
	}
	viz.MaxSampleMs = maxSample
	for _, sp := range rep.RecentSamples {
		h := 6
		if maxSample > 0 && sp.ElapsedMs > 0 {
			h = max(6, int(math.Round(sp.ElapsedMs/maxSample*52)))
		}
		viz.SparkBars = append(viz.SparkBars, ssrTelemetrySpark{
			ShortTask: shortTaskCode(sp.Task),
			TaskLabel: uiText(lang, "task_"+string(sp.Task)+"_lbl"),
			Engine:    sp.Engine,
			ElapsedMs: sp.ElapsedMs,
			ItemCount: sp.ItemCount,
			Status:    sp.Status,
			Stamp:     fmtProgressStamp(lang, sp.CreatedAt),
			HeightPx:  h,
		})
	}

	return viz
}
