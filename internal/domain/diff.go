package domain

import (
	"sort"
	"strings"
	"unicode/utf8"
)

// MaxDiffSingleLineBytes caps abnormally long single diff lines (such as minified assets or hashes)
// so one line cannot crowd out surrounding code changes.
const MaxDiffSingleLineBytes = 800

// TruncateUTF8Bytes truncates s to at most maxBytes without splitting a multi-byte UTF-8 rune.
func TruncateUTF8Bytes(s string, maxBytes int) string {
	if maxBytes <= 0 || len(s) <= maxBytes {
		return s
	}
	b := maxBytes
	for b > 0 && !utf8.RuneStart(s[b]) {
		b--
	}
	return s[:b] + "..."
}

// CompactDiffLine caps an individual diff line to maxBytes (default MaxDiffSingleLineBytes) on a UTF-8 boundary.
func CompactDiffLine(line string, maxBytes int) string {
	if maxBytes <= 0 {
		maxBytes = MaxDiffSingleLineBytes
	}
	if len(line) <= maxBytes {
		return line
	}
	return TruncateUTF8Bytes(line, maxBytes)
}

func normalizeDiffTokens(lines []string) []string {
	var out []string
	for _, raw := range lines {
		norm := strings.Join(strings.Fields(raw), " ")
		if norm != "" {
			out = append(out, norm)
		}
	}
	return out
}

// IsWhitespaceOnlyDiffHunk reports whether oldLines and newLines are identical once blank lines
// and horizontal whitespace differences are normalized away.
func IsWhitespaceOnlyDiffHunk(oldLines, newLines []string) bool {
	normOld := normalizeDiffTokens(oldLines)
	normNew := normalizeDiffTokens(newLines)
	if len(normOld) != len(normNew) {
		return false
	}
	for i := range normOld {
		if normOld[i] != normNew[i] {
			return false
		}
	}
	return true
}

// CompactUnifiedDiffBody strips git header boilerplate from a single file's unified diff,
// collapses whitespace/blank-line-only hunks, and caps single giant lines.
func CompactUnifiedDiffBody(raw string) (body string, whitespaceOnly bool) {
	lines := strings.Split(strings.ReplaceAll(raw, "\r\n", "\n"), "\n")

	type diffHunk struct {
		header   string
		oldLines []string
		newLines []string
		body     []string
	}
	var meta []string
	var hunks []diffHunk
	var cur *diffHunk
	flush := func() {
		if cur != nil {
			hunks = append(hunks, *cur)
			cur = nil
		}
	}

	for _, l := range lines {
		switch {
		case strings.HasPrefix(l, "diff --git "),
			strings.HasPrefix(l, "index "),
			strings.HasPrefix(l, "--- "),
			strings.HasPrefix(l, "+++ "),
			l == `\ No newline at end of file`:
			continue
		case strings.HasPrefix(l, "new file mode "),
			strings.HasPrefix(l, "deleted file mode "),
			strings.HasPrefix(l, "rename from "),
			strings.HasPrefix(l, "rename to "),
			strings.HasPrefix(l, "copy from "),
			strings.HasPrefix(l, "copy to "):
			meta = append(meta, l)
		case strings.HasPrefix(l, "@@ "):
			flush()
			cur = &diffHunk{header: CompactDiffLine(l, MaxDiffSingleLineBytes)}
		case cur != nil:
			if strings.HasPrefix(l, "-") {
				cur.oldLines = append(cur.oldLines, l[1:])
				cur.body = append(cur.body, "-"+CompactDiffLine(l[1:], MaxDiffSingleLineBytes))
			} else if strings.HasPrefix(l, "+") {
				cur.newLines = append(cur.newLines, l[1:])
				cur.body = append(cur.body, "+"+CompactDiffLine(l[1:], MaxDiffSingleLineBytes))
			} else if l != "" {
				cur.body = append(cur.body, CompactDiffLine(l, MaxDiffSingleLineBytes))
			}
		default:
			if trimmed := strings.TrimSpace(l); trimmed != "" {
				meta = append(meta, CompactDiffLine(trimmed, MaxDiffSingleLineBytes))
			}
		}
	}
	flush()

	var semantic []string
	wsHunks := 0
	for _, h := range hunks {
		if IsWhitespaceOnlyDiffHunk(h.oldLines, h.newLines) {
			wsHunks++
			continue
		}
		if h.header != "" {
			semantic = append(semantic, h.header)
		}
		semantic = append(semantic, h.body...)
	}

	var out []string
	out = append(out, meta...)
	out = append(out, semantic...)
	if len(out) == 0 && wsHunks > 0 {
		return "(whitespace/formatting only)", true
	}
	return strings.TrimSpace(strings.Join(out, "\n")), false
}

func formatDiffLines(prefix string, lines []string) string {
	var sb strings.Builder
	for i, l := range lines {
		if i > 0 {
			sb.WriteByte('\n')
		}
		sb.WriteString(prefix)
		sb.WriteString(CompactDiffLine(l, MaxDiffSingleLineBytes))
	}
	return sb.String()
}

func trimChangedLineEdges(oldRaw, newRaw string) (oldMid, newMid []string) {
	oldLines := strings.Split(oldRaw, "\n")
	newLines := strings.Split(newRaw, "\n")
	p := 0
	for p < len(oldLines) && p < len(newLines) && oldLines[p] == newLines[p] {
		p++
	}
	oldRem := oldLines[p:]
	newRem := newLines[p:]
	s := 0
	for s < len(oldRem) && s < len(newRem) && oldRem[len(oldRem)-1-s] == newRem[len(newRem)-1-s] {
		s++
	}
	return oldRem[:len(oldRem)-s], newRem[:len(newRem)-s]
}

