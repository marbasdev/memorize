package progress

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"memorize/internal/domain"
)

type progressCatAgg struct {
	sum     domain.ProgressCategorySummary
	subSeen map[string]bool
	memSeen map[int64]bool
	hasHead bool
}

type progressLaneHead struct {
	id        int64
	status    string
	kind      string
	sessionID string
	summary   string
	nextStep  string
	updatedAt string
	set       bool
}

type progressTrackAgg struct {
	sum           domain.ProgressTrackSummary
	memSeen       map[int64]bool
	hasAnyHead    bool
	hasActiveHead bool
	doingHead     progressLaneHead
	plannedHead   progressLaneHead
}

type progressOverviewBuilder struct {
	out            *domain.ProgressOverview
	kindF, sessF   string
	trkF, catF     string
	stF            string
	doneWindow     string
	catMap         map[string]*progressCatAgg
	catOrder       []string
	trackMap       map[string]*progressTrackAgg
	trackOrder     []string
	sessMap        map[string]*domain.SessionSummary
	sessOrder      []string
	doneCandidates []domain.ProgressRecord
	filtered       []domain.ProgressRecord
}

func newProgressOverviewBuilder(out *domain.ProgressOverview, q domain.ProgressQuery) *progressOverviewBuilder {
	kindF := strings.ToLower(strings.TrimSpace(q.Kind))
	if kindF == "all" {
		kindF = ""
	}
	sessF := strings.TrimSpace(q.SessionID)
	if strings.EqualFold(sessF, "all") {
		sessF = ""
	}
	doneWin := domain.NormalizeDoneWindow(q.DoneWindow)
	out.DoneWindow = doneWin
	return &progressOverviewBuilder{
		out:        out,
		kindF:      kindF,
		sessF:      sessF,
		trkF:       strings.ToLower(strings.TrimSpace(q.Track)),
		catF:       strings.ToLower(strings.TrimSpace(q.Category)),
		stF:        strings.ToLower(strings.TrimSpace(q.Status)),
		doneWindow: doneWin,
		catMap:     make(map[string]*progressCatAgg),
		trackMap:   make(map[string]*progressTrackAgg),
		sessMap:    make(map[string]*domain.SessionSummary),
	}
}

func bumpProgressStatusCounts(total, done, inProg, planned, blocked *int, status string) {
	*total++
	switch status {
	case domain.ProgressStatusDone:
		*done++
	case domain.ProgressStatusInProgress:
		*inProg++
	case domain.ProgressStatusPlanned:
		*planned++
	case domain.ProgressStatusBlocked:
		*blocked++
	}
}

func (b *progressOverviewBuilder) ingest(r domain.ProgressRecord) {
	if r.SessionID != "" {
		sKey := strings.ToLower(r.SessionID)
		ss := b.sessMap[sKey]
		if ss == nil {
			ss = &domain.SessionSummary{
				SessionID: r.SessionID,
				Scope:     r.Scope,
				UpdatedAt: r.UpdatedAt,
			}
			b.sessMap[sKey] = ss
			b.sessOrder = append(b.sessOrder, sKey)
		} else if compareProgressTSDesc(r.UpdatedAt, r.ID, ss.UpdatedAt, 0) {
			ss.UpdatedAt = r.UpdatedAt
		}
		ss.Count++
	}

	tKey := strings.ToLower(r.Track)
	cKey := strings.ToLower(r.Category)

	matchSession := b.sessF == "" || strings.EqualFold(r.SessionID, b.sessF)
	matchTrack := b.trkF == "" || tKey == b.trkF
	matchCat := b.catF == "" || cKey == b.catF || tKey == b.catF
	if matchSession && matchTrack && matchCat {
		b.out.KindCounts[r.Kind]++
	}

	matchKind := b.kindF == "" || r.Kind == b.kindF
	if !(matchKind && matchSession && matchTrack && matchCat) {
		return
	}

	b.out.TotalCount++
	b.out.StatusCounts[r.Status]++

	// Aggregate by Project + Session + Work Track so concurrent projects/sessions show distinctly.
	boardKey := strings.ToLower(r.Scope) + "::" + strings.ToLower(r.SessionID) + "::" + tKey
	if r.Status == domain.ProgressStatusDone {
		b.doneCandidates = append(b.doneCandidates, r)
	}

	b.accumulateTrack(r, boardKey)
	b.accumulateCategory(r, cKey)

	if b.stF == "" || b.stF == "all" || r.Status == b.stF {
		b.filtered = append(b.filtered, r)
	}
}

