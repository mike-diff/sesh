// Terminal markdown rendering for the model's streamed output. The core layer
// is built in; the colors are a theme a user can override by dropping
// theme.json into ~/.sesh (global) or .sesh (project), same mount-point pattern
// as the statusline. Stdlib only: ANSI is just bytes, so no rendering or
// highlighting library is pulled in. The deliberate fidelity concession versus
// a full library is grammar-aware syntax highlighting; all code reads in one
// uniform style, copy-paste clean (color is stripped on copy; gutters are not,
// so there are none).
package harness

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

//go:embed theme.json
var defaultThemeJSON []byte

const ansiReset = "\033[0m"

// Color tiers. 256 is the floor whenever color is on at all: it is the
// near-universal baseline, and detecting genuine 16-color-only terminals
// reliably is not worth downgrading the overwhelming majority that support 256.
const (
	tier256 = iota
	tierTrue
)

// useColor reports whether to emit ANSI to stdout at all.
func useColor() bool { return colorAllowed(isTerminal(os.Stdout)) }

// colorAllowed is the policy behind useColor, split out so it is testable
// without a real terminal: a terminal destination gets color unless NO_COLOR
// (present and non-empty) or a dumb terminal opts out.
func colorAllowed(isTTY bool) bool {
	if v, ok := os.LookupEnv("NO_COLOR"); ok && v != "" {
		return false
	}
	if os.Getenv("TERM") == "dumb" {
		return false
	}
	return isTTY
}

// colorTier picks the richest color depth the terminal advertises.
func colorTier() int {
	if ct := os.Getenv("COLORTERM"); ct == "truecolor" || ct == "24bit" {
		return tierTrue
	}
	return tier256
}

// palette holds the resolved opening SGR sequence for each themed role;
// ansiReset closes any of them.
type palette struct{ heading, code, muted, accent string }

func themeFiles() []string {
	return []string{
		filepath.Join(os.Getenv("HOME"), ".sesh", "theme.json"), // global
		".sesh/theme.json", // project overrides per role
	}
}

// loadPalette starts from the embedded default theme and lets the global then
// project theme.json override individual roles, then resolves each to the
// active color tier.
func loadPalette(tier int) palette {
	roles := map[string]string{}
	json.Unmarshal(defaultThemeJSON, &roles)
	for _, f := range themeFiles() {
		b, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		over := map[string]string{}
		if json.Unmarshal(b, &over) == nil {
			for k, v := range over {
				roles[k] = v
			}
		}
	}
	col := func(role string) string { return resolveColor(roles[role], tier) }
	return palette{
		heading: "\033[1m" + col("heading"), // headings are weight + color, never a bigger glyph
		code:    col("code"),
		muted:   col("muted"),
		accent:  col("accent"),
	}
}

// resolveColor turns a "#rrggbb" theme value into a foreground SGR sequence at
// the given tier, degrading 24-bit color to the 256-color cube. An unparseable
// value yields no color rather than an error: a typo'd theme stays readable.
func resolveColor(hex string, tier int) string {
	r, g, b, ok := parseHex(hex)
	if !ok {
		return ""
	}
	if tier == tierTrue {
		return fmt.Sprintf("\033[38;2;%d;%d;%dm", r, g, b)
	}
	return fmt.Sprintf("\033[38;5;%dm", to256(r, g, b))
}

func parseHex(s string) (r, g, b int, ok bool) {
	s = strings.TrimPrefix(strings.TrimSpace(s), "#")
	if len(s) != 6 {
		return 0, 0, 0, false
	}
	v, err := strconv.ParseUint(s, 16, 32)
	if err != nil {
		return 0, 0, 0, false
	}
	return int(v >> 16 & 0xff), int(v >> 8 & 0xff), int(v & 0xff), true
}

// to256 maps an RGB triple onto the xterm 6x6x6 color cube (indices 16-231).
func to256(r, g, b int) int {
	q := func(v int) int {
		switch {
		case v < 48:
			return 0
		case v < 115:
			return 1
		default:
			return (v - 35) / 40
		}
	}
	return 16 + 36*q(r) + 6*q(g) + q(b)
}

var (
	fenceRe    = regexp.MustCompile("^\\s*(`{3,}|~{3,})")
	hrRe       = regexp.MustCompile(`^\s*(-{3,}|\*{3,}|_{3,})\s*$`)
	headerRe   = regexp.MustCompile(`^(#{1,6})\s+(.*)$`)
	listRe     = regexp.MustCompile(`^(\s*)([-*+]|\d+\.)\s+(.*)$`)
	quoteRe    = regexp.MustCompile(`^\s*>\s?(.*)$`)
	codeSpanRe = regexp.MustCompile("`([^`]+)`")
	boldRe     = regexp.MustCompile(`\*\*([^*]+)\*\*`)
	italRe     = regexp.MustCompile(`\*([^*]+)\*`)
)

