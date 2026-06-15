package harness

// ---------------------------------------------------------------------------
// The edit tool's matching engine: exact-once string replacement made robust
// by normalization and a deterministic tolerance ladder. Chosen by the
// editbench matrix over the prior naive tool and a batch edits[] variant
// (measured by the editbench matrix): zero failed edit calls on both
// reference models, fewest edit calls and tokens at equal success.
//
// The pipeline: strip BOM and normalize CRLF to LF, match through the ladder
// (exact, then trailing-whitespace-tolerant, then a uniform leading-indent
// offset), require uniqueness AFTER normalization, splice, then restore the
// file's original line endings and BOM. Errors are recovery-grade: zero
// matches come with a nearest-miss hint, multiple matches with the count.
//
// The matching is deterministic at every rung. There is deliberately NO
// edit-distance / fuzzy content matching: a near-but-wrong region must be
// reported, never silently edited. Rejecting and asking is the recoverable
// behavior; guessing is the unrecoverable one.
// ---------------------------------------------------------------------------

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/mike-diff/sesh/agent"
)

// lineEnding captures a file's original encoding so a normalized edit can be
// written back the way the file arrived. CRLF and a leading BOM are invisible
// to the model but break exact matching; we strip them for matching and restore
// them on write.
type lineEnding struct {
	crlf bool // the file used \r\n line endings
	bom  bool // the file began with a UTF-8 BOM
}

// utf8BOM is the UTF-8 byte-order mark, written via its escape so no literal
// BOM byte sits in this source file (Go rejects a BOM mid-file).
const utf8BOM = "\uFEFF"

// normalizeContent strips a leading BOM and converts CRLF to LF, returning the
// normalized text and the encoding needed to restore it. Matching always runs
// against the normalized form so invisible characters cannot cause a miss.
func normalizeContent(s string) (string, lineEnding) {
	var enc lineEnding
	if strings.HasPrefix(s, utf8BOM) {
		enc.bom = true
		s = strings.TrimPrefix(s, utf8BOM)
	}
	if strings.Contains(s, "\r\n") {
		enc.crlf = true
		s = strings.ReplaceAll(s, "\r\n", "\n")
	}
	return s, enc
}

// restoreContent re-applies the original encoding to normalized text: LF back
// to CRLF and the BOM back at the front. A round-trip with no edit must return
// the file byte-for-byte.
func restoreContent(s string, enc lineEnding) string {
	if enc.crlf {
		s = strings.ReplaceAll(s, "\n", "\r\n")
	}
	if enc.bom {
		s = utf8BOM + s
	}
	return s
}

// matchResult is where a found match sits in the normalized text, as a byte
// half-open range [start,end). count is how many distinct (non-overlapping)
// matches the winning ladder rung found; uniqueness needs the count, recovery
// needs the location.
type matchResult struct {
	start, end int
	count      int
}

// trimTrailingPerLine drops trailing spaces and tabs from every line. Used to
// build the trailing-whitespace-tolerant rung: a target line the model quoted
// without its trailing spaces still matches the file's line that has them.
func trimTrailingPerLine(s string) string {
	lines := strings.Split(s, "\n")
	for i, ln := range lines {
		lines[i] = strings.TrimRight(ln, " \t")
	}
	return strings.Join(lines, "\n")
}

// commonLeadingWS returns the longest run of spaces/tabs every non-blank line
// shares as a prefix. It powers the uniform-indent rung: the model quotes a
// block dedented (or over-indented) by a constant amount and we still locate it.
// Blank lines are ignored so they do not force the common prefix to empty.
func commonLeadingWS(lines []string) string {
	prefix := ""
	first := true
	for _, ln := range lines {
		if strings.TrimSpace(ln) == "" {
			continue
		}
		lead := ln[:len(ln)-len(strings.TrimLeft(ln, " \t"))]
		if first {
			prefix = lead
			first = false
			continue
		}
		prefix = commonPrefix(prefix, lead)
		if prefix == "" {
			break
		}
	}
	return prefix
}

// commonPrefix lives in tui.go (rune-based); for whitespace prefixes, which are
// single-byte spaces and tabs, it is equivalent, so we reuse it here.

