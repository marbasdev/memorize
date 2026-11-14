package router

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"net"
	"os"
	"sort"
	"strings"
	"time"

	"memorize/internal/domain"
)

const evalTelemetryRetentionCap = 2000

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

// RecordTelemetry persists a bounded runtime latency/outcome sample into eval_telemetry.
func (r *Router) RecordTelemetry(ctx context.Context, task domain.EvalTask, ev domain.Evaluator, start time.Time, itemCount int, evalErr error) {
	if r == nil || r.db == nil || task == "" || ev == nil {
		return
	}
	engine := strings.TrimSpace(ev.Name())
	if engine == "" {
		engine = "unknown"
	}
	elapsedMs := domain.ElapsedMsSince(start)
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

	if _, err := r.db.ExecContext(wCtx, `
		INSERT INTO eval_telemetry (task, engine, elapsed_ms, item_count, status)
		VALUES (?, ?, ?, ?, ?)`,
		string(task), engine, elapsedMs, items, status); err != nil {
		return
	}
	_, _ = r.db.ExecContext(wCtx, `
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

// SummarizeTelemetry aggregates measured per-task and per-engine inference latency across windowHours.
func SummarizeTelemetry(ctx context.Context, db *sql.DB, windowHours int) (domain.EvalTelemetryReport, error) {
	specs := domain.AllEvalTasks()
	out := domain.EvalTelemetryReport{
		WindowHours: windowHours,
		Tasks:       make([]domain.EvalTaskStats, 0, len(specs)),
		ByTask:      make(map[string]domain.EvalTaskStats, len(specs)),
	}
	if db == nil {
		return out, nil
	}

	type acc struct {
		stats      domain.EvalTaskStats
		latencies  []float64
		totalItems int64
	}
	type engAcc struct {
		stats      domain.EvalEngineStats
		latencies  []float64
		totalItems int64
	}
	byTask := make(map[domain.EvalTask]*acc, len(specs))
	for _, sp := range specs {
		byTask[sp.Task] = &acc{stats: domain.EvalTaskStats{Task: sp.Task}}
	}
	byEngine := make(map[string]*engAcc)
	var allLatencies []float64
	var recent []domain.EvalTelemetrySample

	query := `
		SELECT id, task, engine, elapsed_ms, item_count, status, COALESCE(created_at, '')
		FROM eval_telemetry`
	var args []any
	if windowHours > 0 {
		query += ` WHERE created_at >= datetime('now', ?)`
		args = append(args, fmt.Sprintf("-%d hours", windowHours))
	}
	query += ` ORDER BY id ASC`

	rows, err := db.QueryContext(ctx, query, args...)
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
		tKey := domain.EvalTask(strings.TrimSpace(taskRaw))
		a, ok := byTask[tKey]
		if !ok {
			a = &acc{stats: domain.EvalTaskStats{Task: tKey}}
			byTask[tKey] = a
		}
		ea, ok := byEngine[engine]
		if !ok {
			ea = &engAcc{stats: domain.EvalEngineStats{Engine: engine}}
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

		recent = append(recent, domain.EvalTelemetrySample{
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

	engines := make([]domain.EvalEngineStats, 0, len(byEngine))
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

// SummarizeTelemetry returns aggregated per-task and per-engine inference telemetry from the router's DB.
func (r *Router) SummarizeTelemetry(ctx context.Context, windowHours int) (domain.EvalTelemetryReport, error) {
	if r == nil {
		return domain.EvalTelemetryReport{WindowHours: windowHours}, nil
	}
	return SummarizeTelemetry(ctx, r.db, windowHours)
}

// ResetTelemetry clears all recorded inference telemetry rows.
func (r *Router) ResetTelemetry(ctx context.Context) error {
	if r == nil || r.db == nil {
		return nil
	}
	_, err := r.db.ExecContext(ctx, `DELETE FROM eval_telemetry`)
	return err
}
