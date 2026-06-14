package harness

import (
	"fmt"
	"strings"

	"github.com/mike-diff/sesh/agent"
)

// ---------------------------------------------------------------------------
// Rendering: hooks turn core events into terminal output. A different set of
// hooks (or none) is all that separates interactive from print/JSON modes.
// ---------------------------------------------------------------------------

// renderHooks streams text, dims reasoning (when showThink allows it), and
// shows tool I/O.
func renderHooks(g func(agent.ToolCall) error, showThink *bool) agent.Hooks {
	thinking := false
	flushThinking := func() {
		if thinking {
			thinking = false
			emit("\n")
		}
	}
	return agent.Hooks{
		OnText: func(s string) { flushThinking(); emit("%s", s) },
		OnThink: func(s string) {
			if showThink != nil && !*showThink {
				return
			}
			thinking = true
			emit("%s", dim+s+reset)
		},
		OnToolStart: func(c agent.ToolCall) {
			flushThinking()
			emit("%s  > %s %s%s\n", dim, c.Name, compact(string(c.Args)), reset)
		},
		OnToolEnd: func(c agent.ToolCall, r agent.ToolResult) {
			size := ""
			if len(r.Content) > 1024 {
				size = fmt.Sprintf(" [%d bytes]", len(r.Content))
			}
			emit("%s  < %s%s%s\n", dim, compact(firstLine(r.Content)), size, reset)
			// Mutations explain themselves: edit/write results carry a diff
			// block after the summary line, and watching it IS the oversight.
			// Keyed by tool name so file contents in other results never get
			// mistaken for diff lines.
			if c.Name == "edit" || c.Name == "write" {
				emitDiffLines(r.Content)
			}
		},
		Gate: g,
	}
}

// emitDiffLines styles the diff block an edit/write result carries: removals
// red, additions green, context dim. Display-only; the model sees plain text.
func emitDiffLines(content string) {
	rest := ""
	if i := strings.IndexByte(content, '\n'); i >= 0 {
		rest = content[i+1:]
	}
	if rest == "" {
		return
	}
	for _, line := range strings.Split(rest, "\n") {
		switch {
		case strings.HasPrefix(line, "- "):
			emit("%s    %s%s\n", red, line, reset)
		case strings.HasPrefix(line, "+ "):
			emit("%s    %s%s\n", green, line, reset)
		default:
			emit("%s    %s%s\n", dim, line, reset)
		}
	}
}

func compact(s string) string {
	if len(s) > 120 {
		return s[:120] + "..."
	}
	return s
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