// stripCommonIndent removes the block's shared leading whitespace from every
// line, so two blocks that differ only by a uniform indent compare equal.
func stripCommonIndent(s string) string {
	lines := strings.Split(s, "\n")
	prefix := commonLeadingWS(lines)
	if prefix == "" {
		return s
	}
	for i, ln := range lines {
		lines[i] = strings.TrimPrefix(ln, prefix)
	}
	return strings.Join(lines, "\n")
}

// findExact reports every non-overlapping occurrence of old in text as a byte
// range, the primitive each ladder rung reuses.
func findExact(text, old string) []matchResult {
	if old == "" {
		return nil
	}
	var hits []matchResult
	for off := 0; ; {
		i := strings.Index(text[off:], old)
		if i < 0 {
			break
		}
		start := off + i
		hits = append(hits, matchResult{start: start, end: start + len(old)})
		off = start + len(old)
	}
	return hits
}

// matchLadder locates old in the normalized text by trying three deterministic
// rungs in order and stopping at the first that finds any match:
//  1. exact substring,
//  2. ignoring trailing whitespace per line,
//  3. ignoring a uniform leading-indent offset across all lines.
//
// It returns the match range in the ORIGINAL (normalized) text plus the count
// at the winning rung. ok is false when no rung matched. There is no fuzzy
// rung: if none of these deterministic transforms aligns the text, we reject.
func matchLadder(text, old string) (matchResult, bool) {
	// Rung 1: exact.
	if hits := findExact(text, old); len(hits) > 0 {
		return matchResult{start: hits[0].start, end: hits[0].end, count: len(hits)}, true
	}
	// Rung 2: trailing-whitespace-tolerant. Compare line-trimmed text to a
	// line-trimmed needle, then map the hit back to original byte offsets.
	if mr, ok := matchTransformed(text, old, trimTrailingPerLine); ok {
		return mr, true
	}
	// Rung 3: uniform leading-indent offset. Strip each side's common indent
	// (and trailing ws, so the two transforms compose) before comparing.
	dedent := func(s string) string { return stripCommonIndent(trimTrailingPerLine(s)) }
	if mr, ok := matchTransformed(text, old, dedent); ok {
		return mr, true
	}
	return matchResult{}, false
}

// matchTransformed finds old inside text after applying transform to both, then
// translates the match boundaries back to byte offsets in the original text. It
// works line-aligned: a match must start at a line boundary, which keeps the
// per-line transforms sound (they only rewrite line interiors, never join
// lines). Returns the original-text range and the count of matches.
func matchTransformed(text, old string, transform func(string) string) (matchResult, bool) {
	tLines := strings.Split(text, "\n")
	needle := transform(old)
	needleLines := strings.Count(needle, "\n") + 1

	// Byte offset where each original line starts, for mapping back.
	offsets := make([]int, len(tLines)+1)
	for i, ln := range tLines {
		offsets[i+1] = offsets[i] + len(ln) + 1 // +1 for the '\n' separator
	}

	var found []matchResult
	for i := 0; i+needleLines <= len(tLines); i++ {
		window := strings.Join(tLines[i:i+needleLines], "\n")
		if transform(window) != needle {
			continue
		}
		start := offsets[i]
		end := offsets[i] + len(window)
		found = append(found, matchResult{start: start, end: end})
	}
	if len(found) == 0 {
		return matchResult{}, false
	}
	return matchResult{start: found[0].start, end: found[0].end, count: len(found)}, true
}

