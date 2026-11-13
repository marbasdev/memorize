package memory

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net"
	"os"
	"sort"
	"strings"
	"time"
)

const evalTelemetryRetentionCap = 2000

// EvalTaskStats summarizes observed runtime latency and outcomes for one EvalTask.
type EvalTaskStats struct {
	Task           EvalTask `json:"task"`
	Calls          int      `json:"calls"`
	OKCount        int      `json:"ok_count"`
	TimeoutCount   int      `json:"timeout_count"`
	ErrorCount     int      `json:"error_count"`
	AvgElapsedMs   float64  `json:"avg_elapsed_ms"`
	P95ElapsedMs   float64  `json:"p95_elapsed_ms"`
	MaxElapsedMs   float64  `json:"max_elapsed_ms"`
	TotalElapsedMs float64  `json:"total_elapsed_ms"`
	AvgItems       float64  `json:"avg_items"`
	LastEngine     string   `json:"last_engine,omitempty"`
	LastElapsedMs  float64  `json:"last_elapsed_ms"`
	LastStatus     string   `json:"last_status,omitempty"`
	LastAt         string   `json:"last_at,omitempty"`
}

// EvalEngineStats summarizes observed runtime latency and outcomes for one provider:model engine.
type EvalEngineStats struct {
	Engine         string  `json:"engine"`
	Calls          int     `json:"calls"`
	OKCount        int     `json:"ok_count"`
	TimeoutCount   int     `json:"timeout_count"`
	ErrorCount     int     `json:"error_count"`
	AvgElapsedMs   float64 `json:"avg_elapsed_ms"`
	P95ElapsedMs   float64 `json:"p95_elapsed_ms"`
	MaxElapsedMs   float64 `json:"max_elapsed_ms"`
	TotalElapsedMs float64 `json:"total_elapsed_ms"`
	AvgItems       float64 `json:"avg_items"`
}

// EvalTelemetrySample represents a single recent evaluator invocation for sparkline visualization.
type EvalTelemetrySample struct {
	ID        int64    `json:"id"`
	Task      EvalTask `json:"task"`
	Engine    string   `json:"engine"`
	ElapsedMs float64  `json:"elapsed_ms"`
	ItemCount int      `json:"item_count"`
	Status    string   `json:"status"`
	CreatedAt string   `json:"created_at"`
}

// EvalTelemetryReport aggregates measured per-task and per-engine inference performance across a lookback window.
type EvalTelemetryReport struct {
	WindowHours    int                      `json:"window_hours"`
	TotalCalls     int                      `json:"total_calls"`
	TotalTimeouts  int                      `json:"total_timeouts"`
	TotalErrors    int                      `json:"total_errors"`
	TotalElapsedMs float64                  `json:"total_elapsed_ms"`
	AvgElapsedMs   float64                  `json:"avg_elapsed_ms"`
	P95ElapsedMs   float64                  `json:"p95_elapsed_ms"`
	Tasks          []EvalTaskStats          `json:"tasks"`
	ByTask         map[string]EvalTaskStats `json:"by_task"`
	Engines        []EvalEngineStats        `json:"engines"`
	RecentSamples  []EvalTelemetrySample    `json:"recent_samples"`
}

func elapsedMsSince(start time.Time) float64 {
	if start.IsZero() {
		return 0
	}
	ms := float64(time.Since(start).Microseconds()) / 1000.0
	if ms < 0 {
		return 0
	}
	return ms
}

func classifyEvalStatus(err, ctxErr error) string {
	if err == nil {
		return "ok"
	}
	if errors.Is(err, context.DeadlineExceeded) ||
		errors.Is(err, context.Canceled) ||
		errors.Is(ctxErr, context.DeadlineExceeded) ||
		errors.Is(ctxErr, context.Canceled) ||
		os.IsTimeout(err) {
		return "timeout"
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return "timeout"
	}
	return "error"
}

