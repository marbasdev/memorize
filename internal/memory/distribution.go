package memory

import (
	"context"
	"math"
	"sort"
	"strings"
)

const UncategorizedSubKey = "__none__"

type SubCategoryDistItem struct {
	SubCategory         string  `json:"sub_category"`
	IsUncategorized     bool    `json:"is_uncategorized,omitempty"`
	ActiveCount         int     `json:"active_count"`
	PinnedCount         int     `json:"pinned_count"`
	GeneralCount        int     `json:"general_count"`
	SharePct            float64 `json:"share_pct"`
	FlexWeight          int     `json:"flex_weight"`
	X                   float64 `json:"x"`
	Y                   float64 `json:"y"`
	W                   float64 `json:"w"`
	H                   float64 `json:"h"`
	DuplicateAcrossCats bool    `json:"duplicate_across_cats,omitempty"`
}

type CategoryDistItem struct {
	Category           string                `json:"category"`
	ActiveCount        int                   `json:"active_count"`
	PinnedCount        int                   `json:"pinned_count"`
	GeneralCount       int                   `json:"general_count"`
	UncategorizedCount int                   `json:"uncategorized_count"`
	TombCount          int                   `json:"tomb_count"`
	SharePct           float64               `json:"share_pct"`
	FlexWeight         int                   `json:"flex_weight"`
	X                  float64               `json:"x"`
	Y                  float64               `json:"y"`
	W                  float64               `json:"w"`
	H                  float64               `json:"h"`
	SubItems           []SubCategoryDistItem `json:"sub_items,omitempty"`
}

type ScopeDistribution struct {
	Scope                  string             `json:"scope"`
	TotalActive            int                `json:"total_active"`
	TotalPinned            int                `json:"total_pinned"`
	TotalTomb              int                `json:"total_tomb"`
	UncategorizedTotal     int                `json:"uncategorized_total"`
	DuplicateSubCategories []string           `json:"duplicate_sub_categories,omitempty"`
	Categories             []CategoryDistItem `json:"categories"`
}

