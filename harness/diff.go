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
	a := strings.Split(before, "\n")
	b := strings.Split(after, "\n")

	// Trim the common prefix and suffix.
	pre := 0
	for pre < len(a) && pre < len(b) && a[pre] == b[pre] {
		pre++
	}
	suf := 0
	for suf < len(a)-pre && suf < len(b)-pre && a[len(a)-1-suf] == b[len(b)-1-suf] {
		suf++
	}

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
	if len(lines) > limit {
		dropped := len(lines) - limit
		lines = append(lines[:limit], fmt.Sprintf("  ... (%d more diff lines)", dropped))
	}
	return strings.Join(lines, "\n")
}
