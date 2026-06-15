// The diff block: mutations explain themselves. With approval prompts gone,
// oversight is watching the transcript, and "applied 1 edit" is not watchable.
// Edit and write results therefore carry a minimal line diff of what actually
// changed, which the renderer styles and the model can use to verify its own
// work. The diff_lines dial caps it (-1 disables).
package harness

import (
	"fmt"
	"strings"
)

// trimDiff splits before and after into lines and trims the common prefix and
// suffix. The changed region is then a[pre:len(a)-suf] (removed) and
// b[pre:len(b)-suf] (added). diffBlock and diffStat share it so the rendered
// diff and its reported magnitude always describe the same change.
func trimDiff(before, after string) (a, b []string, pre, suf int) {
	a = strings.Split(before, "\n")
	b = strings.Split(after, "\n")
	for pre < len(a) && pre < len(b) && a[pre] == b[pre] {
		pre++
	}
	for suf < len(a)-pre && suf < len(b)-pre && a[len(a)-1-suf] == b[len(b)-1-suf] {
		suf++
	}
	return a, b, pre, suf
}

// diffStat reports how many lines the change added and removed: the true
// magnitude, counted before any truncation, so the summary line stays honest
// even when diffBlock elides the rendered body.
func diffStat(before, after string) (added, removed int) {
	if before == after {
		return 0, 0
	}
	a, b, pre, suf := trimDiff(before, after)
	return len(b) - pre - suf, len(a) - pre - suf
}

// diffBlock renders the line-level change from before to after: common prefix
// and suffix lines are trimmed, and the changed middle is emitted as "- " and
// "+ " lines with one line of unchanged context on each side. Returns "" when
// nothing changed or limit says no diff. The trim-based shape is deliberate:
// deterministic, linear, and dependency-free; a scattered multi-hunk rewrite
// shows as one block spanning the changed region, which is the honest summary
// of a change that big.
func diffBlock(before, after string, limit int) string {
	if limit <= 0 || before == after {
		return ""
	}
	a, b, pre, suf := trimDiff(before, after)

	var lines []string
	if pre > 0 {
		lines = append(lines, "  "+a[pre-1])
	}
	for _, l := range a[pre : len(a)-suf] {
		lines = append(lines, "- "+l)
	}
	for _, l := range b[pre : len(b)-suf] {
		lines = append(lines, "+ "+l)
	}
	if suf > 0 {
		lines = append(lines, "  "+a[len(a)-suf])
	}
	return strings.Join(elide(lines, limit), "\n")
}

// elide caps the diff at limit lines by dropping the middle, not the tail, so a
// large change still shows where it starts (the removed lines) and what it
// became (the added lines) instead of only the leading removals. The dropped
// count is explicit.
func elide(lines []string, limit int) []string {
	if len(lines) <= limit {
		return lines
	}
	head := limit / 2
	tail := limit - head
	out := append([]string{}, lines[:head]...)
	out = append(out, fmt.Sprintf("  ... (%d more diff lines) ...", len(lines)-limit))
	return append(out, lines[len(lines)-tail:]...)
}
