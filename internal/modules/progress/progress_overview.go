package progress

import (
	"context"
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

type progressTrackAgg struct {
	sum        domain.ProgressTrackSummary
	memSeen    map[int64]bool
	hasAnyHead bool
}

type progressProjectAgg struct {
	sum              domain.ProgressProjectSummary
	latestID         int64
	recentCandidates []domain.ProgressRecord
}

type progressOverviewBuilder struct {
	out              *domain.ProgressOverview
	kindF            string
	trkF, catF       string
	doneWindow       string
	catMap           map[string]*progressCatAgg
	catOrder         []string
	trackMap         map[string]*progressTrackAgg
	trackOrder       []string
	projMap          map[string]*progressProjectAgg
	projOrder        []string
	recentCandidates []domain.ProgressRecord
	filtered         []domain.ProgressRecord
}

func newProgressOverviewBuilder(out *domain.ProgressOverview, q domain.ProgressQuery) *progressOverviewBuilder {
	kindF := strings.ToLower(strings.TrimSpace(q.Kind))
	if kindF == "all" {
		kindF = ""
	}
	doneWin := domain.NormalizeDoneWindow(q.DoneWindow)
	out.DoneWindow = doneWin
	return &progressOverviewBuilder{
		out:        out,
		kindF:      kindF,
		trkF:       strings.ToLower(strings.TrimSpace(q.Track)),
		catF:       strings.ToLower(strings.TrimSpace(q.Category)),
		doneWindow: doneWin,
		catMap:     make(map[string]*progressCatAgg),
		trackMap:   make(map[string]*progressTrackAgg),
		projMap:    make(map[string]*progressProjectAgg),
	}
}

func (b *progressOverviewBuilder) ingest(r domain.ProgressRecord) {
	tKey := strings.ToLower(r.Track)
	cKey := strings.ToLower(r.Category)

	matchTrack := b.trkF == "" || tKey == b.trkF
	matchCat := b.catF == "" || cKey == b.catF || tKey == b.catF
	if matchTrack && matchCat {
		b.out.KindCounts[r.Kind]++
	}

	matchKind := b.kindF == "" || r.Kind == b.kindF
	if !(matchKind && matchTrack && matchCat) {
		return
	}

	b.out.TotalCount++
	b.recentCandidates = append(b.recentCandidates, r)

	boardKey := strings.ToLower(r.Scope) + "::" + tKey
	b.accumulateProject(r)
	b.accumulateTrack(r, boardKey)
	b.accumulateCategory(r, cKey)

	b.filtered = append(b.filtered, r)
}

func (b *progressOverviewBuilder) accumulateProject(r domain.ProgressRecord) {
	pKey := strings.ToLower(r.Scope)
	pa := b.projMap[pKey]
	if pa == nil {
		pa = &progressProjectAgg{
			sum: domain.ProgressProjectSummary{
				Scope:           r.Scope,
				ProjectName:     r.ProjectName,
				LatestUpdatedAt: r.UpdatedAt,
			},
			latestID: r.ID,
		}
		b.projMap[pKey] = pa
		b.projOrder = append(b.projOrder, pKey)
	} else if compareProgressTSDesc(r.UpdatedAt, r.ID, pa.sum.LatestUpdatedAt, pa.latestID) {
		pa.sum.LatestUpdatedAt = r.UpdatedAt
		pa.latestID = r.ID
	}

	pa.sum.TotalCount++
	switch r.Kind {
	case domain.ProgressKindMemory:
		pa.sum.MemoryCount++
	case domain.ProgressKindGit:
		pa.sum.GitCount++
	default:
		pa.sum.WorkCount++
	}
	pa.recentCandidates = append(pa.recentCandidates, r)
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
	ta.sum.TotalCount++
	for _, mid := range r.MemoryIDs {
		ta.memSeen[mid] = true
	}
	if !ta.hasAnyHead {
		ta.hasAnyHead = true
		ta.sum.Kind = r.Kind
		ta.sum.LatestID = r.ID
		ta.sum.LatestSummary = r.Summary
		ta.sum.LatestNextStep = r.NextStep
		ta.sum.LatestUpdatedAt = r.UpdatedAt
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
	agg.sum.TotalCount++
	if r.SubCategory != "" && !agg.subSeen[r.SubCategory] {
		agg.subSeen[r.SubCategory] = true
		agg.sum.SubCategories = append(agg.sum.SubCategories, r.SubCategory)
	}
	for _, mid := range r.MemoryIDs {
		agg.memSeen[mid] = true
	}
	if !agg.hasHead {
		agg.hasHead = true
		agg.sum.LatestSummary = r.Summary
		agg.sum.LatestNextStep = r.NextStep
		agg.sum.LatestUpdatedAt = r.UpdatedAt
	}
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

type progressWindowEntry struct {
	rec domain.ProgressRecord
	ts  time.Time
	ok  bool
}

func filterAndSortWindowEntries(candidates []domain.ProgressRecord, cutoff time.Time) []progressWindowEntry {
	var matches []progressWindowEntry
	for _, r := range candidates {
		dt, ok := progressDoneEventTime(r)
		if !cutoff.IsZero() && ok && dt.Before(cutoff) {
			continue
		}
		matches = append(matches, progressWindowEntry{rec: r, ts: dt, ok: ok})
	}
	sort.Slice(matches, func(i, j int) bool {
		if matches[i].ok && matches[j].ok && !matches[i].ts.Equal(matches[j].ts) {
			return matches[i].ts.After(matches[j].ts)
		}
		if matches[i].ok != matches[j].ok {
			return matches[i].ok
		}
		return matches[i].rec.ID > matches[j].rec.ID
	})
	return matches
}

func (b *progressOverviewBuilder) finalizeRecentHistory() {
	out := b.out
	now := time.Now().In(time.Local)
	cutoff := doneWindowCutoff(b.doneWindow, now)
	out.DoneWindowEnd = now.Format("2006-01-02 15:04")
	if !cutoff.IsZero() {
		out.DoneWindowStart = cutoff.In(time.Local).Format("2006-01-02 15:04")
	}

	matches := filterAndSortWindowEntries(b.recentCandidates, cutoff)
	out.DoneWindowTotal = len(matches)

	seenTrack := make(map[string]bool)
	var overflow []domain.ProgressRecord
	for _, m := range matches {
		if len(out.RecentHistory) >= 5 {
			break
		}
		k := strings.ToLower(m.rec.Scope) + "::" + strings.ToLower(m.rec.Track)
		if !seenTrack[k] {
			seenTrack[k] = true
			out.RecentHistory = append(out.RecentHistory, m.rec)
		} else {
			overflow = append(overflow, m.rec)
		}
	}
	for _, r := range overflow {
		if len(out.RecentHistory) >= 5 {
			break
		}
		out.RecentHistory = append(out.RecentHistory, r)
	}
}

func (b *progressOverviewBuilder) finalizeProjects() {
	out := b.out
	cutoff := doneWindowCutoff(b.doneWindow, time.Now().In(time.Local))

	for _, pKey := range b.projOrder {
		pa := b.projMap[pKey]
		matches := filterAndSortWindowEntries(pa.recentCandidates, cutoff)
		for _, m := range matches {
			if len(pa.sum.RecentHistory) >= 5 {
				break
			}
			pa.sum.RecentHistory = append(pa.sum.RecentHistory, m.rec)
		}
		out.Projects = append(out.Projects, pa.sum)
	}
	sort.SliceStable(out.Projects, func(i, j int) bool {
		return compareProgressTSDesc(out.Projects[i].LatestUpdatedAt, 0, out.Projects[j].LatestUpdatedAt, 0)
	})
}

func (b *progressOverviewBuilder) finalize(page, pageSize int) {
	out := b.out

	b.finalizeRecentHistory()
	b.finalizeProjects()

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
		out.AllTracks = append(out.AllTracks, ta.sum)
	}
	sort.Slice(out.AllTracks, func(i, j int) bool {
		return compareProgressTSDesc(out.AllTracks[i].LatestUpdatedAt, out.AllTracks[i].LatestID, out.AllTracks[j].LatestUpdatedAt, out.AllTracks[j].LatestID)
	})
}

func (b *progressOverviewBuilder) finalizeCategories() {
	out := b.out
	for _, cKey := range b.catOrder {
		agg := b.catMap[cKey]
		sort.Strings(agg.sum.SubCategories)
		agg.sum.LinkedMemoryCount = len(agg.memSeen)
		out.Categories = append(out.Categories, agg.sum)
	}
	sort.Slice(out.Categories, func(i, j int) bool {
		if out.Categories[i].TotalCount != out.Categories[j].TotalCount {
			return out.Categories[i].TotalCount > out.Categories[j].TotalCount
		}
		return out.Categories[i].Category < out.Categories[j].Category
	})
}

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
		KindCounts:    map[string]int{},
		Projects:      []domain.ProgressProjectSummary{},
		RecentHistory: []domain.ProgressRecord{},
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

	strictPrivate := 0
	if domain.IsServerMode(ctx) {
		strictPrivate = 1
	}
	curAcc := domain.EffectivePrincipalAccount(ctx)
	accFilter := strings.TrimSpace(q.Account)
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, scope, COALESCE(account, ''), COALESCE(session_id, ''), COALESCE(track, ''), category, COALESCE(sub_category, ''),
		       summary, COALESCE(details, ''), COALESCE(next_step, ''),
		       COALESCE(memory_ids, ''), COALESCE(commit_sha, ''), COALESCE(source, 'manual'),
		       COALESCE(created_at, ''), COALESCE(updated_at, '')
		FROM project_progress
		WHERE (? = '' OR scope = ?)
		  AND (? = '' OR LOWER(COALESCE(account, '')) = LOWER(?))
		  AND (? = 0 OR scope NOT IN ('personal', 'global') OR COALESCE(account, '') = ?)
		ORDER BY id DESC
		LIMIT 2000
	`, sqlScope, sqlScope, accFilter, accFilter, strictPrivate, curAcc)
	if err != nil {
		return out, err
	}
	defer rows.Close()

	builder := newProgressOverviewBuilder(&out, q)
	for rows.Next() {
		var r domain.ProgressRecord
		var memStr string
		if err := rows.Scan(&r.ID, &r.Scope, &r.Account, &r.SessionID, &r.Track, &r.Category, &r.SubCategory,
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