func (b *progressOverviewBuilder) accumulateTrack(r domain.ProgressRecord, boardKey string) {
	ta := b.trackMap[boardKey]
	if ta == nil {
		ta = &progressTrackAgg{
			sum: domain.ProgressTrackSummary{
				Scope:       r.Scope,
				ProjectName: r.ProjectName,
				SessionID:   r.SessionID,
				Kind:        r.Kind,
				Track:       r.Track,
				Category:    r.Category,
				SubCategory: r.SubCategory,
			},
			memSeen: map[int64]bool{},
		}
		b.trackMap[boardKey] = ta
		b.trackOrder = append(b.trackOrder, boardKey)
	}
	bumpProgressStatusCounts(&ta.sum.TotalCount, &ta.sum.DoneCount, &ta.sum.InProgressCount, &ta.sum.PlannedCount, &ta.sum.BlockedCount, r.Status)
	for _, mid := range r.MemoryIDs {
		ta.memSeen[mid] = true
	}
	if !ta.hasAnyHead {
		ta.hasAnyHead = true
		ta.sum.Status = r.Status
		ta.sum.Kind = r.Kind
		ta.sum.LatestID = r.ID
		ta.sum.LatestSummary = r.Summary
		ta.sum.LatestNextStep = r.NextStep
		ta.sum.LatestUpdatedAt = r.UpdatedAt
	}
	if r.Status != domain.ProgressStatusDone {
		if !ta.hasActiveHead || progressStatusPriority(r.Status) < progressStatusPriority(ta.sum.Status) {
			ta.hasActiveHead = true
			ta.sum.Status = r.Status
			ta.sum.Kind = r.Kind
			ta.sum.LatestID = r.ID
			ta.sum.LatestSummary = r.Summary
			if r.NextStep != "" || ta.sum.LatestNextStep == "" {
				ta.sum.LatestNextStep = r.NextStep
			}
			ta.sum.LatestUpdatedAt = r.UpdatedAt
		} else if ta.sum.LatestNextStep == "" && r.NextStep != "" {
			ta.sum.LatestNextStep = r.NextStep
		}
	}
	switch r.Status {
	case domain.ProgressStatusInProgress, domain.ProgressStatusBlocked:
		if !ta.doingHead.set || (ta.doingHead.status == domain.ProgressStatusBlocked && r.Status == domain.ProgressStatusInProgress) {
			prevNext := ta.doingHead.nextStep
			ta.doingHead = progressLaneHead{
				id: r.ID, status: r.Status, kind: r.Kind, sessionID: r.SessionID,
				summary: r.Summary, nextStep: r.NextStep, updatedAt: r.UpdatedAt, set: true,
			}
			if ta.doingHead.nextStep == "" && prevNext != "" {
				ta.doingHead.nextStep = prevNext
			}
		} else if ta.doingHead.nextStep == "" && r.NextStep != "" {
			ta.doingHead.nextStep = r.NextStep
		}
	case domain.ProgressStatusPlanned:
		if !ta.plannedHead.set {
			ta.plannedHead = progressLaneHead{
				id: r.ID, status: r.Status, kind: r.Kind, sessionID: r.SessionID,
				summary: r.Summary, nextStep: r.NextStep, updatedAt: r.UpdatedAt, set: true,
			}
		} else if ta.plannedHead.nextStep == "" && r.NextStep != "" {
			ta.plannedHead.nextStep = r.NextStep
		}
	}
}

