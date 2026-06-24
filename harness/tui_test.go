package harness

import (
	"bufio"
	"os"
	"strings"
	"testing"

	"github.com/mike-diff/sesh/agent"
)

// TestSegmentsImageRenumbering: image tokens display as [image-K] by order of
// appearance, independent of their absolute rune index, so deleting an earlier
// image renumbers the rest. The token rune stays the absolute images-slice index
// for byte lookup. Breaker: number the label off (r-imageBase)+1 instead of the
// running appearance count and the second image reads [image-3] not [image-2].
func TestSegmentsImageRenumbering(t *testing.T) {
	tc := &tuiConsole{images: make([]agent.Image, 3)}
	// Buffer holds tokens for absolute indices 0 and 2 (index 1 was "deleted":
	// its token is gone from the buffer but the slice is not compacted).
	tc.buf = []rune{'a', imageBase + 0, 'b', imageBase + 2}
	if got := strings.Join(tc.segments(), ""); got != "a[image-1]b[image-2]" {
		t.Fatalf("renumber by appearance: %q, want %q", got, "a[image-1]b[image-2]")
	}
}

// TestComposeMessageOrdersImagesAndLabels: composeMessage returns the ordered
// images a buffer carries and writes a [image-K] label for each (never the raw
// private-use rune), with snippets expanded inline. The image order follows the
// buffer, not the absolute index. Breaker: append t.images by absolute index
// rather than in buffer order and the returned images come back reversed.
func TestComposeMessageOrdersImagesAndLabels(t *testing.T) {
	imgs := []agent.Image{{Hash: "h0"}, {Hash: "h1"}}
	tc := &tuiConsole{images: imgs, snippets: []string{"BIG"}}
	// Appearance order is index 1 then index 0; a snippet token sits between them.
	tc.buf = []rune{'s', 'e', 'e', ' ', imageBase + 1, ' ', snippetBase + 0, ' ', imageBase + 0}
	text, got := tc.composeMessage()
	if text != "see [image-1] BIG [image-2]" {
		t.Fatalf("text = %q, want %q", text, "see [image-1] BIG [image-2]")
	}
	if strings.ContainsRune(text, imageBase) {
		t.Fatalf("text leaked a raw image token rune: %q", text)
	}
	if len(got) != 2 || got[0].Hash != "h1" || got[1].Hash != "h0" {
		t.Fatalf("images must follow buffer order, got %+v", got)
	}
}

// TestTakeImagesDrains: takeImages returns the last submit's images once and
// then nothing, so a message's images are attached exactly once. Breaker: drop
// the clear in takeImages and a second turn re-attaches the prior images.
func TestTakeImagesDrains(t *testing.T) {
	tc := &tuiConsole{submitImages: []agent.Image{{Hash: "h0"}}}
	if got := tc.takeImages(); len(got) != 1 {
		t.Fatalf("first take must return the submit's images, got %d", len(got))
	}
	if got := tc.takeImages(); got != nil {
		t.Fatalf("second take must be empty, got %d", len(got))
	}
}

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
		{"ctrl-j", "ab\ncd\r"},                                  // \n: any terminal
		{"backslash-enter", "ab\\\rcd\r"},                       // \ + Enter: universal fallback
		{"kitty shift+enter", "ab\033[13;2ucd\r"},               // CSI 13;2u
		{"extended shift+enter", "ab\033[27;2;13~cd\r"},         // CSI 27;2;13~ (tmux/xterm)
		{"kitty functional shift+enter", "ab\033[57414;3ucd\r"}, // CSI 57414;3u (alt+enter, functional codepoint)
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