// GetCategoryDistribution computes purely factual 2-level category and sub-category
// memory proportions (including unclassified items) directly in SQLite without hardcoded cleanup heuristics.
func (s *Service) GetCategoryDistribution(ctx context.Context, scopeFilter string) (ScopeDistribution, error) {
	sc := strings.TrimSpace(scopeFilter)

	rows, err := s.db.QueryContext(ctx, `
		SELECT
			LOWER(TRIM(COALESCE(NULLIF(category, ''), 'general'))),
			LOWER(TRIM(COALESCE(sub_category, ''))),
			COALESCE(SUM(CASE WHEN active = 1 THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN active = 1 AND pinned = 1 THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN active = 0 THEN 1 ELSE 0 END), 0)
		FROM memories
		WHERE (? = '' OR scope = ?)
		GROUP BY 1, 2
	`, sc, sc)
	if err != nil {
		return ScopeDistribution{Scope: sc}, err
	}
	defer rows.Close()

	type rawSub struct {
		sub    string
		active int
		pinned int
		tomb   int
	}
	byCat := make(map[string][]rawSub)
	subCatOwners := make(map[string]map[string]bool)
	totalActive, totalPinned, totalTomb, uncatTotal := 0, 0, 0, 0

	for rows.Next() {
		var cat, sub string
		var act, pin, tomb int
		if err := rows.Scan(&cat, &sub, &act, &pin, &tomb); err != nil {
			return ScopeDistribution{Scope: sc}, err
		}
		if cat == "" {
			cat = "general"
		}
		byCat[cat] = append(byCat[cat], rawSub{sub: sub, active: act, pinned: pin, tomb: tomb})
		totalActive += act
		totalPinned += pin
		totalTomb += tomb
		if sub == "" {
			uncatTotal += act
		} else if act > 0 {
			if subCatOwners[sub] == nil {
				subCatOwners[sub] = make(map[string]bool)
			}
			subCatOwners[sub][cat] = true
		}
	}
	if err := rows.Err(); err != nil {
		return ScopeDistribution{Scope: sc}, err
	}

	var dupSubs []string
	for sub, owners := range subCatOwners {
		if len(owners) > 1 {
			dupSubs = append(dupSubs, sub)
		}
	}
	sort.Strings(dupSubs)

	out := ScopeDistribution{
		Scope:                  sc,
		TotalActive:            totalActive,
		TotalPinned:            totalPinned,
		TotalTomb:              totalTomb,
		UncategorizedTotal:     uncatTotal,
		DuplicateSubCategories: dupSubs,
		Categories:             make([]CategoryDistItem, 0, len(byCat)),
	}
	if len(byCat) == 0 {
		return out, nil
	}

	for cat, list := range byCat {
		act, pin, tomb, uncatAct, uncatPin := 0, 0, 0, 0, 0
		var subs []SubCategoryDistItem
		for _, r := range list {
			act += r.active
			pin += r.pinned
			tomb += r.tomb
			if r.active <= 0 {
				continue
			}
			if r.sub == "" {
				uncatAct += r.active
				uncatPin += r.pinned
				continue
			}
			subs = append(subs, SubCategoryDistItem{
				SubCategory:         r.sub,
				ActiveCount:         r.active,
				PinnedCount:         r.pinned,
				GeneralCount:        max(0, r.active-r.pinned),
				DuplicateAcrossCats: len(subCatOwners[r.sub]) > 1,
			})
		}
		if act == 0 {
			continue
		}

		sort.Slice(subs, func(i, j int) bool {
			if subs[i].ActiveCount != subs[j].ActiveCount {
				return subs[i].ActiveCount > subs[j].ActiveCount
			}
			return subs[i].SubCategory < subs[j].SubCategory
		})
		if uncatAct > 0 {
			subs = append(subs, SubCategoryDistItem{
				SubCategory:     UncategorizedSubKey,
				IsUncategorized: true,
				ActiveCount:     uncatAct,
				PinnedCount:     uncatPin,
				GeneralCount:    max(0, uncatAct-uncatPin),
			})
		}
		for i := range subs {
			if act > 0 {
				subs[i].SharePct = math.Round((float64(subs[i].ActiveCount)/float64(act))*1000.0) / 10.0
			}
			subs[i].FlexWeight = max(1, subs[i].ActiveCount)
		}

		share := 0.0
		if totalActive > 0 {
			share = math.Round((float64(act)/float64(totalActive))*1000.0) / 10.0
		}

		out.Categories = append(out.Categories, CategoryDistItem{
			Category:           cat,
			ActiveCount:        act,
			PinnedCount:        pin,
			GeneralCount:       max(0, act-pin),
			UncategorizedCount: uncatAct,
			TombCount:          tomb,
			SharePct:           share,
			FlexWeight:         max(2, act),
			SubItems:           subs,
		})
	}

	sort.Slice(out.Categories, func(i, j int) bool {
		if out.Categories[i].ActiveCount != out.Categories[j].ActiveCount {
			return out.Categories[i].ActiveCount > out.Categories[j].ActiveCount
		}
		if out.Categories[i].TombCount != out.Categories[j].TombCount {
			return out.Categories[i].TombCount > out.Categories[j].TombCount
		}
		return out.Categories[i].Category < out.Categories[j].Category
	})

	const outerCanvasAspect = 5.0
	catWeights := make([]float64, len(out.Categories))
	for i, c := range out.Categories {
		if c.ActiveCount > 0 {
			catWeights[i] = float64(c.ActiveCount)
		}
	}
	catBoxes := layoutTreemap2D(catWeights, outerCanvasAspect, 0.055)
	for i := range out.Categories {
		b := catBoxes[i]
		out.Categories[i].X = b.X
		out.Categories[i].Y = b.Y
		out.Categories[i].W = b.W
		out.Categories[i].H = b.H
		if len(out.Categories[i].SubItems) == 0 || b.W <= 0 || b.H <= 0 {
			continue
		}
		subWeights := make([]float64, len(out.Categories[i].SubItems))
		for j, s := range out.Categories[i].SubItems {
			subWeights[j] = float64(max(1, s.ActiveCount))
		}
		innerAspect := ((b.W / 100.0) * outerCanvasAspect) / math.Max(0.15, (b.H/100.0)*0.82)
		subBoxes := layoutTreemap2D(subWeights, innerAspect, 0.02)
		for j := range out.Categories[i].SubItems {
			sb := subBoxes[j]
			out.Categories[i].SubItems[j].X = sb.X
			out.Categories[i].SubItems[j].Y = sb.Y
			out.Categories[i].SubItems[j].W = sb.W
			out.Categories[i].SubItems[j].H = sb.H
		}
	}
	return out, nil
}

