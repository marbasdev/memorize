package domain

import (
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