func (s *Service) recordEvalTelemetry(ctx context.Context, task EvalTask, ev Evaluator, start time.Time, itemCount int, evalErr error) {
	if s == nil || s.db == nil || task == "" || ev == nil {
		return
	}
	engine := strings.TrimSpace(ev.Name())
	if engine == "" {
		engine = "unknown"
	}
	elapsedMs := elapsedMsSince(start)
	var ctxErr error
	if ctx != nil {
		ctxErr = ctx.Err()
	}
	status := classifyEvalStatus(evalErr, ctxErr)
	items := max(0, itemCount)

	base := context.Background()
	if ctx != nil {
		base = context.WithoutCancel(ctx)
	}
	wCtx, cancel := context.WithTimeout(base, time.Second)
	defer cancel()

	if _, err := s.db.ExecContext(wCtx, `
		INSERT INTO eval_telemetry (task, engine, elapsed_ms, item_count, status)
		VALUES (?, ?, ?, ?, ?)`,
		string(task), engine, elapsedMs, items, status); err != nil {
		return
	}
	_, _ = s.db.ExecContext(wCtx, `
		DELETE FROM eval_telemetry
		WHERE id <= COALESCE((SELECT id FROM eval_telemetry ORDER BY id DESC LIMIT 1 OFFSET ?), 0)`,
		evalTelemetryRetentionCap)
}

func nearestRankP95(samples []float64) float64 {
	n := len(samples)
	if n == 0 {
		return 0
	}
	sorted := make([]float64, n)
	copy(sorted, samples)
	sort.Float64s(sorted)
	idx := int(math.Ceil(0.95*float64(n))) - 1
	if idx < 0 {
		idx = 0
	}
	if idx >= n {
		idx = n - 1
	}
	return sorted[idx]
}