// TestAttendTurnQueuesAndCancels: while a turn runs, the live editor cancels the
// turn on a bare Escape. Breaker: drop the bare-Escape branch and cancel is never
// called.
func TestAttendTurnQueuesAndCancels(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "tui-out")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	tc := &tuiConsole{out: f, in: bufio.NewReader(strings.NewReader("\x1b")), cols: 80}
	canceled := false
	if err := tc.attendTurn(turnAttend{
		done:   make(chan struct{}), // never closes; the script's EOF ends attend
		cancel: func() { canceled = true },
		queue:  func(string) {},
	}); err != errTurnOver {
		t.Fatalf("attendTurn err = %v, want errTurnOver", err)
	}
	if !canceled {
		t.Fatal("bare Escape must cancel the turn")
	}
}

// TestAttendTurnSteerCancelsImmediately: a message typed and submitted with Enter
// while a turn runs both queues the steer AND cancels the in-flight turn, so the
// steer is acted on now instead of waiting for the next iteration boundary. The
// script has no Escape: the cancel must come from the steer itself. Breaker:
// remove the turn.cancel() call in the turn-mode Enter branch and canceled stays
// false while the message is still queued.
func TestAttendTurnSteerCancelsImmediately(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "tui-out")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	tc := &tuiConsole{out: f, in: bufio.NewReader(strings.NewReader("fix it\r")), cols: 80}
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
		t.Fatal("queuing a steer must also cancel the in-flight turn")
	}
}

// TestAttendTurnStopCommandCancelsWithoutQueuing: typing "/stop" and Enter while
// a turn runs aborts it like Escape and sends nothing to the model. Unlike an
// ordinary steer it must cancel WITHOUT queuing, so the next turn gets no input.
// The match is case-insensitive and exact. Breaker: route /stop through the steer
// path and it queues "/stop" (queued non-empty), or drop the cancel and canceled
// stays false.
func TestAttendTurnStopCommandCancelsWithoutQueuing(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "tui-out")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	tc := &tuiConsole{out: f, in: bufio.NewReader(strings.NewReader("/STOP\r")), cols: 80}
	var queued []string
	canceled := false
	if err := tc.attendTurn(turnAttend{
		done:   make(chan struct{}), // never closes; the script's EOF ends attend
		cancel: func() { canceled = true },
		queue:  func(s string) { queued = append(queued, s) },
	}); err != errTurnOver {
		t.Fatalf("attendTurn err = %v, want errTurnOver", err)
	}
	if !canceled {
		t.Fatal("/stop must cancel the in-flight turn")
	}
	if len(queued) != 0 {
		t.Fatalf("/stop must not queue anything, got %v", queued)
	}
}

// TestBeginInputPreservesDraftAcrossTurnEnd: a draft typed during a turn (but
// never submitted) must survive the working-to-completed transition and remain
// in the editor when the next prompt opens, so in-progress text is not lost.
// attendTurn leaves the typed buffer in place when the turn ends; the next
// ReadLine re-opens the editor through beginInput, which must carry that buffer
// forward instead of zeroing it. Breaker: revert beginInput to an unconditional
// t.buf = nil and the submitted line comes back empty instead of "fix it".
func TestBeginInputPreservesDraftAcrossTurnEnd(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "tui-out")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	tc := &tuiConsole{out: f, in: bufio.NewReader(strings.NewReader("fix it")), cols: 80}
	// Phase 1: the user types a steering draft while a turn runs; the stream's
	// EOF ends the attend (errTurnOver) the way a turn finishing on its own does.
	if err := tc.attendTurn(turnAttend{
		done:   make(chan struct{}), // never closes; EOF ends attend
		cancel: func() {},
		queue:  func(string) {},
	}); err != errTurnOver {
		t.Fatalf("attendTurn err = %v, want errTurnOver", err)
	}
	if got := string(tc.buf); got != "fix it" {
		t.Fatalf("draft must survive attendTurn: buf = %q, want %q", got, "fix it")
	}
	// Phase 2: the next prompt re-opens the editor. EOF closed the pump's
	// channel, so re-arm a fresh input source the way a live console keeps
	// reading, then submit with Enter.
	tc.runes = nil
	tc.in = bufio.NewReader(strings.NewReader("\r"))
	line, err := tc.ReadLine("-> ")
	if err != nil {
		t.Fatalf("ReadLine: %v", err)
	}
	if line != "fix it" {
		t.Fatalf("draft must carry into the next prompt: line = %q, want %q", line, "fix it")
	}
}