func (b *progressOverviewBuilder) accumulateCategory(r domain.ProgressRecord, cKey string) {
	agg := b.catMap[cKey]
	if agg == nil {
		agg = &progressCatAgg{
			sum:     domain.ProgressCategorySummary{Category: r.Category},
			subSeen: map[string]bool{},
			memSeen: map[int64]bool{},
		}
		b.catMap[cKey] = agg
		b.catOrder = append(b.catOrder, cKey)
	}
	bumpProgressStatusCounts(&agg.sum.TotalCount, &agg.sum.DoneCount, &agg.sum.InProgressCount, &agg.sum.PlannedCount, &agg.sum.BlockedCount, r.Status)
	if r.SubCategory != "" && !agg.subSeen[r.SubCategory] {
		agg.subSeen[r.SubCategory] = true
		agg.sum.SubCategories = append(agg.sum.SubCategories, r.SubCategory)
	}
	for _, mid := range r.MemoryIDs {
		agg.memSeen[mid] = true
	}
	if !agg.hasHead {
		agg.hasHead = true
		agg.sum.LatestStatus = r.Status
		agg.sum.LatestSummary = r.Summary
		agg.sum.LatestNextStep = r.NextStep
		agg.sum.LatestUpdatedAt = r.UpdatedAt
	} else if agg.sum.LatestNextStep == "" && r.NextStep != "" && r.Status != domain.ProgressStatusDone {
		agg.sum.LatestNextStep = r.NextStep
	}
}

func applyProgressLaneHead(base domain.ProgressTrackSummary, status string, h progressLaneHead) domain.ProgressTrackSummary {
	out := base
	out.Status = status
	out.Kind = h.kind
	out.SessionID = h.sessionID
	out.LatestID = h.id
	out.LatestSummary = h.summary
	out.LatestNextStep = h.nextStep
	out.LatestUpdatedAt = h.updatedAt
	return out
}

func compareProgressTSDesc(tsA string, idA int64, tsB string, idB int64) bool {
	ta, okA := ParseProgressTimestamp(tsA)
	tb, okB := ParseProgressTimestamp(tsB)
	if okA && okB && !ta.Equal(tb) {
		return ta.After(tb)
	}
	if okA != okB {
		return okA
	}
	return idA > idB
}

func progressDoneEventTime(r domain.ProgressRecord) (time.Time, bool) {
	t, ok := ParseProgressTimestamp(r.UpdatedAt)
	if !ok {
		t, ok = ParseProgressTimestamp(r.CreatedAt)
	}
	if r.Source == "git_sync" && len(r.Date) == 10 && (!ok || r.Date < t.Format("2006-01-02")) {
		if ct, err := time.ParseInLocation("2006-01-02 15:04:05", r.Date+" 23:59:59", time.Local); err == nil {
			return ct, true
		}
	}
	return t, ok
}

func doneWindowCutoff(win string, now time.Time) time.Time {
	switch win {
	case "24h":
		return now.Add(-24 * time.Hour)
	case "7d":
		return now.Add(-7 * 24 * time.Hour)
	case "30d":
		return now.Add(-30 * 24 * time.Hour)
	default:
		return time.Time{}
	}
}