type treemapBox struct {
	X, Y, W, H float64
}

func roundPct2(v float64) float64 {
	return math.Round(v*100.0) / 100.0
}

func worstTreemapAspect(maxArea, minArea, rowSum, side float64) float64 {
	if minArea <= 0 || rowSum <= 0 || side <= 0 {
		return math.MaxFloat64
	}
	sideSq := side * side
	sumSq := rowSum * rowSum
	return math.Max((sideSq*maxArea)/sumSq, sumSq/(sideSq*minArea))
}

// layoutTreemap2D computes a deterministic Bruls-Huizing-van Wijk 2D squarified treemap
// in [0..100]% coordinates so fixed-height containers reflect proportional area without growing vertically.
func layoutTreemap2D(rawWeights []float64, physAspect float64, minVisualShare float64) []treemapBox {
	n := len(rawWeights)
	boxes := make([]treemapBox, n)
	if n == 0 {
		return boxes
	}
	type weightedIdx struct {
		idx int
		w   float64
	}
	var items []weightedIdx
	rawSum := 0.0
	for i, w := range rawWeights {
		if w > 0 {
			items = append(items, weightedIdx{idx: i, w: w})
			rawSum += w
		}
	}
	if len(items) == 0 || rawSum <= 0 {
		return boxes
	}
	if len(items) == 1 {
		boxes[items[0].idx] = treemapBox{X: 0, Y: 0, W: 100, H: 100}
		return boxes
	}

	floorW := rawSum * minVisualShare
	sumW := 0.0
	for i := range items {
		if floorW > 0 && items[i].w < floorW {
			items[i].w = floorW
		}
		sumW += items[i].w
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].w != items[j].w {
			return items[i].w > items[j].w
		}
		return items[i].idx < items[j].idx
	})

	physW := 100.0 * math.Max(0.25, physAspect)
	physH := 100.0
	totalArea := physW * physH
	areas := make([]float64, len(items))
	for i, it := range items {
		areas[i] = (it.w / sumW) * totalArea
	}

	rx, ry, rw, rh := 0.0, 0.0, physW, physH
	for i := 0; i < len(items); {
		if rw <= 1e-6 || rh <= 1e-6 {
			break
		}
		side := math.Min(rw, rh)
		rowSum := areas[i]
		bestWorst := worstTreemapAspect(areas[i], areas[i], rowSum, side)
		j := i + 1
		for j < len(items) {
			nextSum := rowSum + areas[j]
			nextWorst := worstTreemapAspect(areas[i], areas[j], nextSum, side)
			if nextWorst > bestWorst {
				break
			}
			rowSum = nextSum
			bestWorst = nextWorst
			j++
		}

		if rw >= rh {
			stripW := rowSum / rh
			if j == len(items) || stripW > rw {
				stripW = rw
			}
			cy := ry
			for k := i; k < j; k++ {
				ch := areas[k] / math.Max(1e-6, stripW)
				if k == j-1 {
					ch = (ry + rh) - cy
				}
				boxes[items[k].idx] = treemapBox{
					X: roundPct2((rx / physW) * 100.0),
					Y: roundPct2((cy / physH) * 100.0),
					W: roundPct2((stripW / physW) * 100.0),
					H: roundPct2((math.Max(0, ch) / physH) * 100.0),
				}
				cy += ch
			}
			rx += stripW
			rw -= stripW
		} else {
			stripH := rowSum / rw
			if j == len(items) || stripH > rh {
				stripH = rh
			}
			cx := rx
			for k := i; k < j; k++ {
				cw := areas[k] / math.Max(1e-6, stripH)
				if k == j-1 {
					cw = (rx + rw) - cx
				}
				boxes[items[k].idx] = treemapBox{
					X: roundPct2((cx / physW) * 100.0),
					Y: roundPct2((ry / physH) * 100.0),
					W: roundPct2((math.Max(0, cw) / physW) * 100.0),
					H: roundPct2((stripH / physH) * 100.0),
				}
				cx += cw
			}
			ry += stripH
			rh -= stripH
		}
		i = j
	}
	return boxes
}