// TestBeginInputDropsMaskedDraftForOrdinaryPrompt: a non-empty buffer left from
// a masked prompt must never carry into an ordinary one, so a secret cannot
// leak across prompt types. The carry-over branch is gated on !mask, so a
// non-empty masked buffer is cleared when the next (non-masked) prompt opens.
// Breaker: drop the !mask guard on the carry-over branch and the secret text
// survives into the ordinary prompt.
func TestBeginInputDropsMaskedDraftForOrdinaryPrompt(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "tui-out")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	// Seed the exact state a masked prompt leaves mid-edit (non-empty buffer,
	// masked), then open an ordinary prompt: the secret must not carry.
	tc := &tuiConsole{out: f, cols: 80, buf: []rune("hush"), pos: 4, mask: true}
	tc.beginInput("-> ", false)
	if len(tc.buf) != 0 {
		t.Fatalf("a masked draft must not carry into an ordinary prompt: buf = %q", string(tc.buf))
	}
}

// TestEscIsBareKeysOffIntroducer: bare-Escape detection decides by the byte
// following Esc, not by timing, so it holds up under tmux/SSH latency. A lone
// Esc with the stream then closed is bare; an Esc followed by a CSI introducer
// ([) is a sequence (not bare); an Esc followed by a printable like 'x' is bare,
// because only [ and O introduce a sequence. The channel is pre-seeded so the
// result never depends on the wall clock. Breaker: revert escIsBare to treating
// any present byte as non-bare and the 'x' case flips to false.
func TestEscIsBareKeysOffIntroducer(t *testing.T) {
	cases := []struct {
		name string
		next rune // the byte queued after Esc; 0 means "queue nothing, close"
		want bool
	}{
		{"lone esc then closed", 0, true},
		{"esc then CSI introducer", '[', false},
		{"esc then SS3 introducer", 'O', false},
		{"esc then printable", 'x', true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			tc := &tuiConsole{runes: make(chan rune, 1)}
			if c.next != 0 {
				tc.runes <- c.next
			}
			close(tc.runes) // the closed-stream branch must not misread a queued byte
			if got := tc.escIsBare(); got != c.want {
				t.Fatalf("escIsBare after Esc+%q = %v, want %v", c.next, got, c.want)
			}
		})
	}
}

// attendKeys drives attendTurn headless over a scripted keystroke stream and
// reports whether the turn was canceled and what (if anything) was queued. The
// done channel never closes, so the script's trailing EOF is what ends attend:
// this lets a test feed a sequence and then observe state without racing a
// real turn boundary.
func attendKeys(t *testing.T, keys string) (canceled bool, queued []string) {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "tui-out")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	tc := &tuiConsole{out: f, in: bufio.NewReader(strings.NewReader(keys)), cols: 80}
	if err := tc.attendTurn(turnAttend{
		done:   make(chan struct{}), // never closes; the script's EOF ends attend
		cancel: func() { canceled = true },
		queue:  func(s string) { queued = append(queued, s) },
	}); err != errTurnOver {
		t.Fatalf("attendTurn err = %v, want errTurnOver", err)
	}
	return canceled, queued
}

// TestAttendTurnCSIUEscapeCancels: under extended-keys (CSI-u) mode the Escape
// KEY arrives as a sequence, not a bare 0x1b, so the bare path never sees it.
// The bare form CSI 27 u and the modified form CSI 27;2 u must both cancel the
// turn exactly like a bare Escape, and neither queues nor inserts anything.
// Breaker: remove the isCSIUEscape case in handleEscape and canceled stays false.
func TestAttendTurnCSIUEscapeCancels(t *testing.T) {
	for _, c := range []struct{ name, keys string }{
		{"bare csi-u escape", "\033[27u"},       // CSI 27 u
		{"modified csi-u escape", "\033[27;2u"}, // CSI 27;2 u (a modifier held)
	} {
		t.Run(c.name, func(t *testing.T) {
			canceled, queued := attendKeys(t, c.keys)
			if !canceled {
				t.Fatal("CSI-u Escape must cancel the in-flight turn")
			}
			if len(queued) != 0 {
				t.Fatalf("CSI-u Escape must not queue anything, got %v", queued)
			}
		})
	}
}