func (b *progressOverviewBuilder) finalizeRecentDone() {
	out := b.out
	now := time.Now().In(time.Local)
	cutoff := doneWindowCutoff(b.doneWindow, now)
	out.DoneWindowEnd = now.Format("2006-01-02 15:04")
	if !cutoff.IsZero() {
		out.DoneWindowStart = cutoff.In(time.Local).Format("2006-01-02 15:04")
	}

	type doneEntry struct {
		rec domain.ProgressRecord
		ts  time.Time
		ok  bool
	}
	var matches []doneEntry
	for _, r := range b.doneCandidates {
		dt, ok := progressDoneEventTime(r)
		if !cutoff.IsZero() && ok && dt.Before(cutoff) {
			continue
		}
		matches = append(matches, doneEntry{rec: r, ts: dt, ok: ok})
	}
	out.DoneWindowTotal = len(matches)
	sort.Slice(matches, func(i, j int) bool {
		if matches[i].ok && matches[j].ok && !matches[i].ts.Equal(matches[j].ts) {
			return matches[i].ts.After(matches[j].ts)
		}
		if matches[i].ok != matches[j].ok {
			return matches[i].ok
		}
		return matches[i].rec.ID > matches[j].rec.ID
	})

	seenTrack := make(map[string]bool)
	var overflow []domain.ProgressRecord
	for _, m := range matches {
		if len(out.RecentDone) >= 5 {
			break
		}
		k := strings.ToLower(m.rec.Scope) + "::" + strings.ToLower(m.rec.SessionID) + "::" + strings.ToLower(m.rec.Track)
		if !seenTrack[k] {
			seenTrack[k] = true
			out.RecentDone = append(out.RecentDone, m.rec)
		} else {
			overflow = append(overflow, m.rec)
		}
	}
	for _, r := range overflow {
		if len(out.RecentDone) >= 5 {
			break
		}
		out.RecentDone = append(out.RecentDone, r)
	}
}

func (b *progressOverviewBuilder) finalize(page, pageSize int) {
	out := b.out
	for _, sKey := range b.sessOrder {
		out.Sessions = append(out.Sessions, *b.sessMap[sKey])
	}
	sort.SliceStable(out.Sessions, func(i, j int) bool {
		return compareProgressTSDesc(out.Sessions[i].UpdatedAt, 0, out.Sessions[j].UpdatedAt, 0)
	})

	b.finalizeRecentDone()

	out.FilteredCount = len(b.filtered)
	if out.FilteredCount > 0 {
		out.TotalPages = (out.FilteredCount + pageSize - 1) / pageSize
	}
	if page > out.TotalPages {
		page = out.TotalPages
	}
	out.Page = page
	startIdx := (page - 1) * pageSize
	if startIdx < out.FilteredCount {
		endIdx := min(startIdx+pageSize, out.FilteredCount)
		out.Records = b.filtered[startIdx:endIdx]
	}

	dateIndex := make(map[string]int)
	for _, r := range out.Records {
		idx, exists := dateIndex[r.Date]
		if !exists {
			idx = len(out.Timeline)
			dateIndex[r.Date] = idx
			out.Timeline = append(out.Timeline, domain.ProgressDateGroup{Date: r.Date})
		}
		out.Timeline[idx].Records = append(out.Timeline[idx].Records, r)
	}
	sort.SliceStable(out.Timeline, func(i, j int) bool {
		return out.Timeline[i].Date > out.Timeline[j].Date
	})

	b.finalizeTracks()
	b.finalizeCategories()
}

