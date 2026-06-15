package harness

import (
	"strings"
	"testing"
)

// styled builds a color-on renderer with sentinel role codes, so assertions
// read the rendering decisions (which role styled what) independent of the
// active color tier, and a capture sink.
func styled() (*mdRenderer, *strings.Builder) {
	var b strings.Builder
	m := &mdRenderer{
		out:   func(s string) { b.WriteString(s) },
		color: true,
		pal:   palette{heading: "<h>", code: "<c>", muted: "<m>", accent: "<a>"},
	}
	return m, &b
}

// TestMarkdownInlineCode: a backtick span renders in the code style with the
// backticks stripped. Breaker: drop the codeSpanRe handling in spans and the
// literal backticks survive in the output.
func TestMarkdownInlineCode(t *testing.T) {
	m, b := styled()
	m.write("see `foo` now\n")
	got := b.String()
	if !strings.Contains(got, "<c>foo"+ansiReset) {
		t.Fatalf("inline code must use the code style: %q", got)
	}
	if strings.Contains(got, "`") {
		t.Fatalf("backticks must be stripped: %q", got)
	}
}

// TestMarkdownFenceVerbatim: a fenced block drops the ``` delimiter lines and
// emits its contents verbatim in the code style, with NO inline styling inside
// (so code with * or backticks survives). Breaker: route the fenced branch
// through spans and the `*x*` below becomes italic instead of literal.
func TestMarkdownFenceVerbatim(t *testing.T) {
	m, b := styled()
	m.write("```go\n")
	m.write("a := *x*\n")
	m.write("```\n")
	got := b.String()
	if strings.Contains(got, "```") || strings.Contains(got, "go\n") {
		t.Fatalf("fence delimiter lines must be dropped: %q", got)
	}
	if !strings.Contains(got, "<c>a := *x*"+ansiReset) {
		t.Fatalf("code must render verbatim in the code style: %q", got)
	}
}

// TestMarkdownNewlineGated: a line is held until its newline arrives, so a
// partial delta emits nothing. Breaker: emit deltas immediately instead of
// buffering to the newline and the first write produces output.
func TestMarkdownNewlineGated(t *testing.T) {
	m, b := styled()
	m.write("hel")
	if b.Len() != 0 {
		t.Fatalf("a partial line must not be emitted yet: %q", b.String())
	}
	m.write("lo\n")
	if got := b.String(); !strings.Contains(got, "hello") {
		t.Fatalf("the completed line must emit on its newline: %q", got)
	}
}

// TestMarkdownNoColorPassthrough: with color off the renderer forwards bytes
// untouched and flush adds nothing, so a pipe or -p gets the model's markdown
// exactly. Breaker: style or strip markers in the color-off path and the bytes
// differ.
func TestMarkdownNoColorPassthrough(t *testing.T) {
	var b strings.Builder
	m := &mdRenderer{out: func(s string) { b.WriteString(s) }} // color false
	in := []string{"a `b` ", "**c**\n# h\n"}
	for _, s := range in {
		m.write(s)
	}
	if m.flush() {
		t.Fatal("flush must report nothing emitted when color is off")
	}
	if got, want := b.String(), strings.Join(in, ""); got != want {
		t.Fatalf("passthrough must be byte-for-byte: got %q want %q", got, want)
	}
}

// TestMarkdownHeaderAndList: a header drops its # and a list marker normalizes
// to a bullet, both styled. Breaker: remove the header/list branches and the
// raw "# " / "- " markers reach the screen.
func TestMarkdownHeaderAndList(t *testing.T) {
	m, b := styled()
	m.write("# Title\n- item\n")
	got := b.String()
	if !strings.Contains(got, "<h>Title"+ansiReset) {
		t.Fatalf("header must be styled with the # stripped: %q", got)
	}
	if !strings.Contains(got, "<a>•"+ansiReset+" item") || strings.Contains(got, "- item") {
		t.Fatalf("list marker must normalize to a styled bullet: %q", got)
	}
}

// TestMarkdownFlushTrailingLine: a message ending without a newline still
// reaches the screen, and flush reports it so the caller adds the separator.
// Breaker: make flush a no-op on the buffered partial and the last line is
// lost.
func TestMarkdownFlushTrailingLine(t *testing.T) {
	m, b := styled()
	m.write("done")
	if b.Len() != 0 {
		t.Fatalf("the trailing line must wait for flush: %q", b.String())
	}
	if !m.flush() {
		t.Fatal("flush must report it emitted the buffered line")
	}
	if got := b.String(); !strings.Contains(got, "done") || strings.HasSuffix(got, "\n") {
		t.Fatalf("flush emits the partial line without adding a newline: %q", got)
	}
}

// TestResolveColorTiers: a hex role renders as 24-bit at the truecolor tier and
// degrades to the matching 256-color cube index otherwise. Pins the cube
// formula (pure red -> 196) and that the tier actually switches the encoding.
// Breaker: ignore tier in resolveColor and the 256 case emits a 38;2 sequence.
func TestResolveColorTiers(t *testing.T) {
	if got, want := resolveColor("#ff0000", tierTrue), "\033[38;2;255;0;0m"; got != want {
		t.Fatalf("truecolor: got %q want %q", got, want)
	}
	if got, want := resolveColor("#ff0000", tier256), "\033[38;5;196m"; got != want {
		t.Fatalf("256 degrade: got %q want %q", got, want)
	}
	if resolveColor("not-a-color", tierTrue) != "" {
		t.Fatal("an unparseable theme value must yield no color, not a broken sequence")
	}
}

// TestColorAllowedRespectsNoColor: NO_COLOR (present and non-empty) disables
// color even on a terminal; an empty value does not. Breaker: drop the
// NO_COLOR check and a set value no longer suppresses color.
func TestColorAllowedRespectsNoColor(t *testing.T) {
	t.Setenv("TERM", "xterm-256color") // not "dumb", so only NO_COLOR decides
	t.Setenv("NO_COLOR", "1")
	if colorAllowed(true) {
		t.Fatal("NO_COLOR=1 must disable color on a terminal")
	}
	t.Setenv("NO_COLOR", "")
	if !colorAllowed(true) {
		t.Fatal("empty NO_COLOR must not disable color")
	}
	if colorAllowed(false) {
		t.Fatal("a non-terminal destination must never get color")
	}
}