func formatEdgeDiff(oldMid, newMid []string, addPrefix string, maxBytes int) string {
	if len(oldMid) == 0 && len(newMid) == 0 {
		return ""
	}
	if len(oldMid) > 0 && len(newMid) > 0 {
		removed := formatDiffLines("- ", oldMid)
		added := formatDiffLines("+ ", newMid)
		combined := removed + "\n" + added
		if maxBytes > 0 && len(combined) > maxBytes {
			half := max(64, (maxBytes-50)/2)
			combined = TruncateUTF8Bytes(removed, half) + "\n" + TruncateUTF8Bytes(added, half)
		}
		return combined
	}
	if len(newMid) > 0 {
		return TruncateUTF8Bytes(formatDiffLines(addPrefix, newMid), maxBytes)
	}
	return TruncateUTF8Bytes("Removed:\n"+formatDiffLines("", oldMid), maxBytes)
}

// DiffEditSnippet builds a bounded changed-line summary between oldRaw and newRaw for tool fallback snippets.
func DiffEditSnippet(oldRaw, newRaw string) string {
	oldMid, newMid := trimChangedLineEdges(oldRaw, newRaw)
	return formatEdgeDiff(oldMid, newMid, "", 950)
}

// FormatChangedLineHunks emits only added/removed line hunks between oldRaw and newRaw
// using unique matching anchors (patience LIS) in O(m+n) time and space, collapsing
// pure whitespace-only formatting hunks.
func FormatChangedLineHunks(oldRaw, newRaw string, maxBytes int) string {
	oldMid, newMid := trimChangedLineEdges(oldRaw, newRaw)
	m, n := len(oldMid), len(newMid)
	if m == 0 || n == 0 {
		if IsWhitespaceOnlyDiffHunk(oldMid, newMid) {
			return "(whitespace/formatting only)"
		}
		return formatEdgeDiff(oldMid, newMid, "+ ", maxBytes)
	}

	cntO, cntN, posN := make(map[string]int, m), make(map[string]int, n), make(map[string]int, n)
	for _, l := range oldMid {
		cntO[l]++
	}
	for j, l := range newMid {
		cntN[l]++
		posN[l] = j
	}
	type anchor struct{ o, n int }
	var anchors []anchor
	for i, l := range oldMid {
		if cntO[l] == 1 && cntN[l] == 1 {
			anchors = append(anchors, anchor{i, posN[l]})
		}
	}
	if len(anchors) == 0 {
		if IsWhitespaceOnlyDiffHunk(oldMid, newMid) {
			return "(whitespace/formatting only)"
		}
		return formatEdgeDiff(oldMid, newMid, "+ ", maxBytes)
	}

	tails := make([]int, 0, len(anchors))
	prev := make([]int, len(anchors))
	for i, a := range anchors {
		pos := sort.Search(len(tails), func(k int) bool { return anchors[tails[k]].n >= a.n })
		prev[i] = -1
		if pos > 0 {
			prev[i] = tails[pos-1]
		}
		if pos == len(tails) {
			tails = append(tails, i)
		} else {
			tails[pos] = i
		}
	}
	var kept []anchor
	for idx := tails[len(tails)-1]; idx >= 0; idx = prev[idx] {
		kept = append(kept, anchors[idx])
	}
	for l, r := 0, len(kept)-1; l < r; l, r = l+1, r-1 {
		kept[l], kept[r] = kept[r], kept[l]
	}

	type hunk struct{ oldLines, newLines []string }
	var hunks []hunk
	prevO, prevN := 0, 0
	for _, a := range kept {
		if a.o < prevO || a.n < prevN {
			continue
		}
		aO, aN := a.o, a.n
		for aO > prevO && aN > prevN && oldMid[aO-1] == newMid[aN-1] {
			aO--
			aN--
		}
		if prevO < aO || prevN < aN {
			hunks = append(hunks, hunk{oldMid[prevO:aO], newMid[prevN:aN]})
		}
		bO, bN := a.o+1, a.n+1
		for bO < m && bN < n && oldMid[bO] == newMid[bN] {
			bO++
			bN++
		}
		prevO, prevN = bO, bN
	}
	if prevO < m || prevN < n {
		hunks = append(hunks, hunk{oldMid[prevO:], newMid[prevN:]})
	}
	if len(hunks) == 0 {
		return ""
	}

	var semantic []hunk
	wsHunks := 0
	for _, h := range hunks {
		if IsWhitespaceOnlyDiffHunk(h.oldLines, h.newLines) {
			wsHunks++
			continue
		}
		semantic = append(semantic, h)
	}
	if len(semantic) == 0 && wsHunks > 0 {
		return "(whitespace/formatting only)"
	}
	hunks = semantic

	perCap := maxBytes
	if len(hunks) > 1 && maxBytes > 0 {
		perCap = max(200, (maxBytes-len(hunks)*8)/len(hunks))
	}
	var sb strings.Builder
	for i, h := range hunks {
		rem, add := formatDiffLines("- ", h.oldLines), formatDiffLines("+ ", h.newLines)
		if perCap > 0 && len(rem)+len(add)+1 > perCap {
			if rem != "" && add != "" {
				rem = TruncateUTF8Bytes(rem, max(64, (perCap-8)/2))
				add = TruncateUTF8Bytes(add, perCap-len(rem)-4)
			} else {
				rem, add = TruncateUTF8Bytes(rem, perCap), TruncateUTF8Bytes(add, perCap)
			}
		}
		if i > 0 {
			sb.WriteString("\n...\n")
		}
		sb.WriteString(rem)
		if rem != "" && add != "" {
			sb.WriteByte('\n')
		}
		sb.WriteString(add)
	}
	return TruncateUTF8Bytes(sb.String(), maxBytes)
}
