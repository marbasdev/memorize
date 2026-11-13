package dashboard

import (
	"fmt"
	"net/http"
	"time"

	"memorize/internal/memory"
)

var streamBootID = time.Now().UnixNano()

func registerRetentionAPIRoutes(mux *http.ServeMux, svc *memory.Service) {
	mux.HandleFunc("/api/events", func(w http.ResponseWriter, r *http.Request) {
		if !isAllowedLocalOrigin(r) {
			writeJSON(w, http.StatusForbidden, map[string]any{"error": "cross-origin request blocked"})
			return
		}
		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "streaming unsupported", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache, no-transform")
		w.Header().Set("Connection", "keep-alive")
		w.Header().Set("X-Accel-Buffering", "no")

		curSeq, ch, unsub := svc.SubscribeDomainChanges()
		defer unsub()

		_, _ = fmt.Fprintf(w, ": connected seq=%d\nretry: 1000\nevent: hello\ndata: {\"boot\":%d,\"seq\":%d}\n\n", curSeq, streamBootID, curSeq)
		flusher.Flush()

		ping := time.NewTicker(25 * time.Second)
		defer ping.Stop()

		for {
			select {
			case <-r.Context().Done():
				return
			case ev := <-ch:
				if ev.Domain == "live" {
					_, _ = fmt.Fprintf(w, "event: live\ndata: {\"seq\":%d}\n\n", ev.Seq)
				} else {
					_, _ = fmt.Fprintf(w, "event: change\ndata: {\"seq\":%d,\"domain\":%q}\n\n", ev.Seq, ev.Domain)
				}
				flusher.Flush()
			case <-ping.C:
				_, _ = fmt.Fprint(w, ": ping\n\n")
				flusher.Flush()
			}
		}
	})
	mux.HandleFunc("/api/live-sessions", func(w http.ResponseWriter, r *http.Request) {
		if !isAllowedLocalOrigin(r) {
			writeJSON(w, http.StatusForbidden, map[string]any{"error": "cross-origin request blocked"})
			return
		}
		if r.Method != http.MethodGet {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "GET required"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"sessions": svc.ListLiveSessions()})
	})
	mux.HandleFunc("/api/retention", func(w http.ResponseWriter, r *http.Request) {
		if !isAllowedLocalOrigin(r) {
			writeJSON(w, http.StatusForbidden, map[string]any{"error": "cross-origin request blocked"})
			return
		}
		if r.Method == http.MethodGet {
			writeJSON(w, http.StatusOK, svc.GetRetentionPolicy(r.Context()))
			return
		}
		postJSON(func(w http.ResponseWriter, r *http.Request) {
			var req struct {
				ProgressDays            *int  `json:"progress_days"`
				GuardrailDays           *int  `json:"guardrail_days"`
				GuardrailIncludeBlocked *bool `json:"guardrail_include_blocked"`
			}
			if !decodeJSON(w, r, &req) {
				return
			}
			ctx := r.Context()
			deletedProg := 0
			deletedGr := 0
			if req.ProgressDays != nil {
				_, d, err := svc.SetProgressRetentionDays(ctx, *req.ProgressDays)
				if err != nil {
					writeErr(w, http.StatusInternalServerError, err)
					return
				}
				deletedProg = d
			}
			if req.GuardrailDays != nil || req.GuardrailIncludeBlocked != nil {
				cur := svc.GetRetentionPolicy(ctx)
				days := cur.GuardrailDays
				if req.GuardrailDays != nil {
					days = *req.GuardrailDays
				}
				_, d, err := svc.SetGuardrailRetentionPolicy(ctx, days, req.GuardrailIncludeBlocked)
				if err != nil {
					writeErr(w, http.StatusInternalServerError, err)
					return
				}
				deletedGr = d
			}
			writeJSON(w, http.StatusOK, map[string]any{
				"policy":            svc.GetRetentionPolicy(ctx),
				"deleted_progress":  deletedProg,
				"deleted_guardrail": deletedGr,
			})
		})(w, r)
	})

	mux.HandleFunc("/api/progress/cleanup", postJSON(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Scope         string `json:"scope"`
			OlderThanDays *int   `json:"older_than_days"`
		}
		if !decodeJSON(w, r, &req) {
			return
		}
		days := 7
		if req.OlderThanDays != nil {
			days = *req.OlderThanDays
		}
		deleted, err := svc.CleanupProgressHistory(r.Context(), req.Scope, days)
		if err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"deleted":         deleted,
			"scope":           req.Scope,
			"older_than_days": days,
		})
	}))

	mux.HandleFunc("/api/guardrail-logs/cleanup", postJSON(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Scope          string `json:"scope"`
			OlderThanDays  *int   `json:"older_than_days"`
			IncludeBlocked bool   `json:"include_blocked"`
		}
		if !decodeJSON(w, r, &req) {
			return
		}
		days := 7
		if req.OlderThanDays != nil {
			days = *req.OlderThanDays
		}
		deleted, err := svc.CleanupGuardrailLogs(r.Context(), req.Scope, days, req.IncludeBlocked)
		if err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"deleted":         deleted,
			"scope":           req.Scope,
			"older_than_days": days,
			"include_blocked": req.IncludeBlocked,
		})
	}))
}
