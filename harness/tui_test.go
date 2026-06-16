package harness

import (
	"bufio"
	"os"
	"strings"
	"testing"
)

// TestSegWidthIgnoresANSI: a segment carrying highlight SGR measures by its
// visible width, so the input window math stays aligned. Breaker: stop
// stripping ANSI in segWidth and the escape bytes inflate the width.
func TestSegWidthIgnoresANSI(t *testing.T) {
	if w := segWidth("\033[35mx\033[0m"); w != 1 {
		t.Fatalf("segWidth = %d, want 1 (SGR must not count)", w)
	}
}

func TestStripANSI(t *testing.T) {
	cases := map[string]string{
		"plain":                      "plain",
		"\033[33m  approve? \033[0m": "  approve? ",
		"\033[2K\033[5;1Htext":       "text",
	}
	for in, want := range cases {
		if got := stripANSI(in); got != want {
			t.Fatalf("stripANSI(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestPlainConsoleReadKey: piped "y\n" answers a gate like a keypress, and an
// empty line means no.
func TestPlainConsoleReadKey(t *testing.T) {
	c := &plainConsole{in: bufio.NewReader(strings.NewReader("y\n\nn\n"))}
	if k, err := c.ReadKey("? "); err != nil || k != 'y' {
		t.Fatalf("y line: %q err=%v", k, err)
	}
	if k, _ := c.ReadKey("? "); k != '\n' {
		t.Fatalf("empty line should read as decline: %q", k)
	}
	if k, _ := c.ReadKey("? "); k != 'n' {
		t.Fatalf("n line: %q", k)
	}
}

// TestPlainSelect: the pipe fallback picker takes a number, treats blank or
// junk as cancel, and rejects out-of-range picks.
func TestPlainSelect(t *testing.T) {
	c := &plainConsole{in: bufio.NewReader(strings.NewReader("2\n\nzzz\n9\n"))}
	items := []string{"a", "b", "c"}
	if idx, err := c.Select("t", items, 0); err != nil || idx != 1 {
		t.Fatalf("pick 2: idx=%d err=%v", idx, err)
	}
	if idx, _ := c.Select("t", items, 0); idx != -1 {
		t.Fatalf("blank should cancel: %d", idx)
	}
	if idx, _ := c.Select("t", items, 0); idx != -1 {
		t.Fatalf("junk should cancel: %d", idx)
	}
	if idx, _ := c.Select("t", items, 0); idx != -1 {
		t.Fatalf("out of range should cancel: %d", idx)
	}
}

// TestNewConsoleFallsBack: under go test stdin/stdout are not usable
// terminals, so the plain console must be selected.
func TestNewConsoleFallsBack(t *testing.T) {
	c := newConsole()
	defer c.Close()
	if _, ok := c.(*plainConsole); !ok {
		t.Fatalf("expected plainConsole, got %T", c)
	}
}

// TestLayoutWrapsAndBreaks: a long logical line wraps by display width and a
// hard newline starts a fresh row. Breaker: drop the wrap flush in layout and
// 25 chars stay on one row instead of three.
func TestLayoutWrapsAndBreaks(t *testing.T) {
	tc := &tuiConsole{buf: []rune(strings.Repeat("a", 25))}
	L := tc.layout(10)
	if len(L.rows) != 3 || segWidth(L.rows[0]) != 10 || segWidth(L.rows[2]) != 5 {
		t.Fatalf("wrap: %d rows %v", len(L.rows), L.rows)
	}
	tc = &tuiConsole{buf: []rune("ab\ncde")}
	L = tc.layout(10)
	if len(L.rows) != 2 || L.rows[0] != "ab" || L.rows[1] != "cde" {
		t.Fatalf("newline break: %d rows %v", len(L.rows), L.rows)
	}
}

// TestLayoutCursorMapping: a cursor position maps to the right visual row and
// column across a hard newline and across a soft wrap (where the position
// before the wrapping rune belongs to the new row, not the end of the old one).
// Breaker: record the cursor before the wrap flush and colOf[5] becomes 5,0's row.
func TestLayoutCursorMapping(t *testing.T) {
	tc := &tuiConsole{buf: []rune("ab\ncd")} // a0 b1 \n2 c3 d4
	L := tc.layout(10)
	for _, c := range []struct{ pos, row, col int }{
		{2, 0, 2}, // before the newline: end of row 0
		{3, 1, 0}, // after the newline: start of row 1
		{5, 1, 2}, // end of buffer
	} {
		if L.rowOf[c.pos] != c.row || L.colOf[c.pos] != c.col {
			t.Fatalf("pos %d -> (%d,%d), want (%d,%d)", c.pos, L.rowOf[c.pos], L.colOf[c.pos], c.row, c.col)
		}
	}
	tc = &tuiConsole{buf: []rune(strings.Repeat("a", 12))}
	L = tc.layout(5) // rows of 5,5,2
	if L.rowOf[5] != 1 || L.colOf[5] != 0 {
		t.Fatalf("wrap boundary pos 5 -> (%d,%d), want (1,0)", L.rowOf[5], L.colOf[5])
	}
}

// TestCursorVerticalNav: Up/Down move between visual rows keeping the column,
// and report false at the top/bottom row so history navigation takes over.
// Breaker: invert the edge guard (return true at row 0) and Up never reaches
// history.
func TestCursorVerticalNav(t *testing.T) {
	tc := &tuiConsole{buf: []rune("ab\ncd"), cols: 20, prompt: "you> "}
	tc.pos = 4 // row 1, col 1 (before 'd')
	if !tc.cursorUpLocked() || tc.pos != 1 {
		t.Fatalf("up to row 0 col 1: moved=%v pos=%d", true, tc.pos)
	}
	if tc.cursorUpLocked() {
		t.Fatal("up from the top row must fall through to history")
	}
	if !tc.cursorDownLocked() || tc.pos != 4 {
		t.Fatalf("down to row 1: pos=%d", tc.pos)
	}
	if tc.cursorDownLocked() {
		t.Fatal("down from the bottom row must fall through to history")
	}
}

// TestEditorNewlineKeys drives the real editor headless and asserts that every
// way to insert a newline lands the same multi-line message, across the terminal
// encodings sesh must accept. Each row exercises a distinct decode path and
// breaks on its own one-line change (drop the \+Enter branch; drop a CSI match).
func TestEditorNewlineKeys(t *testing.T) {
	for _, c := range []struct{ name, keys string }{
		{"ctrl-j", "ab\ncd\r"},                          // \n: any terminal
		{"backslash-enter", "ab\\\rcd\r"},               // \ + Enter: universal fallback
		{"kitty shift+enter", "ab\033[13;2ucd\r"},       // CSI 13;2u
		{"extended shift+enter", "ab\033[27;2;13~cd\r"}, // CSI 27;2;13~ (tmux/xterm)
	} {
		if line, _ := driveKeys(t, nil, c.keys); line != "ab\ncd" {
			t.Fatalf("%s: line = %q, want %q", c.name, line, "ab\ncd")
		}
	}
}

// TestEditorSecretSubmitsOnBackslashEnter: in a masked prompt Enter always
// submits, even after a backslash, so \+Enter never traps a password behind a
// newline. Breaker: drop the !mask guard on the \+Enter branch.
func TestEditorSecretSubmitsOnBackslashEnter(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "tui-out")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	tc := &tuiConsole{out: f, in: bufio.NewReader(strings.NewReader("pw\\\r")), cols: 80}
	if line, _ := tc.readLine("pw> ", true); line != "pw\\" {
		t.Fatalf("masked \\+Enter must submit: line = %q, want %q", line, "pw\\")
	}
}

// TestAttendTurnQueuesAndCancels: while a turn runs, the live editor queues a
// typed message on Enter (instead of submitting) and cancels the turn on a bare
// Escape. Breaker: drop the turn-mode Enter branch and "fix it" is never queued;
// drop the bare-Escape branch and cancel is never called.
func TestAttendTurnQueuesAndCancels(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "tui-out")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	tc := &tuiConsole{out: f, in: bufio.NewReader(strings.NewReader("fix it\r\x1b")), cols: 80}
	var queued []string
	canceled := false
	if err := tc.attendTurn(turnAttend{
		done:   make(chan struct{}), // never closes; the script's EOF ends attend
		cancel: func() { canceled = true },
		queue:  func(s string) { queued = append(queued, s) },
	}); err != errTurnOver {
		t.Fatalf("attendTurn err = %v, want errTurnOver", err)
	}
	if len(queued) != 1 || queued[0] != "fix it" {
		t.Fatalf("Enter must queue the message, got %v", queued)
	}
	if !canceled {
		t.Fatal("bare Escape must cancel the turn")
	}
}

// TestInputCapClampsToTerminal: the editor height honors the dial but never
// grows past what the terminal can hold under the dividers and status. Breaker:
// drop the t.rows-reserve clamp and a 7-row terminal returns 6.
func TestInputCapClampsToTerminal(t *testing.T) {
	if c := (&tuiConsole{rows: 40, maxInputRows: 6}).inputCap(); c != 6 {
		t.Fatalf("roomy terminal: cap=%d want 6", c)
	}
	if c := (&tuiConsole{rows: 7, maxInputRows: 6}).inputCap(); c != 3 {
		t.Fatalf("short terminal: cap=%d want 3", c)
	}
	if c := (&tuiConsole{rows: 10, maxInputRows: 6, procStatus: "x"}).inputCap(); c != 5 {
		t.Fatalf("process row reserved: cap=%d want 5", c)
	}
}
