// The terminal footer UI. The footer (status line + input row) is anchored to
// the conversation tail: it is drawn after the content while waiting for
// input and removed while a turn streams, so all output scrolls through the
// terminal's real bottom row. That keeps native scrollback, tmux copy-mode,
// search, and selection working: the lessons of the alt-screen/scroll-region
// tradeoffs that plague full-frame TUIs. Nothing but ANSI escapes and stty.
//
// The input row is a small line editor: cursor movement, per-project history
// on up/down, bracketed paste (large pastes collapse to atomic [snippet #N]
// tokens, expanded on submit), Shift+Enter or Ctrl-J for newlines, and tab
// completion fed by the repl. When stdin or stdout is not a terminal (pipes,
// tests, print mode) everything falls back to the plain line-based console.
package harness

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/mike-diff/sesh/agent"
)

// console abstracts where user input comes from and how the footer is drawn,
// so the same REPL serves a plain pipe and a full-terminal session. This is
// the input-side twin of the agent core's Hooks: injectable, swappable.
type console interface {
	ReadLine(prompt string) (string, error)                        // a line of input
	ReadSecret(prompt string) (string, error)                      // a line, never echoed
	ReadKey(prompt string) (byte, error)                           // one key (approval gates)
	Select(title string, items []string, current int) (int, error) // pick from a list; -1 = cancelled
	Print(s string)                                                // transcript output (footer makes room)
	SetStatus(s string)                                            // update the status line
	SetTitle(s string)                                             // terminal title (turn progress)
	Close()                                                        // restore the terminal
}

// activeConsole routes transcript output once the interactive console exists,
// so the footer can lift out of the way of every write and re-seat below it.
var activeConsole console

// emit is the transcript print used by all interactive output. Before a
// console exists (startup, print mode, doctor) it goes straight to stdout.
func emit(format string, a ...any) {
	s := fmt.Sprintf(format, a...)
	if activeConsole != nil {
		activeConsole.Print(s)
		return
	}
	fmt.Print(s)
}

// newConsole picks the footer TUI when both ends are a real terminal,
// otherwise the plain console.
func newConsole() console {
	if isTerminal(os.Stdin) && isTerminal(os.Stdout) {
		if t, err := newTUI(); err == nil {
			return t
		}
	}
	return &plainConsole{in: bufio.NewReader(os.Stdin)}
}