// nearestMiss locates the region of text most word-similar to old and returns a
// few of its lines, so a zero-match error can show the model what is actually
// there. Error quality drives recovery: a bare "not found" strands the model,
// a quoted nearby region lets it correct its needle.
//
// This word-overlap similarity is for the ERROR HINT only; it never selects an
// edit region (matchLadder, which is whitespace-tolerant but never fuzzy, does
// that). Showing a near region is safe; editing one would not be.
func nearestMiss(text, old string) string {
	tLines := strings.Split(text, "\n")
	oldLines := strings.Split(strings.TrimRight(old, "\n"), "\n")
	if len(oldLines) == 0 || len(tLines) == 0 {
		return ""
	}
	// The needle's word vocabulary; a region's score is how many of its words a
	// window's lines collectively contain. A single renamed identifier still
	// leaves most words shared, so the region surfaces.
	want := map[string]bool{}
	for _, ln := range oldLines {
		for _, w := range words(ln) {
			want[w] = true
		}
	}
	if len(want) == 0 {
		return ""
	}
	win := len(oldLines)
	if win < 1 {
		win = 1
	}
	bestScore, bestAt := 0, -1
	for i := 0; i < len(tLines); i++ {
		end := i + win
		if end > len(tLines) {
			end = len(tLines)
		}
		seen := map[string]bool{}
		for _, ln := range tLines[i:end] {
			for _, w := range words(ln) {
				if want[w] {
					seen[w] = true
				}
			}
		}
		if len(seen) > bestScore {
			bestScore, bestAt = len(seen), i
		}
	}
	if bestAt < 0 || bestScore == 0 {
		return ""
	}
	end := bestAt + win
	if end > len(tLines) {
		end = len(tLines)
	}
	const showMax = 6
	shown := tLines[bestAt:end]
	if len(shown) > showMax {
		shown = shown[:showMax]
	}
	return fmt.Sprintf("nearest similar region is around line %d:\n%s", bestAt+1, strings.Join(shown, "\n"))
}

// words splits a line into identifier-like tokens for the nearest-miss scan:
// maximal runs of letters, digits, and underscore. Punctuation and whitespace
// separate tokens, so "func handleRequest() {" yields func, handleRequest.
func words(s string) []string {
	return strings.FieldsFunc(s, func(r rune) bool {
		return !(r == '_' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9'))
	})
}

// ---------------------------------------------------------------------------
// hardened edit: same {path, old, new} schema, normalized matching, recovery
// errors. Returns (result, isErr) like every tool action.
// ---------------------------------------------------------------------------

func doEditHardened(path, old, new string, unsafe bool) (string, bool) {
	if msg := confine(path, unsafe); msg != "" {
		return msg, true
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return err.Error(), true
	}
	text, enc := normalizeContent(string(raw))

	mr, ok := matchLadder(text, old)
	if !ok {
		hint := nearestMiss(text, old)
		msg := "old text not found in " + path + " (tried exact, trailing-whitespace, and indent-tolerant matching); read the file and try again"
		if hint != "" {
			msg += "\n" + hint
		}
		return msg, true
	}
	if mr.count > 1 {
		return fmt.Sprintf("old text matches %d places in %s; include more surrounding text to make it unique", mr.count, path), true
	}

	out := text[:mr.start] + new + text[mr.end:]
	if err := os.WriteFile(path, []byte(restoreContent(out, enc)), 0o644); err != nil {
		return err.Error(), true
	}
	res := "applied 1 edit to " + path
	if d := diffBlock(text, out, tune.DiffLines); d != "" {
		add, del := diffStat(text, out)
		res += fmt.Sprintf(" (+%d -%d)\n%s", add, del, d)
	}
	return res, false
}

// ---------------------------------------------------------------------------
// The tool builder.
// ---------------------------------------------------------------------------

const hardenedEditDesc = "Replace one occurrence of old with new in a file. old must match exactly once; include enough surrounding text to make it unique. Whitespace-tolerant: trailing spaces and a uniform indent offset are matched, and the file's original line endings and BOM are preserved."

// hardenedEditTool is the shipped edit tool: exact-once string replacement
// with normalized, whitespace-tolerant matching. The editbench matrix chose
// it: zero failed edit calls on both reference models.
func hardenedEditTool(unsafePaths bool) agent.Tool {
	return agent.Tool{
		Def: agent.ToolDef{Name: "edit", Description: hardenedEditDesc, Schema: editSchema()},
		Run: func(_ context.Context, raw json.RawMessage) (string, bool) {
			var in toolInput
			if err := json.Unmarshal(raw, &in); err != nil {
				return "invalid tool input: " + err.Error(), true
			}
			return doEditHardened(in.Path, in.Old, in.New, unsafePaths)
		},
		// Mutation is never parallel-safe, matching the shipped edit tool.
		Parallel: false,
	}
}

// editSchema is the edit tool's {path, old, new} schema.
func editSchema() map[string]any {
	str := func(d string) map[string]any { return map[string]any{"type": "string", "description": d} }
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"path": str("Path to the file to edit."),
			"old":  str("The exact text to replace. Must match once (whitespace-tolerant)."),
			"new":  str("The replacement text."),
		},
		"required": []string{"path", "old", "new"},
	}
}
