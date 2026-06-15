package harness

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// mkTree builds a temp working tree from the given relative paths and returns
// its root, so mention resolution can be tested without touching the real cwd.
func mkTree(t *testing.T, files ...string) string {
	t.Helper()
	root := t.TempDir()
	for _, f := range files {
		p := filepath.Join(root, f)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

// TestMentionToken: the token ending at the cursor is returned only when it
// starts with a mention sigil. Breaker: drop the sigil check and plain words
// are treated as mentions.
func TestMentionToken(t *testing.T) {
	buf := []rune("see @foo")
	if start, tok, ok := mentionToken(buf, len(buf)); !ok || start != 4 || tok != "@foo" {
		t.Fatalf("got start=%d tok=%q ok=%v, want 4 @foo true", start, tok, ok)
	}
	if _, _, ok := mentionToken([]rune("see foo"), 7); ok {
		t.Fatal("a plain word must not be a mention token")
	}
	if _, _, ok := mentionToken([]rune("x"), 0); ok {
		t.Fatal("pos 0 has no token")
	}
	if _, tok, ok := mentionToken([]rune("a #sk"), 5); !ok || tok != "#sk" {
		t.Fatalf("skill token not found: %q %v", tok, ok)
	}
}

// TestMentionCompleteSkills: "#" completes installed skill names by prefix,
// sorted. Breaker: ignore the prefix and unrelated skills are offered.
func TestMentionCompleteSkills(t *testing.T) {
	m := &mentions{skills: map[string]bool{"frontend-design": true, "report-issue": true}}
	if got := m.complete("#fr"); len(got) != 1 || got[0] != "#frontend-design" {
		t.Fatalf("#fr -> %v, want [#frontend-design]", got)
	}
	if got := m.complete("#"); len(got) != 2 || got[0] != "#frontend-design" || got[1] != "#report-issue" {
		t.Fatalf("# -> %v, want both sorted", got)
	}
	if got := m.complete("#zzz"); got != nil {
		t.Fatalf("no match must be nil, got %v", got)
	}
}

// TestMentionCompletePath: "@" completes working-tree entries by prefix, with a
// trailing slash on directories so completion can drill in. Breaker: drop the
// directory slash and "@sub/" never opens the directory's contents.
func TestMentionCompletePath(t *testing.T) {
	m := &mentions{root: mkTree(t, "a.go", "sub/b.go")}
	if got := m.complete("@"); len(got) != 2 || got[0] != "@a.go" || got[1] != "@sub/" {
		t.Fatalf("@ -> %v, want [@a.go @sub/]", got)
	}
	if got := m.complete("@sub/"); len(got) != 1 || got[0] != "@sub/b.go" {
		t.Fatalf("@sub/ -> %v, want [@sub/b.go]", got)
	}
}

// TestMentionResolve: an existing path is kept, a bare name that uniquely
// matches one tree file is rewritten to its relative path, and an ambiguous or
// unknown name is left exactly as typed. Breaker: return the first match instead
// of requiring uniqueness and the ambiguous case is silently rewritten.
func TestMentionResolve(t *testing.T) {
	m := &mentions{root: mkTree(t, "x.go", "harness/steering.go", "a/dup.go", "b/dup.go")}
	if rep, ok := m.resolve("@x.go"); !ok || rep != "@x.go" {
		t.Fatalf("existing path: %q %v", rep, ok)
	}
	if rep, ok := m.resolve("@steering.go"); !ok || rep != "@harness/steering.go" {
		t.Fatalf("unique basename must rewrite to its path: %q %v", rep, ok)
	}
	if rep, ok := m.resolve("@dup.go"); ok || rep != "@dup.go" {
		t.Fatalf("ambiguous name must be left as typed: %q %v", rep, ok)
	}
	if rep, ok := m.resolve("@nope.go"); ok || rep != "@nope.go" {
		t.Fatalf("unknown name must be left as typed: %q %v", rep, ok)
	}
}

// TestMentionOK: recognition is true only for installed skills and existing
// paths, which is what gates highlighting. Breaker: always return true and
// unrecognized tokens light up.
func TestMentionOK(t *testing.T) {
	m := &mentions{root: mkTree(t, "x.go"), skills: map[string]bool{"frontend-design": true}}
	for _, tok := range []string{"#frontend-design", "@x.go"} {
		if !m.ok(tok) {
			t.Fatalf("%q must be recognized", tok)
		}
	}
	for _, tok := range []string{"#nope", "@missing.go", "plain"} {
		if m.ok(tok) {
			t.Fatalf("%q must not be recognized", tok)
		}
	}
}

// TestMentionSpans: only recognized mentions get a highlight span. Breaker: drop
// the ok() check and unrecognized #/@ tokens are spanned too.
func TestMentionSpans(t *testing.T) {
	m := &mentions{root: mkTree(t, "x.go"), skills: map[string]bool{"fd": true}}
	buf := []rune("use @x.go and #fd plus @nope and #bad")
	got := m.spans(buf)
	if len(got) != 2 {
		t.Fatalf("want 2 recognized spans, got %d: %v", len(got), got)
	}
	if s := string(buf[got[0][0]:got[0][1]]); s != "@x.go" {
		t.Fatalf("first span = %q, want @x.go", s)
	}
	if s := string(buf[got[1][0]:got[1][1]]); s != "#fd" {
		t.Fatalf("second span = %q, want #fd", s)
	}
}

// TestMentionPathConfinement: completion and resolution stay inside the working
// directory; an escaping path resolves to nothing. Breaker: drop the ".."/abs
// guard in existsRel and an outside path resolves.
func TestMentionPathConfinement(t *testing.T) {
	m := &mentions{root: mkTree(t, "x.go")}
	if _, ok := m.existsRel("../x.go"); ok {
		t.Fatal("a parent-escaping path must not resolve")
	}
	if got := m.complete("@../"); got != nil {
		t.Fatalf("completion must not escape the root, got %v", got)
	}
}

// TestMentionHighlightInSegments: a recognized mention's runes are wrapped in
// the highlight color while surrounding text stays plain. Breaker: drop the
// highlight branch in segments and the mention is rendered like any other text.
func TestMentionHighlightInSegments(t *testing.T) {
	tc := &tuiConsole{
		buf:     []rune("see @x.go ok"),
		mention: &mentions{root: mkTree(t, "x.go"), sgr: "<S>"},
	}
	segs := tc.segments()
	if joined := strings.Join(segs, ""); !strings.Contains(joined, "<S>@"+ansiReset) {
		t.Fatalf("recognized mention must be highlighted: %q", joined)
	}
	if strings.Contains(segs[0], "<S>") {
		t.Fatalf("non-mention text must stay plain: %q", segs[0])
	}
}