// SummarizeEvalTelemetry computes per-task and per-engine call counts, latency (avg/p95/max),
// timeout/error rates, average evaluated item counts, and recent samples over the past windowHours.
func (s *Service) SummarizeEvalTelemetry(ctx context.Context, windowHours int) (EvalTelemetryReport, error) {
	specs := AllEvalTasks()
	out := EvalTelemetryReport{
		WindowHours: windowHours,
		Tasks:       make([]EvalTaskStats, 0, len(specs)),
		ByTask:      make(map[string]EvalTaskStats, len(specs)),
	}
	if s == nil || s.db == nil {
		return out, nil
	}

	type acc struct {
		stats      EvalTaskStats
		latencies  []float64
		totalItems int64
	}
	type engAcc struct {
		stats      EvalEngineStats
		latencies  []float64
		totalItems int64
	}
	byTask := make(map[EvalTask]*acc, len(specs))
	for _, sp := range specs {
		byTask[sp.Task] = &acc{stats: EvalTaskStats{Task: sp.Task}}
	}
	byEngine := make(map[string]*engAcc)
	var allLatencies []float64
	var recent []EvalTelemetrySample

	query := `
		SELECT id, task, engine, elapsed_ms, item_count, status, COALESCE(created_at, '')
		FROM eval_telemetry`
	var args []any
	if windowHours > 0 {
		query += ` WHERE created_at >= datetime('now', ?)`
		args = append(args, fmt.Sprintf("-%d hours", windowHours))
	}
	query += ` ORDER BY id ASC`

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return out, err
	}
	defer rows.Close()

	const maxRecentSamples = 24
	for rows.Next() {
		var sampleID int64
		var taskRaw, engine, status, createdAt string
		var elapsedMs float64
		var itemCnt int
		if err := rows.Scan(&sampleID, &taskRaw, &engine, &elapsedMs, &itemCnt, &status, &createdAt); err != nil {
			return out, err
		}
		if elapsedMs < 0 {
			elapsedMs = 0
		}
		items := max(0, itemCnt)
		tKey := EvalTask(strings.TrimSpace(taskRaw))
		a, ok := byTask[tKey]
		if !ok {
			a = &acc{stats: EvalTaskStats{Task: tKey}}
			byTask[tKey] = a
		}
		ea, ok := byEngine[engine]
		if !ok {
			ea = &engAcc{stats: EvalEngineStats{Engine: engine}}
			byEngine[engine] = ea
		}

		a.stats.Calls++
		ea.stats.Calls++
		out.TotalCalls++
		switch status {
		case "ok":
			a.stats.OKCount++
			ea.stats.OKCount++
		case "timeout":
			a.stats.TimeoutCount++
			ea.stats.TimeoutCount++
			out.TotalTimeouts++
		default:
			a.stats.ErrorCount++
			ea.stats.ErrorCount++
			out.TotalErrors++
		}

		a.stats.TotalElapsedMs += elapsedMs
		ea.stats.TotalElapsedMs += elapsedMs
		out.TotalElapsedMs += elapsedMs
		if elapsedMs > a.stats.MaxElapsedMs {
			a.stats.MaxElapsedMs = elapsedMs
		}
		if elapsedMs > ea.stats.MaxElapsedMs {
			ea.stats.MaxElapsedMs = elapsedMs
		}
		a.latencies = append(a.latencies, elapsedMs)
		ea.latencies = append(ea.latencies, elapsedMs)
		allLatencies = append(allLatencies, elapsedMs)

		a.totalItems += int64(items)
		ea.totalItems += int64(items)

		a.stats.LastEngine = engine
		a.stats.LastElapsedMs = elapsedMs
		a.stats.LastStatus = status
		a.stats.LastAt = createdAt

		recent = append(recent, EvalTelemetrySample{
			ID:        sampleID,
			Task:      tKey,
			Engine:    engine,
			ElapsedMs: elapsedMs,
			ItemCount: items,
			Status:    status,
			CreatedAt: createdAt,
		})
		if len(recent) > maxRecentSamples {
			recent = recent[len(recent)-maxRecentSamples:]
		}
	}
	if err := rows.Err(); err != nil {
		return out, err
	}

	if out.TotalCalls > 0 {
		out.AvgElapsedMs = out.TotalElapsedMs / float64(out.TotalCalls)
		out.P95ElapsedMs = nearestRankP95(allLatencies)
	}
	out.RecentSamples = recent

	for _, sp := range specs {
		a := byTask[sp.Task]
		st := a.stats
		if st.Calls > 0 {
			st.AvgElapsedMs = st.TotalElapsedMs / float64(st.Calls)
			st.P95ElapsedMs = nearestRankP95(a.latencies)
			st.AvgItems = float64(a.totalItems) / float64(st.Calls)
		}
		out.Tasks = append(out.Tasks, st)
		out.ByTask[string(sp.Task)] = st
	}

	engines := make([]EvalEngineStats, 0, len(byEngine))
	for _, ea := range byEngine {
		st := ea.stats
		if st.Calls > 0 {
			st.AvgElapsedMs = st.TotalElapsedMs / float64(st.Calls)
			st.P95ElapsedMs = nearestRankP95(ea.latencies)
			st.AvgItems = float64(ea.totalItems) / float64(st.Calls)
		}
		engines = append(engines, st)
	}
	sort.Slice(engines, func(i, j int) bool {
		if engines[i].TotalElapsedMs != engines[j].TotalElapsedMs {
			return engines[i].TotalElapsedMs > engines[j].TotalElapsedMs
		}
		if engines[i].Calls != engines[j].Calls {
			return engines[i].Calls > engines[j].Calls
		}
		return engines[i].Engine < engines[j].Engine
	})
	out.Engines = engines
	return out, nil
}

// ResetEvalTelemetry clears recorded inference latency telemetry so post-tuning runs start fresh.
func (s *Service) ResetEvalTelemetry(ctx context.Context) error {
	if s == nil || s.db == nil {
		return nil
	}
	_, err := s.db.ExecContext(ctx, `DELETE FROM eval_telemetry`)
	return err
}