// TestAttendTurnShiftEnterDoesNotCancel: extended Enter (CSI 13;2u) shares the
// final 'u' with the CSI-u Escape but carries codepoint 13, not 27, so it must
// insert a newline and leave the turn running. Feeding only the sequence (no
// submitting Enter) isolates it from the steer-cancel that a real Enter would
// trigger. Breaker: match the cancel on final=='u' alone and this wrongly cancels.
func TestAttendTurnShiftEnterDoesNotCancel(t *testing.T) {
	canceled, queued := attendKeys(t, "ab\033[13;2ucd")
	if canceled {
		t.Fatal("Shift+Enter (codepoint 13) must not be treated as Escape")
	}
	if len(queued) != 0 {
		t.Fatalf("Shift+Enter must not queue anything on its own, got %v", queued)
	}
}

// TestAttendTurnEditingKeysDoNotCancel: ordinary CSI editing sequences during a
// turn (up-arrow CSI A, delete CSI 3~) edit the buffer and never cancel, so the
// CSI-u Escape routing did not swallow the rest of the editing keys. Breaker:
// route every decoded sequence to cancel and these flip canceled to true.
func TestAttendTurnEditingKeysDoNotCancel(t *testing.T) {
	for _, c := range []struct{ name, keys string }{
		{"up arrow", "\033[A"}, // CSI A: history / cursor up
		{"delete", "x\033[3~"}, // CSI 3~: delete at cursor
	} {
		t.Run(c.name, func(t *testing.T) {
			if canceled, _ := attendKeys(t, c.keys); canceled {
				t.Fatalf("%s must not cancel the turn", c.name)
			}
		})
	}
}

// TestIsCSIUCtrlC: only Ctrl-modified codepoint 99 ('c') under final 'u' is
// Ctrl-C. Plain 'c' (99 or 99;1), the Escape key (27), and extended Enter
// (13;2) must all be excluded so they keep their own meaning. Breaker: match on
// final=='u' alone and the escape/enter cases flip to true.
func TestIsCSIUCtrlC(t *testing.T) {
	cases := []struct {
		params string
		final  rune
		want   bool
	}{
		{"99;5", 'u', true},  // Ctrl+c: codepoint 99, modifier 5 (Ctrl)
		{"99;7", 'u', true},  // Ctrl+Alt+c: the Ctrl bit is still set
		{"99", 'u', false},   // plain 'c', no modifier
		{"99;1", 'u', false}, // explicit "no modifier"
		{"27", 'u', false},   // the Escape key
		{"13;2", 'u', false}, // Shift+Enter
		{"99;5", '~', false}, // right codepoint+mod but not a CSI-u final
	}
	for _, c := range cases {
		if got := isCSIUCtrlC(c.params, c.final); got != c.want {
			t.Errorf("isCSIUCtrlC(%q, %q) = %v, want %v", c.params, c.final, got, c.want)
		}
	}
}