func (b *progressOverviewBuilder) finalizeTracks() {
	out := b.out
	for _, tKey := range b.trackOrder {
		ta := b.trackMap[tKey]
		ta.sum.LinkedMemoryCount = len(ta.memSeen)
		ta.sum.ActiveCount = ta.sum.TotalCount - ta.sum.DoneCount
		if ta.sum.TotalCount > 0 {
			ta.sum.DonePct = float64(ta.sum.DoneCount*100) / float64(ta.sum.TotalCount)
		}
		out.AllTracks = append(out.AllTracks, ta.sum)
		if ta.sum.ActiveCount > 0 {
			out.ActiveTracks = append(out.ActiveTracks, ta.sum)
		}
		if ta.doingHead.set {
			d := applyProgressLaneHead(ta.sum, ta.doingHead.status, ta.doingHead)
			out.DoingTracks = append(out.DoingTracks, d)
			if d.LatestNextStep != "" {
				out.NextStepItems = append(out.NextStepItems, domain.ProgressNextStepItem{
					ID:            d.LatestID,
					Scope:         d.Scope,
					ProjectName:   d.ProjectName,
					SessionID:     d.SessionID,
					Kind:          d.Kind,
					Track:         d.Track,
					Status:        d.Status,
					NextStep:      d.LatestNextStep,
					ParentSummary: d.LatestSummary,
					UpdatedAt:     d.LatestUpdatedAt,
				})
			}
		}
		if ta.plannedHead.set {
			out.PlannedTracks = append(out.PlannedTracks, applyProgressLaneHead(ta.sum, domain.ProgressStatusPlanned, ta.plannedHead))
		}
	}
	sortByPriorityThenID := func(list []domain.ProgressTrackSummary) {
		sort.Slice(list, func(i, j int) bool {
			pi := progressStatusPriority(list[i].Status)
			pj := progressStatusPriority(list[j].Status)
			if pi != pj {
				return pi < pj
			}
			return compareProgressTSDesc(list[i].LatestUpdatedAt, list[i].LatestID, list[j].LatestUpdatedAt, list[j].LatestID)
		})
	}
	sortByPriorityThenID(out.DoingTracks)
	sortByPriorityThenID(out.ActiveTracks)
	sort.Slice(out.PlannedTracks, func(i, j int) bool {
		return compareProgressTSDesc(out.PlannedTracks[i].LatestUpdatedAt, out.PlannedTracks[i].LatestID, out.PlannedTracks[j].LatestUpdatedAt, out.PlannedTracks[j].LatestID)
	})
	sort.Slice(out.AllTracks, func(i, j int) bool {
		if (out.AllTracks[i].ActiveCount > 0) != (out.AllTracks[j].ActiveCount > 0) {
			return out.AllTracks[i].ActiveCount > 0
		}
		return compareProgressTSDesc(out.AllTracks[i].LatestUpdatedAt, out.AllTracks[i].LatestID, out.AllTracks[j].LatestUpdatedAt, out.AllTracks[j].LatestID)
	})
}

func (b *progressOverviewBuilder) finalizeCategories() {
	out := b.out
	for _, cKey := range b.catOrder {
		agg := b.catMap[cKey]
		sort.Strings(agg.sum.SubCategories)
		agg.sum.LinkedMemoryCount = len(agg.memSeen)
		if agg.sum.TotalCount > 0 {
			agg.sum.DonePct = float64(agg.sum.DoneCount*100) / float64(agg.sum.TotalCount)
		}
		out.Categories = append(out.Categories, agg.sum)
	}
	sort.Slice(out.Categories, func(i, j int) bool {
		activeI := out.Categories[i].InProgressCount + out.Categories[i].BlockedCount
		activeJ := out.Categories[j].InProgressCount + out.Categories[j].BlockedCount
		if activeI != activeJ {
			return activeI > activeJ
		}
		if out.Categories[i].TotalCount != out.Categories[j].TotalCount {
			return out.Categories[i].TotalCount > out.Categories[j].TotalCount
		}
		return out.Categories[i].Category < out.Categories[j].Category
	})
}

