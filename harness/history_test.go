package harness

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestHistoryRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "h.jsonl")

	var hist []string
	hist = appendHistory(path, hist, "first")
	hist = appendHistory(path, hist, "first") // immediate repeat: skipped
	hist = appendHistory(path, hist, "")      // empty: skipped
	hist = appendHistory(path, hist, "exit")  // quit commands and aliases: skipped
	hist = appendHistory(path, hist, "quit")
	hist = appendHistory(path, hist, "/exit")
	hist = appendHistory(path, hist, "/quit")
	hist = appendHistory(path, hist, "multi\nline paste")
	if len(hist) != 2 {
		t.Fatalf("in-memory history: %v", hist)
	}

	got := loadHistory(path)
	if len(got) != 2 || got[0] != "first" || got[1] != "multi\nline paste" {
		t.Fatalf("loaded: %v", got)
	}

	// the file must be private: typed input can be sensitive
	if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o600 {
		t.Fatalf("history perms: %v", fi.Mode().Perm())
	}
}

func TestHistoryTrim(t *testing.T) {
	path := filepath.Join(t.TempDir(), "h.jsonl")
	var hist []string
	for i := 0; i < historyKeep+150; i++ {
		hist = appendHistory(path, hist, "entry-"+strings.Repeat("x", i%7)+string(rune('a'+i%26))+itoa(i))
	}
	got := loadHistory(path)
	if len(got) != historyKeep {
		t.Fatalf("trim: %d entries, want %d", len(got), historyKeep)
	}
	// trimmed file persists the cap
	if again := loadHistory(path); len(again) != historyKeep {
		t.Fatalf("rewrite: %d entries", len(again))
	}
}

func itoa(i int) string {
	return strings.TrimSpace(strings.ReplaceAll(string(rune('0'+i/100%10))+string(rune('0'+i/10%10))+string(rune('0'+i%10)), " ", ""))
}

func TestHistMove(t *testing.T) {
	tc := &tuiConsole{hist: []string{"one", "two"}, buf: []rune("draft"), histIdx: 2}

	tc.histMoveLocked(-1) // up: latest entry, draft stashed
	if string(tc.buf) != "two" || tc.pos != 3 {
		t.Fatalf("up: %q pos=%d", string(tc.buf), tc.pos)
	}
	tc.histMoveLocked(-1)
	if string(tc.buf) != "one" {
		t.Fatalf("up up: %q", string(tc.buf))
	}
	tc.histMoveLocked(-1) // past the top: no change
	if string(tc.buf) != "one" {
		t.Fatalf("top clamp: %q", string(tc.buf))
	}
	tc.histMoveLocked(+1)
	tc.histMoveLocked(+1) // back down to the draft
	if string(tc.buf) != "draft" {
		t.Fatalf("draft restore: %q", string(tc.buf))
	}
}

func TestCommonPrefix(t *testing.T) {
	if got := commonPrefix("/provider add", "/provider apple"); got != "/provider a" {
		t.Fatalf("lcp: %q", got)
	}
	if got := commonPrefix("abc", "abc"); got != "abc" {
		t.Fatalf("equal: %q", got)
	}
	if got := commonPrefix("x", "y"); got != "" {
		t.Fatalf("disjoint: %q", got)
	}
}

func TestReplCompletions(t *testing.T) {
	r := &repl{
		models: []string{"alpha:9b", "alpha:4b", "beta-1"},
		pcfg: ProvidersConfig{Providers: map[string]Profile{
			"cloud": {}, "local": {},
		}},
	}
	if got := r.completions("/mo"); len(got) != 1 || got[0] != "/model " {
		t.Fatalf("command: %v", got)
	}
	if got := r.completions("/model alpha"); len(got) != 2 {
		t.Fatalf("models: %v", got)
	}
	if got := r.completions("/provider c"); len(got) != 1 || got[0] != "/provider cloud" {
		t.Fatalf("providers: %v", got)
	}
	if got := r.completions("/provider "); len(got) != 4 { // add, remove, ollama, zai
		t.Fatalf("provider all: %v", got)
	}
	if got := r.completions("hello"); got != nil {
		t.Fatalf("chat text must not complete: %v", got)
	}
	if got := r.completions("/ex"); len(got) != 1 || got[0] != "/exit" {
		t.Fatalf("/exit must complete: %v", got)
	}
}

func TestSegments(t *testing.T) {
	tc := &tuiConsole{buf: []rune("a\nb")}
	// A newline renders as a real break (layout starts a new row; the submit
	// echo shows the message on multiple lines), not an inline glyph.
	if got := strings.Join(tc.segments(), ""); got != "a\nb" {
		t.Fatalf("newline display: %q", got)
	}
	tc.mask = true
	if got := strings.Join(tc.segments(), ""); got != "***" {
		t.Fatalf("mask: %q", got)
	}

	// a snippet token renders as its label and expands on submit
	tc = &tuiConsole{snippets: []string{"line1\nline2\nline3\nline4"}}
	tc.buf = []rune{'x', snippetBase, 'y'}
	if got := strings.Join(tc.segments(), ""); got != "x[snippet #1: 4 lines]y" {
		t.Fatalf("snippet label: %q", got)
	}
	if got := tc.expandSnippets(); got != "xline1\nline2\nline3\nline4y" {
		t.Fatalf("snippet expansion: %q", got)
	}
}

func TestInsertPasteThresholds(t *testing.T) {
	tc := &tuiConsole{out: os.NewFile(0, ""), cols: 80}
	// small paste: inline
	tc.insertPasteLocked([]rune("one\ntwo"))
	if len(tc.snippets) != 0 || string(tc.buf) != "one\ntwo" {
		t.Fatalf("small paste should inline: snippets=%d buf=%q", len(tc.snippets), string(tc.buf))
	}
	// big paste: collapses to one atomic token
	tc2 := &tuiConsole{out: os.NewFile(0, ""), cols: 80}
	big := "l1\nl2\nl3\nl4\nl5"
	tc2.insertPasteLocked([]rune(big))
	if len(tc2.snippets) != 1 || len(tc2.buf) != 1 || tc2.buf[0] != snippetBase {
		t.Fatalf("big paste should collapse: snippets=%d buf=%v", len(tc2.snippets), tc2.buf)
	}
	// backspace deletes the whole snippet in one keystroke (it is one rune)
	tc2.pos = 1
	tc2.buf = append(tc2.buf[:tc2.pos-1], tc2.buf[tc2.pos:]...)
	if len(tc2.buf) != 0 {
		t.Fatal("snippet should delete atomically")
	}
}

func TestRuneWidth(t *testing.T) {
	if runeWidth('a') != 1 || runeWidth('↵') != 1 {
		t.Fatal("narrow runes")
	}
	if runeWidth('中') != 2 || runeWidth('한') != 2 || runeWidth('🚀') != 2 {
		t.Fatal("wide runes")
	}
	if got := clipToWidth("ab中cd", 4); got != "ab中" {
		t.Fatalf("clip on wide boundary: %q", got)
	}
}