// mdRenderer styles the model's streamed markdown for the terminal. It is
// newline-gated: a line reaches the screen only once complete, so inline spans
// (code, bold, italic) can be styled and code fences buffered whole, without
// the flicker of restyling text already drawn. Color off makes write a
// byte-for-byte passthrough, which is what pipes and -p want.
type mdRenderer struct {
	out   func(string)
	color bool
	pal   palette
	line  []byte // the current partial line, held until its newline arrives
	fence bool   // inside a ``` / ~~~ block: emit verbatim, no inline styling
}

// newMarkdown builds a renderer writing to out (the transcript sink). It styles
// only when color is enabled; otherwise it forwards bytes untouched.
func newMarkdown(out func(string)) *mdRenderer {
	m := &mdRenderer{out: out}
	if useColor() {
		m.color = true
		m.pal = loadPalette(colorTier())
	}
	return m
}

// write feeds streamed deltas in and emits completed lines styled. Bytes after
// the last newline stay buffered for the next call or flush.
func (m *mdRenderer) write(s string) {
	if m == nil {
		return
	}
	if !m.color {
		m.out(s)
		return
	}
	m.line = append(m.line, s...)
	for {
		i := bytes.IndexByte(m.line, '\n')
		if i < 0 {
			break
		}
		m.emitLine(string(m.line[:i]), true)
		m.line = append(m.line[:0], m.line[i+1:]...)
	}
}

// flush emits any buffered partial line (a message rarely ends on a newline)
// and resets block state for the next message. It reports whether it emitted
// anything, so the caller can add the separating newline only when needed.
func (m *mdRenderer) flush() bool {
	if m == nil || !m.color {
		return false
	}
	emitted := false
	if len(m.line) > 0 {
		m.emitLine(string(m.line), false)
		m.line = m.line[:0]
		emitted = true
	}
	m.fence = false
	return emitted
}

// emitLine renders one complete line. nl re-adds the newline write consumed;
// the trailing partial line from flush passes nl=false to preserve the text
// exactly as the model produced it.
func (m *mdRenderer) emitLine(line string, nl bool) {
	var out string
	switch {
	case m.fence:
		if fenceRe.MatchString(line) {
			m.fence = false
			return // closing fence: drop the delimiter line
		}
		out = m.pal.code + line + ansiReset // verbatim, so copied code stays intact
	case fenceRe.MatchString(line):
		m.fence = true
		return // opening fence: drop the delimiter line
	case hrRe.MatchString(line):
		out = m.pal.muted + line + ansiReset
	case headerRe.MatchString(line):
		out = m.pal.heading + headerRe.FindStringSubmatch(line)[2] + ansiReset
	case listRe.MatchString(line):
		g := listRe.FindStringSubmatch(line)
		bullet := g[2]
		if bullet == "-" || bullet == "*" || bullet == "+" {
			bullet = "•"
		}
		out = g[1] + m.pal.accent + bullet + ansiReset + " " + m.spans(g[3])
	case quoteRe.MatchString(line):
		out = m.pal.muted + "│ " + quoteRe.FindStringSubmatch(line)[1] + ansiReset
	default:
		out = m.spans(line)
	}
	if nl {
		out += "\n"
	}
	m.out(out)
}

// spans styles inline code, bold, and italic within a line. Code is handled
// first and lifted out so its contents are never re-read as emphasis (a `*` or
// snake_case inside a code span stays literal). Underscore emphasis is left
// alone on purpose, so snake_case identifiers in prose are not mangled.
func (m *mdRenderer) spans(s string) string {
	var saved []string
	s = codeSpanRe.ReplaceAllStringFunc(s, func(g string) string {
		saved = append(saved, m.pal.code+g[1:len(g)-1]+ansiReset)
		return "\x00" + strconv.Itoa(len(saved)-1) + "\x00"
	})
	s = boldRe.ReplaceAllString(s, "\033[1m$1"+ansiReset)
	s = italRe.ReplaceAllString(s, "\033[3m$1"+ansiReset)
	for i, v := range saved {
		s = strings.Replace(s, "\x00"+strconv.Itoa(i)+"\x00", v, 1)
	}
	return s
}