// GetProgressOverviewWithQuery builds the complete filtered, paginated progress board and timeline overview.
func (s *Service) GetProgressOverviewWithQuery(ctx context.Context, q domain.ProgressQuery) (domain.ProgressOverview, error) {
	rawSc := strings.TrimSpace(q.Scope)
	sqlScope := ""
	if rawSc != "" && !strings.EqualFold(rawSc, "all") {
		sqlScope = domain.NormalizeScope(rawSc)
	}
	s.upgradeLegacyMemorySyncRows(ctx, sqlScope)

	pageSize := q.PageSize
	if pageSize <= 0 {
		pageSize = 10
	}
	if pageSize > 200 {
		pageSize = 200
	}
	page := q.Page
	if page <= 0 {
		page = 1
	}

	out := domain.ProgressOverview{
		Scope:         sqlScope,
		Page:          1,
		PageSize:      pageSize,
		TotalPages:    1,
		StatusCounts:  map[string]int{},
		KindCounts:    map[string]int{},
		Sessions:      []domain.SessionSummary{},
		ActiveTracks:  []domain.ProgressTrackSummary{},
		DoingTracks:   []domain.ProgressTrackSummary{},
		PlannedTracks: []domain.ProgressTrackSummary{},
		NextStepItems: []domain.ProgressNextStepItem{},
		RecentDone:    []domain.ProgressRecord{},
		AllTracks:     []domain.ProgressTrackSummary{},
		Categories:    []domain.ProgressCategorySummary{},
		Timeline:      []domain.ProgressDateGroup{},
		Records:       []domain.ProgressRecord{},
	}
	if sqlScope == "" {
		out.Sync = s.InspectAllProjectsProgressSync(ctx)
	} else if strings.HasPrefix(sqlScope, "project:") {
		out.Sync = s.InspectProgressSync(ctx, sqlScope, q.ExplicitRepo)
	}

	rows, err := s.db.QueryContext(ctx, `
		SELECT id, scope, COALESCE(session_id, ''), COALESCE(track, ''), category, COALESCE(sub_category, ''), status,
		       summary, COALESCE(details, ''), COALESCE(next_step, ''),
		       COALESCE(memory_ids, ''), COALESCE(commit_sha, ''), COALESCE(source, 'manual'),
		       COALESCE(created_at, ''), COALESCE(updated_at, '')
		FROM project_progress
		WHERE (? = '' OR scope = ?)
		ORDER BY id DESC
		LIMIT 2000
	`, sqlScope, sqlScope)
	if err != nil {
		return out, err
	}
	defer rows.Close()

	builder := newProgressOverviewBuilder(&out, q)
	for rows.Next() {
		var r domain.ProgressRecord
		var memStr string
		if err := rows.Scan(&r.ID, &r.Scope, &r.SessionID, &r.Track, &r.Category, &r.SubCategory, &r.Status,
			&r.Summary, &r.Details, &r.NextStep, &memStr, &r.CommitSHA, &r.Source, &r.CreatedAt, &r.UpdatedAt); err != nil {
			return out, err
		}
		r.ProjectName = domain.ProjectShortName(r.Scope)
		r.Track = domain.EffectiveProgressTrack(r.Track, r.Category, r.SubCategory)
		r.MemoryIDs = ParseMemoryIDsStr(memStr)
		r.Kind = domain.ProgressWorkKind(r.Source, r.CommitSHA, r.MemoryIDs)
		r.DetailsLines = splitDetailLines(r.Details, r.Summary)
		extractProgressDateTime(&r)
		builder.ingest(r)
	}
	if err := rows.Err(); err != nil {
		return out, err
	}

	builder.finalize(page, pageSize)
	return out, nil
}

// FormatActiveProgressForSession builds a compact summary of ongoing ('in_progress', 'blocked')
// and planned ('planned') work tracks for SessionStart injection so new sessions inherit open work.
func (s *Service) FormatActiveProgressForSession(ctx context.Context, rawScope string) string {
	sc := domain.NormalizeScope(rawScope)
	if !strings.HasPrefix(sc, "project:") {
		return ""
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, COALESCE(track, ''), category, COALESCE(sub_category, ''), status, summary, COALESCE(next_step, '')
		FROM project_progress
		WHERE scope = ? AND status IN ('in_progress', 'blocked', 'planned')
		ORDER BY id DESC
		LIMIT 12
	`, sc)
	if err != nil {
		return ""
	}
	defer rows.Close()

	var lines []string
	for rows.Next() {
		var id int64
		var trk, cat, sub, st, sum, next string
		if rows.Scan(&id, &trk, &cat, &sub, &st, &sum, &next) == nil {
			eff := domain.EffectiveProgressTrack(trk, cat, sub)
			line := fmt.Sprintf("- [#%d] **[%s]** (`%s`): %s", id, eff, st, strings.TrimSpace(sum))
			if strings.TrimSpace(next) != "" {
				line += " -> Next: " + strings.TrimSpace(next)
			}
			lines = append(lines, line)
		}
	}
	if len(lines) == 0 {
		return ""
	}
	var sb strings.Builder
	sb.WriteString("[PROJECT PROGRESS STATE — project_progress: planned | in_progress | blocked | done]\n")
	sb.WriteString(strings.Join(lines, "\n"))
	return sb.String()
}
