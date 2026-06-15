package harness

import (
	"bufio"
	"os"
	"strings"
	"testing"
)

// driveKeys runs the real readLine keystroke loop headless: keys is the exact
// rune stream a user types (\t = Tab, \r = Enter to submit). It returns the
// submitted line and everything drawn to the terminal (captured through a temp
// file, since the console writes to an *os.File), so a test can assert on the
// editor's actual behavior, not just its helpers.
func driveKeys(t *testing.T, m *mentions, keys string) (line, drawn string) {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "tui-out")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	tc := &tuiConsole{
		out:     f,
		in:      bufio.NewReader(strings.NewReader(keys)),
		cols:    80,
		mention: m,
	}
	line, _ = tc.readLine("-> ", false)
	f.Sync()
	b, _ := os.ReadFile(f.Name())
	return line, string(b)
}

func mentionsFor(t *testing.T, tree, skills []string) *mentions {
	set := map[string]bool{}
	for _, s := range skills {
		set[s] = true
	}
	return &mentions{root: mkTree(t, tree...), skills: set}
}

// TestEditorMentionsEndToEnd drives the editor as the user, asserting the line
// that would be submitted after a stream of keystrokes. Each row exercises a
// distinct path through the readLine loop (Tab completion in place, space
// normalization, multi-mention, the no-op fallbacks).
func TestEditorMentionsEndToEnd(t *testing.T) {
	cases := []struct {
		name   string
		tree   []string
		skills []string
		keys   string
		want   string
	}{
		{"skill Tab completes a unique name", nil, []string{"frontend-design"}, "#fr\t\r", "#frontend-design"},
		{"skill Tab extends to the common prefix", nil, []string{"frontend-design", "frontend-docs"}, "#fr\t\r", "#frontend-d"},
		{"skill is not expanded on space", nil, []string{"frontend-design"}, "#fr \r", "#fr"},
		{"skill Tab with no match is a no-op", nil, []string{"frontend-design"}, "#zzz\t\r", "#zzz"},
		{"file Tab completes a single file", []string{"a.go"}, nil, "@a\t\r", "@a.go"},
		{"file Tab drills into a directory", []string{"sub/b.go"}, nil, "@su\t\t\r", "@sub/b.go"},
		{"space normalizes a bare name to its relpath", []string{"harness/steering.go"}, nil, "@steering.go \r", "@harness/steering.go"},
		{"space keeps an already-valid path", []string{"a.go"}, nil, "@a.go \r", "@a.go"},
		{"space leaves an ambiguous name as typed", []string{"a/dup.go", "b/dup.go"}, nil, "@dup.go \r", "@dup.go"},
		{"space leaves an unknown name as typed", nil, nil, "@nope.go \r", "@nope.go"},
		{"two mentions complete in one line", []string{"sub/b.go"}, []string{"frontend-design"}, "use #fr\t and @su\t\t\r", "use #frontend-design and @sub/b.go"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m := mentionsFor(t, c.tree, c.skills)
			if line, _ := driveKeys(t, m, c.keys); line != c.want {
				t.Fatalf("keys %q -> %q, want %q", c.keys, line, c.want)
			}
		})
	}
}

// TestEditorMentionHighlightDraw: a recognized mention is colored in the drawn
// input row as the user types, and an unresolved one is not. This exercises the
// highlight through the real draw path, not just segments() in isolation.
// Breaker: drop the highlight branch in segments and the recognized case loses
// its color.
func TestEditorMentionHighlightDraw(t *testing.T) {
	const sgr = "\033[35m"
	m := &mentions{root: mkTree(t, "a.go"), sgr: sgr}

	if _, drawn := driveKeys(t, m, "@a.go"); !strings.Contains(drawn, sgr) {
		t.Fatal("a recognized @file must be highlighted in the drawn input row")
	}
	if _, drawn := driveKeys(t, m, "@nope.go"); strings.Contains(drawn, sgr) {
		t.Fatal("an unresolved @file must not be highlighted")
	}
}

// TestEditorMentionsDisabledInSecret: a masked (password) prompt does no
// completion, normalization, or highlighting, so a secret never triggers the
// filesystem or leaks a path. Breaker: drop the !mask guards on Tab/space.
func TestEditorMentionsDisabledInSecret(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "out")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	tc := &tuiConsole{
		out:     f,
		in:      bufio.NewReader(strings.NewReader("@a.go\t \r")),
		cols:    80,
		mention: &mentions{root: mkTree(t, "a.go"), sgr: "\033[35m"},
	}
	line, _ := tc.readLine("pw> ", true) // masked
	if line != "@a.go" {
		t.Fatalf("masked input must not complete or normalize: %q", line)
	}
	f.Sync()
	if b, _ := os.ReadFile(f.Name()); strings.Contains(string(b), "\033[35m") {
		t.Fatal("masked input must not highlight mentions")
	}
}

// TestEditorWithoutMentions: with the feature off (nil), Tab and space behave
// normally and nothing panics. Breaker: drop a nil guard in completeLocked or
// normalizeLocked and this panics.
func TestEditorWithoutMentions(t *testing.T) {
	if line, _ := driveKeys(t, nil, "@a.go\t and #x \r"); line != "@a.go and #x" {
		t.Fatalf("inert editor changed the text: %q", line)
	}
}