// TestIsCSIUEnter: under final 'u', codepoint 13 (standard) and 57414 (kitty's
// functional Enter, 0xE006) are both Enter; a present modifier other than "1"
// marks it modified (Shift/Alt -> newline) while bare or "1" is unmodified
// (submit). Escape (27) and Ctrl-C (99) must be excluded. Breaker: drop the
// 57414 codepoint and the functional case flips to enter=false.
func TestIsCSIUEnter(t *testing.T) {
	cases := []struct {
		params        string
		final         rune
		enter, modded bool
	}{
		{"13", 'u', true, false},     // standard Enter, unmodified
		{"57414", 'u', true, false},  // kitty functional Enter, unmodified
		{"13;2", 'u', true, true},    // Shift+Enter
		{"57414;3", 'u', true, true}, // Alt+Enter (functional codepoint)
		{"13;1", 'u', true, false},   // explicit "no modifier"
		{"27", 'u', false, false},    // the Escape key
		{"99;5", 'u', false, false},  // Ctrl-C
		{"13", '~', false, false},    // right codepoint but not a CSI-u final
	}
	for _, c := range cases {
		enter, modded := isCSIUEnter(c.params, c.final)
		if enter != c.enter || modded != c.modded {
			t.Errorf("isCSIUEnter(%q, %q) = (%v, %v), want (%v, %v)",
				c.params, c.final, enter, modded, c.enter, c.modded)
		}
	}
}

// TestReadLineCSIUEnterSubmits: under extended-keys mode an unmodified Enter
// arrives as a CSI-u sequence (kitty functional 57414), never a bare \r, so the
// editor must normalize it back to a Return and submit the typed line through
// the single submit path. Breaker: remove the unget('\r') for an unmodified
// CSI-u Enter and ReadLine never returns the line.
func TestReadLineCSIUEnterSubmits(t *testing.T) {
	if line, _ := driveKeys(t, nil, "hi\033[57414u"); line != "hi" {
		t.Fatalf("CSI-u Enter must submit: line = %q, want %q", line, "hi")
	}
}

// TestAttendTurnCSIUEnterSteers: during a turn an unmodified CSI-u Enter must
// take the same steer path a bare \r would (queue the text and cancel the
// in-flight turn), not insert a newline. Breaker: route an unmodified CSI-u
// Enter to the newline case and nothing is queued.
func TestAttendTurnCSIUEnterSteers(t *testing.T) {
	canceled, queued := attendKeys(t, "fix it\033[57414u")
	if len(queued) != 1 || queued[0] != "fix it" {
		t.Fatalf("CSI-u Enter mid-turn must queue the steer, got %v", queued)
	}
	if !canceled {
		t.Fatal("queuing a steer via CSI-u Enter must cancel the in-flight turn")
	}
}

// ctrlCKeys drives readLineMode headless over a scripted keystroke stream with a
// spy onCtrlC installed, reporting whether the hook fired and what the editor
// returned. The stream's trailing EOF ends the idle read (turn == nil), so the
// test can feed a Ctrl-C form and observe the hook without racing a real turn.
func ctrlCKeys(t *testing.T, keys string) (fired bool, line string) {
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
		onCtrlC: func() { fired = true },
	}
	line, _ = tc.readLineMode("-> ", false, nil)
	return fired, line
}

// TestHandleEscapeCSIUCtrlCInvokesHook: under extended-keys mode Ctrl-C arrives
// as CSI 99;5 u (never an OS signal), so the editor must route it to onCtrlC and
// not type a 'c' or run any editing action. Breaker: drop the isCSIUCtrlC branch
// in handleEscape and the hook never fires.
func TestHandleEscapeCSIUCtrlCInvokesHook(t *testing.T) {
	fired, line := ctrlCKeys(t, "\033[99;5u")
	if !fired {
		t.Fatal("CSI-u Ctrl-C must invoke onCtrlC")
	}
	if line != "" {
		t.Fatalf("CSI-u Ctrl-C must not edit the buffer, got %q", line)
	}
}

// TestReadLineRawCtrlCInvokesHook: a raw 0x03 byte (terminals that send no
// signal) also routes to onCtrlC, without holding the editor mutex so the
// force-quit and warning Prints cannot deadlock. Breaker: drop the 0x03 case in
// readLineMode and the hook never fires.
func TestReadLineRawCtrlCInvokesHook(t *testing.T) {
	fired, line := ctrlCKeys(t, "\x03")
	if !fired {
		t.Fatal("raw 0x03 must invoke onCtrlC")
	}
	if line != "" {
		t.Fatalf("raw 0x03 must not produce input, got %q", line)
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