func isTerminal(f *os.File) bool {
	fi, err := f.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

// ---------------------------------------------------------------------------
// plainConsole: line-based behavior, for pipes and tests.
// ---------------------------------------------------------------------------

// plainConsole is the pipe/script fallback. out redirects Print (print mode
// points it at stderr so management notices never contaminate piped stdout);
// nil means stdout.
type plainConsole struct {
	in  *bufio.Reader
	out io.Writer
}

func (c *plainConsole) ReadLine(prompt string) (string, error) {
	fmt.Print(prompt)
	line, err := c.in.ReadString('\n')
	if err != nil && strings.TrimSpace(line) == "" {
		return "", err
	}
	return strings.TrimSpace(line), nil
}

func (c *plainConsole) ReadSecret(prompt string) (string, error) {
	fmt.Print(prompt)
	setEcho(false)
	line, err := c.in.ReadString('\n')
	setEcho(true)
	fmt.Println()
	if err != nil && strings.TrimSpace(line) == "" {
		return "", err
	}
	return strings.TrimSpace(line), nil
}

// ReadKey reads a line and takes its first byte, so piped input like "y\n"
// answers a gate the same way a single keypress does.
func (c *plainConsole) ReadKey(prompt string) (byte, error) {
	line, err := c.ReadLine(prompt)
	if err != nil {
		return 0, err
	}
	if line == "" {
		return '\n', nil
	}
	return line[0], nil
}

func (c *plainConsole) Print(s string) {
	if c.out != nil {
		fmt.Fprint(c.out, s)
		return
	}
	fmt.Print(s)
}
func (c *plainConsole) SetStatus(string) {}
func (c *plainConsole) SetTitle(string)  {}
func (c *plainConsole) Close()           {}

// Select falls back to a numbered list and a number prompt, so pipes and
// scripts can drive pickers too.
func (c *plainConsole) Select(title string, items []string, current int) (int, error) {
	fmt.Println(title)
	for i, it := range items {
		marker := "  "
		if i == current {
			marker = "* "
		}
		fmt.Printf("%s%2d  %s\n", marker, i+1, it)
	}
	line, err := c.ReadLine(fmt.Sprintf("pick [1-%d, enter cancels]> ", len(items)))
	if err != nil || line == "" {
		return -1, err
	}
	n, aerr := strconv.Atoi(line)
	if aerr != nil || n < 1 || n > len(items) {
		return -1, nil
	}
	return n - 1, nil
}

// ---------------------------------------------------------------------------
// tuiConsole: the anchored footer with a line editor.
// ---------------------------------------------------------------------------

// snippetBase marks collapsed-paste tokens in the buffer: one private-use
// rune per snippet, so the cursor treats a snippet as a single character.
const snippetBase rune = 0xE000

// imageBase marks pasted-image tokens in the buffer, in a separate private-use
// block from snippetBase. A token's rune value is its absolute index into the
// images slice (stable for byte lookup); its displayed [image-N] number is by
// order of appearance, so a deletion renumbers the rest.
const imageBase rune = 0xF000

type tuiConsole struct {
	mu         sync.Mutex
	out        *os.File
	in         *bufio.Reader
	rows       int
	cols       int
	status     string
	procStatus string // the optional process row; "" means no row
	footer     bool   // status+input rows are currently drawn at the content tail
	footerProc bool   // the last footer draw included the process row
	col        int    // logical column at the content tail (0 = fresh line)
	pad        bool   // footer was drawn after a partial line, behind a pushed \n
	atExit     func() // run on signal-driven exit (reap owned processes)
	onCtrlC    func() // a Ctrl-C keystroke (extended-keys CSI-u, or raw 0x03) reaches quit through this; nil disables

	// the input editor's state
	prompt string
	buf    []rune
	pos    int  // cursor index into buf
	mask   bool // draw the buffer as asterisks (secrets)

	maxInputRows int // editor grows to this many rows, then scrolls (dial)
	winTop       int // first visual row shown once the editor scrolls
	curVis       int // cursor's row within the drawn window, for footer teardown

	// pastes large enough to collapse; index i renders as [snippet #i+1]
	snippets []string

	// images is the append-only list of pasted images for the message being
	// composed; an imageBase token's rune value indexes into it. It is never
	// compacted on delete (that would invalidate live token indices). submitImages
	// holds the images of the last submitted message, drained by takeImages.
	// visionOK reports whether the active model can see images; nil disables the
	// gate (the plain console and tests leave it unset).
	images       []agent.Image
	submitImages []agent.Image
	visionOK     func() bool

	// history and completion
	hist      []string
	histIdx   int    // == len(hist) means the live draft
	draft     []rune // stashed draft while navigating history
	histPath  string
	completer func(line string) []string
	mention   *mentions // recognizes/completes/highlights #skill and @file tokens; nil disables

	// runes is the single decoded-keystroke stream: one pump goroutine owns the
	// real stdin and feeds this channel, so reads become selectable. That is what
	// lets the editor stay live during a turn (select on a key or the turn's
	// end) instead of freezing while the model works. ungot is a tiny pushback
	// for lookahead (bare-Escape detection). Both are touched only by whichever
	// consumer is currently reading; the pump only ever sends.
	runes chan rune
	ungot []rune
}

func newTUI() (*tuiConsole, error) {
	t := &tuiConsole{out: os.Stdout, in: bufio.NewReader(os.Stdin), maxInputRows: 6, runes: make(chan rune, 1024)}
	if err := t.measure(); err != nil {
		return nil, err
	}
	go t.pump()
	// cbreak: keys arrive immediately and unechoed; output processing stays
	// on. -icrnl keeps Enter (\r) distinct from Ctrl-J (\n): with the default
	// mapping both arrive as \n and submit-vs-newline cannot be told apart.
	if err := stty("-icanon", "-echo", "-icrnl"); err != nil {
		return nil, err
	}
	t.histPath = historyPath()
	t.hist = loadHistory(t.histPath)
	fmt.Fprint(t.out, "\033[?2004h") // bracketed paste: pastes arrive marked
	// Kitty keyboard protocol, disambiguation flag only: makes Shift+Enter
	// distinguishable from Enter where supported, without the broader flags
	// that break IME composition. Terminals that don't support it ignore the
	// push, and Ctrl-J inserts a newline everywhere regardless.
	fmt.Fprint(t.out, "\033[>1u")

	// Restore the terminal on SIGTERM; raw-ish mode must never leak. SIGINT is
	// deliberately not handled here: main owns Ctrl-C (cancel turn, then quit)
	// and calls Close itself before exiting.
	sigc := make(chan os.Signal, 1)
	signal.Notify(sigc, syscall.SIGTERM, syscall.SIGHUP)
	go func() {
		<-sigc
		if t.atExit != nil {
			t.atExit() // reap owned processes before the terminal goes
		}
		t.Close()
		os.Exit(143)
	}()
	// Re-measure on terminal resize; the input row re-windows on the next draw.
	winc := make(chan os.Signal, 1)
	signal.Notify(winc, syscall.SIGWINCH)
	go func() {
		for range winc {
			t.mu.Lock()
			t.measure()
			if t.footer {
				t.removeFooterLocked()
				t.drawFooterLocked()
			}
			t.mu.Unlock()
		}
	}()
	return t, nil
}

func stty(args ...string) error {
	cmd := exec.Command("stty", args...)
	cmd.Stdin = os.Stdin
	return cmd.Run()
}

func (t *tuiConsole) measure() error {
	cmd := exec.Command("stty", "size")
	cmd.Stdin = os.Stdin
	out, err := cmd.Output()
	if err != nil {
		return err
	}
	parts := strings.Fields(strings.TrimSpace(string(out)))
	if len(parts) != 2 {
		return fmt.Errorf("unexpected stty size output %q", out)
	}
	t.rows, _ = strconv.Atoi(parts[0])
	t.cols, _ = strconv.Atoi(parts[1])
	if t.rows < 7 || t.cols < 20 {
		return fmt.Errorf("terminal too small (%dx%d)", t.cols, t.rows)
	}
	return nil
}

// ---------------------------------------------------------------------------
// The footer: two rows that stay seated below the content at all times,
// pi-style. Every transcript write lifts them, writes through (so output
// scrolls the real screen and feeds scrollback), and re-seats them; DEC 2026
// synchronized output makes that atomic on terminals that support it.
// ---------------------------------------------------------------------------

// Print writes transcript output below-the-footer-safely. This is the one
// path all interactive output takes (via emit), which is what lets the footer
// stay visible during streaming without owning the screen.
func (t *tuiConsole) Print(s string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	had := t.footer
	fmt.Fprint(t.out, "\033[?2026h") // begin synchronized update (best effort)
	t.removeFooterLocked()
	t.writeLocked(s)
	if had {
		t.drawFooterLocked()
	}
	fmt.Fprint(t.out, "\033[?2026l")
}

// writeLocked emits content and tracks the logical column of the tail, so the
// footer can be re-seated after partial (still-streaming) lines. ANSI CSI
// sequences are skipped; wrapping is approximated by display width.
func (t *tuiConsole) writeLocked(s string) {
	fmt.Fprint(t.out, s)
	i := 0
	for i < len(s) {
		if s[i] == 0x1b && i+1 < len(s) && s[i+1] == '[' {
			j := i + 2
			for j < len(s) && (s[j] < '@' || s[j] > '~') {
				j++
			}
			i = j + 1
			continue
		}
		r, size := rune(s[i]), 1
		if r >= 0x80 {
			r, size = decodeRuneAt(s, i)
		}
		if r == '\n' || r == '\r' {
			t.col = 0
		} else {
			t.col += runeWidth(r)
		}
		i += size
	}
}

func decodeRuneAt(s string, i int) (rune, int) {
	for _, r := range s[i:] {
		return r, len(string(r))
	}
	return 0, 1
}

// visualCol is the on-screen column of the content tail within its last
// wrapped row.
func (t *tuiConsole) visualCol() int {
	if t.col <= 0 || t.cols <= 0 {
		return 0
	}
	return ((t.col - 1) % t.cols) + 1
}

// drawFooterLocked seats the footer below the content tail:
//
//	──────────────────  top divider
//	you> input…         the editor, one or more rows (cursor lives on one)
//	more text…
//	──────────────────  bottom divider
//	status line
//
// The editor grows with the text up to inputCap rows, then scrolls vertically
// with the cursor kept in view; a dim ⋯ in the gutter marks content scrolled
// off above or below. The cursor is left at the editing position. A partial
// streamed line gets a pushed newline first, undone on the next lift.
func (t *tuiConsole) drawFooterLocked() {
	if t.footer {
		return
	}
	t.pad = t.col > 0
	if t.pad {
		fmt.Fprint(t.out, "\n")
	}
	div := dim + strings.Repeat("─", t.cols) + reset
	s := t.status
	if !strings.ContainsRune(s, 0x1b) && segWidth(s) > t.cols {
		s = clipToWidth(s, t.cols)
	}

	promptW := len([]rune(stripANSI(t.prompt)))
	L := t.layout(t.cols - promptW)
	cur := L.rowOf[t.pos]
	winLen := len(L.rows)
	if cap := t.inputCap(); winLen > cap {
		winLen = cap
	}
	// keep the cursor's row inside the window, then clamp to the row range
	if cur < t.winTop {
		t.winTop = cur
	}
	if cur >= t.winTop+winLen {
		t.winTop = cur - winLen + 1
	}
	if maxTop := len(L.rows) - winLen; t.winTop > maxTop {
		t.winTop = maxTop
	}
	if t.winTop < 0 {
		t.winTop = 0
	}
	t.curVis = cur - t.winTop

	fmt.Fprintf(t.out, "\r\033[2K%s\n", div) // top divider
	for k := 0; k < winLen; k++ {
		gut := strings.Repeat(" ", promptW)
		switch {
		case k == 0 && t.winTop == 0: // first row, not scrolled: the prompt
			gut = t.prompt
		case k == 0: // content scrolled off above
			gut = scrollGutter(promptW)
		case k == winLen-1 && t.winTop+winLen < len(L.rows): // content below
			gut = scrollGutter(promptW)
		}
		fmt.Fprintf(t.out, "\r\033[2K%s%s\n", gut, L.rows[t.winTop+k])
	}
	fmt.Fprintf(t.out, "\r\033[2K%s\n", div) // bottom divider
	t.footerProc = t.procStatus != ""
	if t.footerProc {
		ps := t.procStatus
		if segWidth(ps) > t.cols {
			ps = clipToWidth(ps, t.cols)
		}
		fmt.Fprintf(t.out, "\r\033[2K%s%s%s\n", dim, s, reset) // status, with \n
		fmt.Fprintf(t.out, "\r\033[2K%s", ps)                  // process row, no \n
	} else {
		fmt.Fprintf(t.out, "\r\033[2K%s%s%s", dim, s, reset) // status, no trailing \n
	}
	// climb back to the cursor's input row and column
	up := winLen - t.curVis + 1
	if t.footerProc {
		up++
	}
	fmt.Fprintf(t.out, "\033[%dA\r", up)
	if col := promptW + L.colOf[t.pos]; col > 0 {
		fmt.Fprintf(t.out, "\033[%dC", col)
	}
	t.footer = true
}

// removeFooterLocked lifts the whole footer and returns the cursor to the exact
// content position, mid-line included. The cursor must be on the input row the
// last draw parked it on; clearing from the top divider down erases every
// footer row regardless of how many the editor grew to.
func (t *tuiConsole) removeFooterLocked() {
	if !t.footer {
		return
	}
	fmt.Fprintf(t.out, "\033[%dA\r\033[0J", t.curVis+1) // up to top divider, clear down
	t.footer = false
	if t.pad {
		fmt.Fprint(t.out, "\033[1A\r")
		if vc := t.visualCol(); vc > 0 {
			fmt.Fprintf(t.out, "\033[%dC", vc)
		}
	}
}

// refreshFooterLocked repaints the footer in place: lift, redraw, atomic under a
// synchronized update so a growing or scrolling editor never tears. Used after
// every keystroke, where the editor's height can change.
func (t *tuiConsole) refreshFooterLocked() {
	fmt.Fprint(t.out, "\033[?2026h")
	t.removeFooterLocked()
	t.drawFooterLocked()
	fmt.Fprint(t.out, "\033[?2026l")
}

// layout is the editor's wrapped view of the buffer at a given text width: the
// display string of each visual row (gutter excluded), and for every cursor
// position the visual row and column it maps to. Hard newlines start a new row;
// a long logical line wraps by display width. rowOf/colOf are indexed by cursor
// position 0..len(buf), so [pos] is where the cursor sits before that rune.
type layout struct {
	rows  []string
	rowOf []int
	colOf []int
}

func (t *tuiConsole) layout(textWidth int) layout {
	if textWidth < 1 {
		textWidth = 1
	}
	segs := t.segments()
	L := layout{rowOf: make([]int, len(t.buf)+1), colOf: make([]int, len(t.buf)+1)}
	var b strings.Builder
	w := 0
	flush := func() { L.rows = append(L.rows, b.String()); b.Reset(); w = 0 }
	record := func(p int) { L.rowOf[p] = len(L.rows); L.colOf[p] = w }
	for i, r := range t.buf {
		if r == '\n' {
			record(i) // before the newline: the end of the current row
			flush()
			continue
		}
		sw := segWidth(segs[i])
		if w+sw > textWidth && w > 0 {
			flush() // wrap: this rune (and the cursor before it) start a new row
		}
		record(i)
		b.WriteString(segs[i])
		w += sw
	}
	record(len(t.buf)) // the end-of-buffer position
	flush()
	return L
}

// inputCap is how many editor rows may show before it scrolls: the dial,
// clamped so the footer always fits with its dividers, status, optional process
// row, and one content line above it.
func (t *tuiConsole) inputCap() int {
	c := t.maxInputRows
	if c < 1 {
		c = 1
	}
	reserve := 4 // top divider, bottom divider, status, one content line
	if t.procStatus != "" {
		reserve = 5
	}
	if max := t.rows - reserve; c > max {
		c = max
	}
	if c < 1 {
		c = 1
	}
	return c
}

// scrollGutter is the dim ⋯ shown in the editor's gutter when content is
// scrolled out of view, aligned to where the prompt's text would end.
func scrollGutter(promptW int) string {
	if promptW < 1 {
		return dim + "⋯" + reset
	}
	return strings.Repeat(" ", promptW-1) + dim + "⋯" + reset
}

// posAtRowCol returns the cursor position on the given visual row whose column
// is nearest goalCol, for vertical cursor moves.
func posAtRowCol(L layout, row, goalCol int) int {
	best, bestDelta := 0, 1<<30
	for p, r := range L.rowOf {
		if r != row {
			continue
		}
		d := L.colOf[p] - goalCol
		if d < 0 {
			d = -d
		}
		if d < bestDelta {
			best, bestDelta = p, d
		}
	}
	return best
}

// cursorUpLocked / cursorDownLocked move the cursor one visual row, keeping the
// column. They report false at the top/bottom row so the caller falls through to
// history navigation, the readline behavior in a multi-line buffer.
func (t *tuiConsole) cursorUpLocked() bool {
	L := t.layout(t.cols - len([]rune(stripANSI(t.prompt))))
	row := L.rowOf[t.pos]
	if row == 0 {
		return false
	}
	t.pos = posAtRowCol(L, row-1, L.colOf[t.pos])
	return true
}

func (t *tuiConsole) cursorDownLocked() bool {
	L := t.layout(t.cols - len([]rune(stripANSI(t.prompt))))
	row := L.rowOf[t.pos]
	if row >= len(L.rows)-1 {
		return false
	}
	t.pos = posAtRowCol(L, row+1, L.colOf[t.pos])
	return true
}

// lineStart / lineEnd bound the logical (newline-delimited) line the cursor is
// in, for Ctrl-A / Ctrl-E in a multi-line buffer.
func lineStart(buf []rune, pos int) int {
	for i := pos - 1; i >= 0; i-- {
		if buf[i] == '\n' {
			return i + 1
		}
	}
	return 0
}

func lineEnd(buf []rune, pos int) int {
	for i := pos; i < len(buf); i++ {
		if buf[i] == '\n' {
			return i
		}
	}
	return len(buf)
}

// segments renders each buffer rune as its display string: masked, newline
// marks, snippet labels, or the rune itself.
func (t *tuiConsole) segments() []string {
	hi := t.mentionMask()
	segs := make([]string, len(t.buf))
	imgSeen := 0 // running count of image tokens, so display numbering is by appearance
	for i, r := range t.buf {
		switch {
		case t.mask:
			segs[i] = "*"
		case r == '\n':
			// A real break: layout starts a new row here, and the submit echo
			// shows the message on multiple lines.
			segs[i] = "\n"
		case r >= snippetBase && r < imageBase && int(r-snippetBase) < len(t.snippets):
			n := int(r - snippetBase)
			segs[i] = fmt.Sprintf("[snippet #%d: %d lines]", n+1, 1+strings.Count(t.snippets[n], "\n"))
		case r >= imageBase && int(r-imageBase) < len(t.images):
			imgSeen++
			segs[i] = fmt.Sprintf("[image-%d]", imgSeen)
		default:
			segs[i] = string(r)
		}
		if hi != nil && hi[i] {
			// Self-contained per rune, so the sliding window can never split a
			// color span; segWidth ignores the SGR so columns stay aligned.
			segs[i] = t.mention.sgr + segs[i] + ansiReset
		}
	}
	return segs
}

// mentionMask marks the buffer runes that fall inside a recognized #skill or
// @file token, or nil when there is nothing to highlight.
func (t *tuiConsole) mentionMask() []bool {
	if t.mention == nil || t.mask || t.mention.sgr == "" {
		return nil
	}
	spans := t.mention.spans(t.buf)
	if len(spans) == 0 {
		return nil
	}
	hi := make([]bool, len(t.buf))
	for _, s := range spans {
		for i := s[0]; i < s[1]; i++ {
			hi[i] = true
		}
	}
	return hi
}

func segWidth(s string) int {
	w := 0
	for _, r := range stripANSI(s) { // a segment may carry mention-highlight SGR
		w += runeWidth(r)
	}
	return w
}

// runeWidth is a minimal display-width table: wide for CJK, fullwidth forms,
// and emoji; 1 otherwise. Imperfect for grapheme clusters, but it keeps the
// cursor math from corrupting the input row.
func runeWidth(r rune) int {
	switch {
	case r >= 0x1100 && r <= 0x115F, // hangul jamo
		r >= 0x2E80 && r <= 0xA4CF, // CJK radicals..yi
		r >= 0xAC00 && r <= 0xD7A3, // hangul syllables
		r >= 0xF900 && r <= 0xFAFF, // CJK compat ideographs
		r >= 0xFE30 && r <= 0xFE4F, // CJK compat forms
		r >= 0xFF00 && r <= 0xFF60, // fullwidth forms
		r >= 0xFFE0 && r <= 0xFFE6,
		r >= 0x1F300 && r <= 0x1FAFF, // emoji
		r >= 0x20000 && r <= 0x3FFFD: // CJK ext B+
		return 2
	}
	return 1
}

func clipToWidth(s string, w int) string {
	out, used := make([]rune, 0, w), 0
	for _, r := range s {
		used += runeWidth(r)
		if used > w {
			break
		}
		out = append(out, r)
	}
	return string(out)
}

// stripANSI removes CSI sequences (colors, cursor moves) so prompt width can
// be measured. Non-CSI escapes pass through; we never put those in prompts.
func stripANSI(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == 0x1b && i+1 < len(s) && s[i+1] == '[' {
			j := i + 2
			for j < len(s) && (s[j] < '@' || s[j] > '~') {
				j++
			}
			i = j
			continue
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

func (t *tuiConsole) SetStatus(s string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.status = s
	if t.footer {
		fmt.Fprint(t.out, "\033[?2026h")
		t.removeFooterLocked()
		t.drawFooterLocked()
		fmt.Fprint(t.out, "\033[?2026l")
	}
}

// SetProcLine sets the footer's process row. Empty hides it. Like SetStatus,
// it redraws in place so a process appearing or finishing is reflected at once.
func (t *tuiConsole) SetProcLine(s string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if s == t.procStatus {
		return
	}
	t.procStatus = s
	if t.footer {
		fmt.Fprint(t.out, "\033[?2026h")
		t.removeFooterLocked()
		t.drawFooterLocked()
		fmt.Fprint(t.out, "\033[?2026l")
	}
}

// width is the current terminal column count, for fitting the process row.
func (t *tuiConsole) width() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.cols
}

// SetTitle reports turn progress through the terminal title (OSC 2), which
// tmux shows as the pane title: visible liveness with zero transcript noise.
func (t *tuiConsole) SetTitle(s string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	fmt.Fprintf(t.out, "\033]2;%s\007", s)
}

// ---------------------------------------------------------------------------
// Input.
// ---------------------------------------------------------------------------

// beginInput resets the editor and takes ownership of the input row; the
// footer itself persists between inputs.
func (t *tuiConsole) beginInput(prompt string, mask bool) {
	t.mu.Lock()
	// A turn can end while the user is mid-typing a steering message. Carry
	// that draft into the next prompt instead of discarding it: endInput and
	// the steer/stop paths already clear the buffer on submit, so a non-empty
	// one here uniquely identifies the carry-over. The buffer's snippet and
	// image tokens index into the slices, so they carry too. A buffer left
	// masked (a secret prompt) never carries: gating on the prior mask, not
	// the new one, stops a secret leaking into an ordinary prompt either way.
	if !t.mask && len(t.buf) > 0 {
		t.prompt, t.mask = prompt, mask
	} else {
		t.prompt, t.buf, t.pos, t.mask = prompt, nil, 0, mask
		t.snippets = nil
		t.images = nil
	}
	t.histIdx = len(t.hist)
	t.draft = nil
	t.winTop = 0
	t.refreshFooterLocked()
	t.mu.Unlock()
}

// endInput clears the editor, echoes what was entered into the transcript
// above the footer, and leaves the footer seated. Callers hold the mutex.
func (t *tuiConsole) endInput(echo string) {
	t.buf, t.pos = nil, 0
	t.removeFooterLocked()
	if echo != "" {
		t.writeLocked(echo + "\n")
	}
	t.drawFooterLocked()
}

// noteQueuedLocked drops a transcript line confirming a typed message was
// queued to steer the running turn, so the user sees it was captured rather than
// dropped. Cyan, not dim, because a steer interrupts the turn now and that is
// worth more than a muted aside. Caller holds the mutex.
func (t *tuiConsole) noteQueuedLocked(msg string) {
	if r := []rune(msg); len(r) > 60 {
		msg = string(r[:60]) + "..."
	}
	t.removeFooterLocked()
	t.writeLocked(fmt.Sprintf("%s  queued, steering now: %s%s\n", cyan, msg, reset))
	t.drawFooterLocked()
}

// pump is the one goroutine that reads the real terminal. Every keystroke
// becomes a rune on t.runes, so all other reads select on a channel and the
// editor can stay live during a turn. It runs for the life of the console;
// EOF closes the channel, which every consumer reads as end of input.
func (t *tuiConsole) pump() {
	for {
		r, _, err := t.in.ReadRune()
		if err != nil {
			close(t.runes)
			return
		}
		t.runes <- r
	}
}

// nextRune returns the next keystroke, or ok=false at end of input. ungot is a
// one-deep lookahead used by bare-Escape detection. Only the active consumer
// (always the main goroutine) calls this, so no lock is needed.
func (t *tuiConsole) nextRune() (rune, bool) {
	if n := len(t.ungot); n > 0 {
		r := t.ungot[n-1]
		t.ungot = t.ungot[:n-1]
		return r, true
	}
	r, ok := <-t.runes
	return r, ok
}

func (t *tuiConsole) unget(r rune) { t.ungot = append(t.ungot, r) }

// nextKey is the editor's read: like nextRune, but when done is non-nil it also
// returns the moment the turn ends (over=true), so the live editor stops
// attending without consuming a keystroke meant for the next prompt.
func (t *tuiConsole) nextKey(done <-chan struct{}) (r rune, over, eof bool) {
	if n := len(t.ungot); n > 0 {
		r = t.ungot[n-1]
		t.ungot = t.ungot[:n-1]
		return r, false, false
	}
	if done == nil {
		r, ok := <-t.runes
		return r, false, !ok
	}
	select {
	case r, ok := <-t.runes:
		return r, false, !ok
	case <-done:
		return 0, true, false
	}
}

// turnAttend wires the live editor to a running turn: done closes when the turn
// finishes, cancel aborts it (Escape), and queue stashes a typed message to
// steer the agent at the next iteration boundary.
type turnAttend struct {
	done   <-chan struct{}
	cancel func()
	queue  func(string)
}

// errTurnOver ends a live-editor attend when the turn finishes on its own.
var errTurnOver = fmt.Errorf("turn over")

func (t *tuiConsole) ReadLine(prompt string) (string, error) {
	return t.readLineMode(prompt, false, nil)
}

func (t *tuiConsole) ReadSecret(prompt string) (string, error) {
	return t.readLineMode(prompt, true, nil)
}

// attendTurn runs the editor live while a turn works: typing edits as usual,
// Escape cancels the turn, and Enter queues the message to steer. It returns
// when the turn ends (errTurnOver) or on EOF.
func (t *tuiConsole) attendTurn(ta turnAttend) error {
	_, err := t.readLineMode("-> ", false, &ta)
	return err
}

// readLineMode runs the line editor. With turn == nil it is the normal
// between-turns prompt. With turn set it attends a running turn: it reads keys
// the same way but Escape cancels the turn, Enter queues the typed text to steer
// instead of submitting, and it returns errTurnOver the instant the turn ends.
func (t *tuiConsole) readLine(prompt string, mask bool) (string, error) {
	return t.readLineMode(prompt, mask, nil)
}

func (t *tuiConsole) readLineMode(prompt string, mask bool, turn *turnAttend) (string, error) {
	if t.runes == nil { // a test-built console has no pump yet; the real one starts it in newTUI
		t.runes = make(chan rune, 1024)
		go t.pump()
	}
	t.beginInput(prompt, mask)
	var done <-chan struct{}
	if turn != nil {
		done = turn.done
	}
	for {
		r, over, eof := t.nextKey(done)
		if over {
			return "", errTurnOver // the turn finished; stop attending
		}
		t.mu.Lock()
		switch {
		case eof:
			if turn != nil { // Ctrl-D / EOF must not end the session mid-turn
				t.mu.Unlock()
				return "", errTurnOver
			}
			t.endInput("")
			t.mu.Unlock()
			return "", io.EOF
		case r == '\r' && turn != nil: // Enter while working: queue the steer and cancel now
			// A queued steer carries text only: images pasted into it are not sent
			// (per the feature's non-goal), but their labels still render so no raw
			// token rune leaks into the steer text.
			text, imgs := t.composeMessage()
			if line := strings.TrimSpace(text); strings.EqualFold(line, "/stop") {
				// /stop is the typed twin of Escape: abort the in-flight turn and
				// return to the prompt, but do NOT queue anything, so the model gets
				// no next turn. It is intercepted here because a line typed during a
				// turn never reaches the between-turns command dispatch.
				turn.cancel()
				t.removeFooterLocked()
				t.writeLocked(fmt.Sprintf("%s  stopping the current turn%s\n", cyan, reset))
				t.drawFooterLocked()
				t.buf, t.pos, t.snippets, t.images = nil, 0, nil, nil
			} else if line != "" {
				turn.queue(line)
				// Cancel the in-flight turn so the steer is acted on now, not at the
				// next natural boundary. The queued text is not lost: the worker's
				// runTurn returns a cancel error, the live loop drains the queue into
				// the next turn's input, and the cancel takes a different lock than
				// this one so holding the editor mutex here is safe.
				turn.cancel()
				t.noteQueuedLocked(line)
				if len(imgs) > 0 {
					t.removeFooterLocked()
					t.writeLocked(fmt.Sprintf("%s  images are not carried on a steer; resend them in a fresh message%s\n", dim, reset))
					t.drawFooterLocked()
				}
				t.buf, t.pos, t.snippets, t.images = nil, 0, nil, nil
			}
		case r == '\r': // Enter submits; Shift+Enter, Ctrl-J, and \+Enter newline
			if !mask && t.pos > 0 && t.buf[t.pos-1] == '\\' {
				// \ + Enter: a universal newline for terminals (tmux, Apple
				// Terminal, VTE without extended keys) that cannot tell
				// Shift+Enter from Enter at the byte level. The backslash is
				// consumed, bash-continuation style.
				t.buf[t.pos-1] = '\n'
				break
			}
			segs := t.segments()
			var shown strings.Builder
			for _, s := range segs {
				shown.WriteString(s)
			}
			text, imgs := t.composeMessage()
			line := strings.TrimSpace(text)
			t.submitImages = imgs // drained by takeImages after ReadLine returns
			t.endInput(t.prompt + shown.String())
			if !mask {
				t.hist = appendHistory(t.histPath, t.hist, line)
			}
			t.mu.Unlock()
			return line, nil
		case r == '\n': // Ctrl-J: newline everywhere, no protocol needed
			t.insertLocked('\n')
		case r == 0x03: // Ctrl-C: a raw 0x03 byte (terminals that do not send a signal)
			// Route to the same quit path as the signal and the CSI-u form. onCtrlC
			// must run without t.mu: its force-quit closes the console and the warning
			// Prints, both taking the console mutex, which would deadlock under t.mu.
			// Mirror the Ctrl+V / Escape branches that already drop the lock.
			t.mu.Unlock()
			if t.onCtrlC != nil {
				t.onCtrlC()
			}
			t.mu.Lock()
		case r == 0x04: // Ctrl-D on an empty line ends the session (not mid-turn)
			if turn == nil && len(t.buf) == 0 {
				t.endInput("")
				t.mu.Unlock()
				return "", io.EOF
			}
		case r == 0x7f || r == 0x08: // backspace: delete before the cursor
			if t.pos > 0 {
				t.buf = append(t.buf[:t.pos-1], t.buf[t.pos:]...)
				t.pos--
			}
		case r == 0x15: // Ctrl-U clears the line
			t.buf, t.pos = nil, 0
		case r == 0x0b: // Ctrl-K kills to end of line
			t.buf = t.buf[:t.pos]
		case r == 0x01: // Ctrl-A: start of the current logical line
			t.pos = lineStart(t.buf, t.pos)
		case r == 0x05: // Ctrl-E: end of the current logical line
			t.pos = lineEnd(t.buf, t.pos)
		case r == 0x16 && !mask: // Ctrl-V: paste an image off the clipboard
			// The capture shells out and writes to the transcript, so it must run
			// without the mutex (Print relocks it); mirror the Escape handler.
			t.mu.Unlock()
			t.captureImage()
			t.mu.Lock()
		case r == '\t':
			if !mask {
				t.completeLocked()
			}
		case r == ' ' && !mask: // a space closes an @file token: normalize it to its path
			t.normalizeLocked()
			t.insertLocked(' ')
		case r == 0x1b:
			t.mu.Unlock()
			if turn != nil && t.escIsBare() { // bare Escape cancels the running turn
				turn.cancel()
			} else {
				// onEscape fires only if the sequence decodes to the Escape KEY
				// itself (CSI-u mode delivers it as a sequence, not a bare 0x1b);
				// it cancels a live turn and is a no-op between turns.
				t.handleEscape(func() {
					if turn != nil {
						turn.cancel()
					}
				})
			}
			t.mu.Lock()
		case r >= 0x20: // printable, unicode included via ReadRune
			t.insertLocked(r)
		}
		if t.footer {
			t.refreshFooterLocked()
		}
		t.mu.Unlock()
	}
}

func (t *tuiConsole) insertLocked(r rune) {
	t.buf = append(t.buf[:t.pos], append([]rune{r}, t.buf[t.pos:]...)...)
	t.pos++
}

// expandSnippets returns the buffer with snippet tokens replaced by their
// full pasted content.
func (t *tuiConsole) expandSnippets() string {
	var b strings.Builder
	for _, r := range t.buf {
		if r >= snippetBase && r < imageBase && int(r-snippetBase) < len(t.snippets) {
			b.WriteString(t.snippets[int(r-snippetBase)])
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// captureImage runs the Ctrl-V pipeline: read the clipboard image, gate it on
// the active model's vision support, downscale and store it, then insert an
// atomic [image-N] token and print an honest note. It takes the mutex itself
// (the caller released it so the transcript writes can relock); every path that
// declines to insert states why, so a paste is never silently dropped.
func (t *tuiConsole) captureImage() {
	raw, _, err := readClipboardImage()
	if err != nil {
		t.note("can't paste image: " + err.Error())
		return
	}
	t.captureRaw(raw)
}

// captureImageQuiet is the Cmd+V empty-paste path: it captures an image only
// when one is actually on the clipboard and emits nothing otherwise, so a plain
// empty paste (no image) produces no output. Once an image is found it runs the
// same gating, store, and honest-note path as Ctrl+V.
func (t *tuiConsole) captureImageQuiet() {
	raw, _, err := readClipboardImage()
	if err != nil {
		return // no image (or no tool): stay silent, this was an ordinary empty paste
	}
	t.captureRaw(raw)
}

// captureRaw is the shared tail of both capture paths once raw clipboard bytes
// are in hand: vision gating, downscale, blob store, the atomic [image-N] token,
// and the honest note. It takes the mutex itself for the buffer edit (the caller
// released it so the transcript writes can relock).
func (t *tuiConsole) captureRaw(raw []byte) {
	if t.visionOK != nil && !t.visionOK() {
		t.note("the current model can't see images; switch with /model, or set \"vision\": true on the provider profile if it does")
		return
	}
	data, mediaType, w, h, err := decodeAndDownscale(raw)
	if err != nil {
		t.note("can't paste image: " + err.Error())
		return
	}
	hash, err := storeBlob(data, mediaType)
	if err != nil {
		t.note("can't paste image: " + err.Error())
		return
	}
	im := agent.Image{Hash: hash, MediaType: mediaType, Width: w, Height: h, Data: data}
	t.mu.Lock()
	t.images = append(t.images, im)
	t.insertLocked(imageBase + rune(len(t.images)-1))
	if t.footer {
		t.refreshFooterLocked()
	}
	t.mu.Unlock()
	dims := "unknown size"
	if w > 0 && h > 0 {
		dims = fmt.Sprintf("%dx%d", w, h)
	}
	t.note(fmt.Sprintf("image captured: %s %s, ~%d tokens",
		mediaType, dims, estimateImageTokens(w, h)))
}

// note drops a dim transcript line above the footer, the capture feedback
// counterpart to the snippet note. It takes the mutex itself.
func (t *tuiConsole) note(msg string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.removeFooterLocked()
	t.writeLocked(fmt.Sprintf("%s  %s%s\n", dim, msg, reset))
	t.drawFooterLocked()
}

// composeMessage walks the buffer into the text to send and the ordered images
// it carries: snippet tokens expand to their pasted content, image tokens append
// their image and write a [image-K] label (numbered by appearance, matching what
// segments() drew), and every other rune passes through. An image token never
// leaks into the text as a raw private-use rune.
func (t *tuiConsole) composeMessage() (string, []agent.Image) {
	var b strings.Builder
	var imgs []agent.Image
	for _, r := range t.buf {
		switch {
		case r >= snippetBase && r < imageBase && int(r-snippetBase) < len(t.snippets):
			b.WriteString(t.snippets[int(r-snippetBase)])
		case r >= imageBase && int(r-imageBase) < len(t.images):
			imgs = append(imgs, t.images[int(r-imageBase)])
			fmt.Fprintf(&b, "[image-%d]", len(imgs))
		default:
			b.WriteRune(r)
		}
	}
	return b.String(), imgs
}

// takeImages returns the images of the last submitted message and clears them,
// so the next submit starts empty. The repl pulls them after ReadLine to attach
// to the user Turn.
func (t *tuiConsole) takeImages() []agent.Image {
	imgs := t.submitImages
	t.submitImages = nil
	return imgs
}

// handleEscape reads one escape sequence and applies its editing action:
// arrows move the cursor or walk history, home/end/delete edit, Shift+Enter
// inserts a newline (CSI 13;2u from the Kitty disambiguation flag, or CSI
// 27;2;13~ from terminals/tmux in extended-keys mode), and a bracketed-paste
// begin marker pulls the whole paste into the buffer. Under extended-keys mode
// plain Enter also arrives here as a CSI-u sequence (codepoint 13 or kitty
// functional 57414): an unmodified one is normalized back to a plain Return and
// ungot so readLineMode's single submit/steer path handles it, while a modified
// one inserts a newline. Alt+V (ESC then a plain v) is the Ctrl+V fallback for
// terminals like Windows Terminal that swallow Ctrl+V for their own paste; it
// runs the same image-capture pipeline.
//
// onEscape, when set, is invoked instead of any editing action if the sequence
// decodes to the Escape KEY itself: under extended-keys (CSI-u) mode the
// Escape key arrives as a sequence (CSI 27 u), not a bare 0x1b, so the bare
// path in readLineMode never sees it. Routing it here keeps a single consumer
// of the sequence bytes (no double-read) while still reaching cancel.
func (t *tuiConsole) handleEscape(onEscape func()) {
	// Peek the byte after ESC: a plain v is Alt+V, not a CSI/SS3 introducer, so
	// route it to the capture pipeline before the sequence decoder. The capture
	// writes to the transcript (Print relocks), so it must run unlocked, exactly
	// like the Ctrl+V case; the caller already released the mutex for handleEscape.
	if r, more := t.nextRune(); more {
		if r == 'v' || r == 'V' {
			t.captureImage()
			return
		}
		t.unget(r) // not Alt+V: hand the introducer back to the CSI/SS3 decoder
	}
	params, final, ok := t.readCSI()
	if !ok {
		return
	}
	if final == '~' && params == "200" { // bracketed paste
		content := t.readPaste()
		if len(content) == 0 {
			// macOS Cmd+V often delivers an image paste as an empty bracketed
			// paste. Try a quiet image capture: it acts only when an image is
			// actually on the clipboard, so an ordinary empty paste stays silent.
			t.captureImageQuiet()
			return
		}
		t.mu.Lock()
		t.insertPasteLocked(content)
		t.mu.Unlock()
		return
	}
	if isCSIUEscape(params, final) {
		// The Escape KEY under extended-keys mode. It must reach cancel exactly
		// like a bare 0x1b; with no live turn it is a harmless no-op (the bytes
		// are already consumed, so nothing strays into the buffer).
		if onEscape != nil {
			onEscape()
		}
		return
	}
	if isCSIUCtrlC(params, final) {
		// Ctrl-C under extended-keys mode arrives here as a keystroke, never as an
		// OS signal, so it must reach the same quit path the signal handler uses.
		// onCtrlC is called WITHOUT t.mu held (this branch runs before the lock
		// below): its force-quit closes the console and the first-press warning
		// Prints, both of which take the console mutex and would deadlock under t.mu.
		if t.onCtrlC != nil {
			t.onCtrlC()
		}
		return
	}
	if enter, mod := isCSIUEnter(params, final); enter {
		// Under extended-keys / kitty mode the Enter key is reported as a CSI-u
		// sequence (codepoint 13 or functional 57414), never a bare \r, so the
		// bare submit/steer path in readLineMode never sees it. Normalize an
		// unmodified Enter back to a plain \r and unget it: readLineMode's next
		// loop iteration reads it from t.ungot and runs the single submit/steer
		// path unchanged. A modified Enter (Shift/Alt) inserts a newline instead,
		// matching Ctrl-J and the kitty 13;2 / 13;3 forms below.
		if mod {
			t.mu.Lock()
			t.insertLocked('\n')
			t.mu.Unlock()
		} else {
			t.unget('\r')
		}
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	switch {
	case final == '~' && (params == "27;2;13" || params == "27;3;13"): // shift/alt+enter, the legacy '~' form (the kitty 'u' form is decoded above)
		t.insertLocked('\n')
	case final == 'A': // up: move a visual row, else walk history at the top
		if !t.cursorUpLocked() {
			t.histMoveLocked(-1)
		}
	case final == 'B': // down: move a visual row, else walk history at the bottom
		if !t.cursorDownLocked() {
			t.histMoveLocked(+1)
		}
	case final == 'C': // right
		if t.pos < len(t.buf) {
			t.pos++
		}
	case final == 'D': // left
		if t.pos > 0 {
			t.pos--
		}
	case final == 'H' || (final == '~' && params == "1"): // home
		t.pos = 0
	case final == 'F' || (final == '~' && params == "4"): // end
		t.pos = len(t.buf)
	case final == '~' && params == "3": // delete at cursor
		if t.pos < len(t.buf) {
			t.buf = append(t.buf[:t.pos], t.buf[t.pos+1:]...)
		}
	}
}

// isCSIUEscape reports whether a decoded sequence (params, final) is the Escape
// KEY under extended-keys / CSI-u mode. There the key is reported by its Unicode
// codepoint in the first parameter field, and Escape is codepoint 27: bare as
// CSI 27 u, or with a modifier as CSI 27;<mods> u. Keying on the codepoint, not
// just the final 'u', is load-bearing: extended Enter is also final 'u' but
// codepoint 13 (CSI 13;2u), and must keep inserting a newline rather than cancel.
func isCSIUEscape(params string, final rune) bool {
	return final == 'u' && (params == "27" || strings.HasPrefix(params, "27;"))
}

// isCSIUCtrlC reports whether a decoded sequence is the Ctrl-C key under
// extended-keys / CSI-u mode. The key is codepoint 99 ('c') with a Ctrl
// modifier, reported as CSI 99;5 u (modifier 5 == Ctrl). Both the codepoint and
// the modifier are load-bearing: unmodified 'c' is CSI 99 u or CSI 99;1 u and
// must keep typing the letter, not quit, so a bare or modifier-1 form is excluded.
func isCSIUCtrlC(params string, final rune) bool {
	if final != 'u' {
		return false
	}
	cp, mods, hasMods := strings.Cut(params, ";")
	if cp != "99" || !hasMods {
		return false
	}
	// The modifier field is the bitmask + 1; Ctrl is bit 4, so any reported value
	// with that bit set (e.g. 5 = Ctrl, 7 = Ctrl+Alt) counts. Modifier 1 is "none".
	m, err := strconv.Atoi(mods)
	return err == nil && (m-1)&4 != 0
}

// isCSIUEnter reports whether a decoded sequence is the Enter key under
// extended-keys / CSI-u mode, and whether a modifier is held. Enter is reported
// by codepoint in the first field: standard codepoint 13, or kitty's functional
// code 57414 (0xE006). modified is true when a modifier field is present and is
// not "1" ("none"): Shift+Enter and Alt+Enter (e.g. 13;2, 57414;3) are modified
// and must insert a newline, while bare 13/57414 or explicit 13;1/57414;1 are
// unmodified and must submit. The codepoint check excludes Escape (27) and
// Ctrl-C (99), which also end in 'u'.
func isCSIUEnter(params string, final rune) (enter, modified bool) {
	if final != 'u' {
		return false, false
	}
	cp, mods, hasMods := strings.Cut(params, ";")
	if cp != "13" && cp != "57414" {
		return false, false
	}
	return true, hasMods && mods != "1"
}

// snippet thresholds: pastes beyond either collapse to an atomic token so the
// input row stays readable; the full content is sent on submit.
const (
	snippetLines = 3
	snippetChars = 200
)

// insertPasteLocked inserts pasted content: small pastes inline, large ones
// as a snippet token. A dim capture note goes to the transcript so the user
// can verify what was grabbed without it bloating the input row.
func (t *tuiConsole) insertPasteLocked(content []rune) {
	text := string(content)
	lines := 1 + strings.Count(text, "\n")
	if lines <= snippetLines && len(content) <= snippetChars {
		for _, r := range content {
			t.insertLocked(r)
		}
		return
	}
	t.snippets = append(t.snippets, text)
	t.insertLocked(snippetBase + rune(len(t.snippets)-1))
	first := firstLine(text)
	if len(first) > 60 {
		first = first[:60] + "..."
	}
	note := fmt.Sprintf("%s  snippet #%d captured: %d lines, %d bytes (starts: %q)%s",
		dim, len(t.snippets), lines, len(text), first, reset)
	t.removeFooterLocked()
	t.writeLocked(note + "\n")
	t.drawFooterLocked()
}

// readCSI consumes the remainder of an escape sequence after ESC was read,
// returning its parameter bytes and final byte. ok=false for a bare escape
// or read error; unknown sequences still return so callers can ignore them.
func (t *tuiConsole) readCSI() (params string, final rune, ok bool) {
	r, more := t.nextRune()
	if !more || (r != '[' && r != 'O') {
		return "", 0, false
	}
	var p []rune
	for {
		c, more := t.nextRune()
		if !more {
			return "", 0, false
		}
		if c >= '@' && c <= '~' {
			return string(p), c, true
		}
		p = append(p, c)
	}
}

// readPaste collects bracketed-paste content until the end marker, turning
// carriage returns into newlines so a multiline paste is one message.
func (t *tuiConsole) readPaste() []rune {
	var content []rune
	for {
		r, more := t.nextRune()
		if !more {
			return content
		}
		if r == 0x1b {
			if params, final, ok := t.readCSI(); ok && final == '~' && params == "201" {
				return content
			}
			continue // discard any other sequence inside a paste
		}
		if r == '\r' {
			r = '\n'
		}
		content = append(content, r)
	}
}

// histMoveLocked walks the per-project input history; the live draft is
// stashed on the way up and restored at the bottom.
func (t *tuiConsole) histMoveLocked(delta int) {
	if len(t.hist) == 0 || t.mask {
		return
	}
	idx := t.histIdx + delta
	if idx < 0 || idx > len(t.hist) {
		return
	}
	if t.histIdx == len(t.hist) && idx < len(t.hist) {
		t.draft = append([]rune(nil), t.buf...)
	}
	t.histIdx = idx
	if idx == len(t.hist) {
		t.buf = append([]rune(nil), t.draft...)
	} else {
		t.buf = []rune(t.hist[idx])
	}
	t.pos = len(t.buf)
}

// completeLocked extends the buffer to the longest common prefix of the
// repl-provided completions; when several remain and no progress was made,
// they are listed in the transcript above the footer.
func (t *tuiConsole) completeLocked() {
	// A #skill or @file token under the cursor completes in place, anywhere in
	// the line; everything else falls back to whole-line command completion.
	if t.mention != nil {
		if start, tok, ok := mentionToken(t.buf, t.pos); ok {
			if cands := t.mention.complete(tok); len(cands) > 0 {
				t.completeRangeLocked(start, t.pos, cands)
				return
			}
		}
	}
	if t.completer == nil || t.pos != len(t.buf) {
		return
	}
	if cands := t.completer(string(t.buf)); len(cands) > 0 {
		t.completeRangeLocked(0, len(t.buf), cands)
	}
}

// completeRangeLocked extends buf[start:end] to the longest common prefix of the
// candidates; when that makes no progress and several remain, it lists them dim
// above the footer.
func (t *tuiConsole) completeRangeLocked(start, end int, cands []string) {
	lcp := cands[0]
	for _, c := range cands[1:] {
		lcp = commonPrefix(lcp, c)
	}
	if repl := []rune(lcp); len(repl) > end-start {
		t.buf = append(t.buf[:start], append(repl, t.buf[end:]...)...)
		t.pos = start + len(repl)
		return
	}
	if len(cands) > 1 {
		shown := cands
		if len(shown) > 8 {
			shown = append(append([]string{}, shown[:8]...), "…")
		}
		t.removeFooterLocked()
		t.writeLocked(fmt.Sprintf("%s  %s%s\n", dim, strings.Join(shown, "  "), reset))
		t.drawFooterLocked()
	}
}

// normalizeLocked rewrites the @file token ending at the cursor to its
// working-directory relative path, the moment a space closes it. A #skill or an
// unresolved/ambiguous @name is left exactly as typed.
func (t *tuiConsole) normalizeLocked() {
	if t.mention == nil {
		return
	}
	start, tok, ok := mentionToken(t.buf, t.pos)
	if !ok || len(tok) < 2 || tok[0] != '@' {
		return
	}
	rep, ok := t.mention.resolve(tok)
	if !ok || rep == tok {
		return
	}
	repR := []rune(rep)
	t.buf = append(t.buf[:start], append(repR, t.buf[t.pos:]...)...)
	t.pos = start + len(repR)
}

// ---------------------------------------------------------------------------
// Select: an arrow-key picker drawn where the footer sits, so navigating it
// never touches scrollback. Up/down move, enter executes, 1-9 jump-select,
// q or a bare Esc cancels.
// ---------------------------------------------------------------------------

const pickerWindow = 8

func (t *tuiConsole) Select(title string, items []string, current int) (int, error) {
	if len(items) == 0 {
		return -1, nil
	}
	t.mu.Lock()
	had := t.footer
	t.removeFooterLocked()
	pad := t.col > 0
	if pad {
		fmt.Fprint(t.out, "\n")
	}
	t.mu.Unlock()

	sel := current
	if sel < 0 || sel >= len(items) {
		sel = 0
	}
	itemRows := min(len(items), pickerWindow)
	totalRows := itemRows + 3 // divider, items, divider, hint
	drawn := false

	draw := func() {
		t.mu.Lock()
		defer t.mu.Unlock()
		fmt.Fprint(t.out, "\033[?2026h")
		if drawn {
			fmt.Fprintf(t.out, "\033[%dA", totalRows-1)
		}
		start := 0
		if sel >= start+itemRows {
			start = sel - itemRows + 1
		}
		div := dim + strings.Repeat("─", t.cols) + reset
		rows := []string{div}
		for i := start; i < start+itemRows; i++ {
			label := clipToWidth(items[i], t.cols-5)
			switch {
			case i == sel:
				rows = append(rows, fmt.Sprintf("%s ❯ %s%s", yellow, label, reset))
			default:
				rows = append(rows, fmt.Sprintf("   %s%s%s", dim, label, reset))
			}
		}
		more := ""
		if len(items) > itemRows {
			more = fmt.Sprintf(" · %d/%d", sel+1, len(items))
		}
		rows = append(rows, div, fmt.Sprintf("%s%s · ↑/↓ move · enter select · q cancels%s%s", dim, title, more, reset))
		for i, row := range rows {
			fmt.Fprint(t.out, "\r\033[2K"+row)
			if i < len(rows)-1 {
				fmt.Fprint(t.out, "\n")
			}
		}
		drawn = true
		fmt.Fprint(t.out, "\033[?2026l")
	}

	finish := func(idx int) (int, error) {
		t.mu.Lock()
		defer t.mu.Unlock()
		// clear the picker rows and return to the content tail
		fmt.Fprintf(t.out, "\033[%dA\r\033[0J", totalRows-1)
		if pad {
			fmt.Fprint(t.out, "\033[1A\r")
			if vc := t.visualCol(); vc > 0 {
				fmt.Fprintf(t.out, "\033[%dC", vc)
			}
		}
		if had {
			t.drawFooterLocked()
		}
		return idx, nil
	}

	draw()
	for {
		r, ok := t.nextRune()
		if !ok {
			return finish(-1)
		}
		switch {
		case r == '\r' || r == '\n':
			return finish(sel)
		case r == 'q' || r == 'Q' || r == 0x03:
			return finish(-1)
		case r >= '1' && r <= '9':
			if idx := int(r - '1'); idx < len(items) {
				return finish(idx)
			}
		case r == 'k': // vim up
			if sel > 0 {
				sel--
			}
			draw()
		case r == 'j': // vim down
			if sel < len(items)-1 {
				sel++
			}
			draw()
		case r == 0x1b:
			if t.escIsBare() {
				return finish(-1)
			}
			params, final, ok := t.readCSI()
			if !ok {
				return finish(-1)
			}
			_ = params
			switch final {
			case 'A':
				if sel > 0 {
					sel--
				}
				draw()
			case 'B':
				if sel < len(items)-1 {
					sel++
				}
				draw()
			}
		}
	}
}

// escIsBare reports whether an Esc keypress arrived alone (the user pressing the
// key to cancel) rather than as the lead byte of an escape sequence. It decides
// by the byte that FOLLOWS Esc, not by timing: a CSI or SS3 sequence's introducer
// ([ or O) arrives contiguously behind Esc, while a bare Escape is followed by
// nothing or by an unrelated keystroke. Keying off the introducer is what makes
// this robust under tmux/SSH, where tmux's escape-time can delay a lone Esc past
// any tight timer and a presence-based test would misread it as a sequence. A
// peeked introducer is put back for handleEscape; a peeked non-introducer is put
// back so it is processed as the user's next keystroke.
func (t *tuiConsole) escIsBare() bool {
	if len(t.ungot) > 0 {
		// A pushed-back rune only exists because a prior peek read it; treat its
		// value the same as a freshly peeked byte so a non-introducer still reads
		// as bare.
		r := t.ungot[len(t.ungot)-1]
		return r != '[' && r != 'O'
	}
	select {
	case r, ok := <-t.runes:
		if ok {
			t.unget(r)
			return r != '[' && r != 'O' // only [ (CSI) and O (SS3) introduce a sequence
		}
		return true // a closed stream after Esc counts as bare
	case <-time.After(25 * time.Millisecond):
		// No byte arrived contiguously: a real sequence's introducer would already
		// be here, so this was the bare key. The window only needs to cover the
		// contiguous burst, not tmux's delay before the lone Esc reaches us.
		return true
	}
}

func commonPrefix(a, b string) string {
	ar, br := []rune(a), []rune(b)
	n := min(len(ar), len(br))
	i := 0
	for i < n && ar[i] == br[i] {
		i++
	}
	return string(ar[:i])
}

// ReadKey reads a single keypress for approval gates. The prompt prints
// inline in the transcript (gates happen mid-turn), with the footer lifted
// for the question and re-seated after; escape sequences read as a decline.
func (t *tuiConsole) ReadKey(prompt string) (byte, error) {
	t.mu.Lock()
	had := t.footer
	t.removeFooterLocked()
	t.writeLocked(prompt)
	t.mu.Unlock()
	r, ok := t.nextRune()
	if ok && r == 0x1b {
		t.readCSI()
		r = 'n'
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if !ok {
		t.writeLocked("\n")
		return 0, io.EOF
	}
	t.writeLocked(fmt.Sprintf("%c\n", r))
	if had {
		t.drawFooterLocked()
	}
	return byte(r), nil
}

func (t *tuiConsole) Close() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.removeFooterLocked()
	fmt.Fprint(t.out, "\033[<u")     // pop the Kitty keyboard mode
	fmt.Fprint(t.out, "\033[?2004l") // bracketed paste off
	fmt.Fprint(t.out, "\033]2;\007") // clear the title
	stty("icanon", "echo", "icrnl")
}
