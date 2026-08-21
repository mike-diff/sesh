// Shaping tool output for the context window. A tool result that exceeds the
// budget keeps its head AND its tail, with the middle elided and the full text
// spilled to a file the model can read back.
//
// Both ends are load-bearing, which one-ended truncation cannot serve: a failing
// command puts its diagnostic anywhere in the output (measured: 51% in, for a
// verbose test run with one failure) but its verdict on the last line (measured:
// 30 bytes from the end). Cutting either end alone destroys one of the two, and
// keeping only the head is worse than losing information: a truncated run of a
// FAILING suite reads as an unbroken wall of PASS lines, which steers the drive
// judge toward done.
//
// The middle is recoverable rather than trusted, the same bargain the handoff
// makes: the pointer names the elided line range, so read's existing offset
// paging lands directly on it. The shapes here are the ones the harness already
// uses elsewhere (a tail-biased log with its elision reported, in proc; a
// head-plus-tail cut with an elided middle, in renderTranscript).
package harness

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/mike-diff/sesh/agent"
)

func outputsDir() string { return filepath.Join(os.Getenv("HOME"), ".sesh", "out") }

// outputDir is where one conversation's spilled output lives. It is keyed by
// the CHAIN root, not the session id: a handoff renames nothing here, so a
// pointer written before the boundary still resolves after it. Session.Root is
// empty until the first handoff, and that handoff sets the successor's Root to
// this id, so the key is the same on both sides of every boundary.
func outputDir(s *Session) string {
	key := s.Root
	if key == "" {
		key = s.ID
	}
	return filepath.Join(outputsDir(), key)
}

// outStore hands out the spill files for one conversation. Allocation is locked
// because a reply's parallel tool calls (and concurrent task subagents) shape
// their results at the same time.
type outStore struct {
	mu  sync.Mutex
	dir string
	seq int
}

// spill is the live store, resolved once at startup like the tuning dials. Nil
// means shaping still trims but nothing is written, which is what the bench rig
// and doctor run with.
var spill *outStore

// newOutStore resumes the id sequence from what is already on disk, so a
// resumed session does not hand out ids that overwrite the previous run's
// files. A dir it cannot read yields an empty store rather than an error:
// failing to spill must degrade to plain trimming, never break a tool call.
func newOutStore(dir string) *outStore {
	o := &outStore{dir: dir}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return o
	}
	for _, e := range entries {
		name := strings.TrimSuffix(e.Name(), ".log")
		n, err := strconv.Atoi(strings.TrimPrefix(name, "out-"))
		if err == nil && n > o.seq {
			o.seq = n
		}
	}
	return o
}

// put writes s to a fresh file and returns its path. The write is atomic
// write-then-rename, like the blob store, so a crash mid-spill cannot leave a
// truncated file behind a pointer that claims it is complete.
func (o *outStore) put(s string) (string, error) {
	o.mu.Lock()
	o.seq++
	path := filepath.Join(o.dir, fmt.Sprintf("out-%d.log", o.seq))
	o.mu.Unlock()
	if err := os.MkdirAll(o.dir, 0o700); err != nil {
		return "", err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(s), 0o600); err != nil {
		return "", err
	}
	if err := os.Rename(tmp, path); err != nil {
		return "", err
	}
	return path, nil
}

// headCut returns the largest offset within budget that ends a line, so the
// kept head is whole lines. A single line longer than the budget has no
// boundary to find and is cut mid-line: showing part of it beats showing none.
func headCut(s string, budget int) int {
	if budget >= len(s) {
		return len(s)
	}
	if i := strings.LastIndexByte(s[:budget], '\n'); i >= 0 {
		return i + 1
	}
	return budget
}

// tailCut returns the smallest offset at or after len(s)-budget that starts a
// line, the mirror of headCut.
func tailCut(s string, budget int) int {
	if budget >= len(s) {
		return 0
	}
	start := len(s) - budget
	if i := strings.IndexByte(s[start:], '\n'); i >= 0 {
		return start + i + 1
	}
	return start
}

// shape bounds one tool result to the configured budget. Under it, the string
// is returned untouched. Over it, the head and tail are kept around an elided
// middle, and the note between them says how much was dropped and where the
// full text is.
func shape(s string) string {
	// Masking runs FIRST, before any copy is kept: the shaped ends the model
	// sees, and the spilled file it can read back, must both hold the masked
	// text. Masking after the spill would persist the secret on disk; masking
	// only the model's copy would leave the read-back path leaky.
	if !tune.ResultMaskOff {
		s = maskSecrets(s)
	}
	max := tune.ResultMaxChars
	if max <= 0 || len(s) <= max {
		return s
	}
	// The note sits inside the budget, so the shaped result never exceeds it
	// however long the spill path turns out to be.
	const noteReserve = 240
	body := max - noteReserve
	if body < 512 { // a budget too small to say anything useful about: trim only
		return s[:max]
	}
	head := body * tune.ResultHeadPct / 100
	h := headCut(s, head)
	t := tailCut(s, body-h)
	if t <= h { // nothing actually elided; the cuts met
		return s[:max]
	}

	total := lineCount(s)
	first := strings.Count(s[:h], "\n") + 1
	last := total - lineCount(s[t:])

	note := fmt.Sprintf("lines %d-%d of %d elided (%d bytes)", first, last, total, t-h)
	if path, err := spillFull(s); err == nil {
		note += fmt.Sprintf("; full output: %s (read it with offset %d)", path, first)
	}
	return s[:h] + "\n... [" + note + "]\n" + s[t:]
}

// spillFull writes the untrimmed result out of line. It reports an error when
// spilling is off or unavailable, in which case the shaped result carries the
// elided range without a path: the model is still told what it is missing.
func spillFull(s string) (string, error) {
	if spill == nil || tune.ResultSpillOff {
		return "", os.ErrNotExist
	}
	return spill.put(s)
}

// shaped wraps a tool so its result is bounded before it reaches the context
// window. Applied at assembly to the tools whose output size the harness does
// not otherwise control: bash, the engines, and tool mods. The tools that
// already shape themselves (read's paging, search's suppression, proc's
// tail-biased logs, edit and write's bounded diff) are deliberately left alone,
// so there is exactly one shaping policy per result.
func shaped(t agent.Tool) agent.Tool {
	inner := t.Run
	t.Run = func(ctx context.Context, raw json.RawMessage) (string, bool) {
		out, isErr := inner(ctx, raw)
		return shape(out), isErr
	}
	return t
}

// outputGCMinAge is how long a conversation's spilled output outlives its last
// write before gcOutput will collect it. The floor is generous because the cost
// of collecting too early (a pointer in a resumed transcript stops resolving)
// is paid by the model, while the cost of collecting too late is disk.
func outputGCMinAge() time.Duration {
	return time.Duration(tune.ResultKeepDays) * 24 * time.Hour
}

// gcOutput removes spilled output for conversations that have been idle past
// the age floor, keyed by directory rather than by scanning transcripts for
// pointers: a live session writes as it works, so its directory is never stale.
// Conservative like the blob sweep: every error is skipped, the current
// conversation is never touched, and a recent directory is always kept. A
// collected pointer degrades into a read error the model can act on, which is
// why erring toward collection is safe here.
func gcOutput(keep string) {
	entries, err := os.ReadDir(outputsDir())
	if err != nil {
		return // nothing spilled yet, or unreadable
	}
	cutoff := time.Now().Add(-outputGCMinAge())
	for _, e := range entries {
		if !e.IsDir() || e.Name() == keep {
			continue
		}
		dir := filepath.Join(outputsDir(), e.Name())
		if newest(dir).After(cutoff) {
			continue // still in use, or recently was
		}
		os.RemoveAll(dir) // best-effort; an error just leaves the directory
	}
}

// newest reports the most recent modification time in dir, the directory's own
// included, so a directory whose files were all deleted still ages out.
func newest(dir string) time.Time {
	info, err := os.Stat(dir)
	if err != nil {
		return time.Now() // unreadable: treat as fresh and leave it alone
	}
	latest := info.ModTime()
	entries, err := os.ReadDir(dir)
	if err != nil {
		return latest
	}
	for _, e := range entries {
		if fi, err := e.Info(); err == nil && fi.ModTime().After(latest) {
			latest = fi.ModTime()
		}
	}
	return latest
}

// outputUsage totals the spilled output on disk: how many conversations have
// any, and how many bytes they hold. Reporting only; every error is skipped,
// because doctor must describe what it can reach rather than fail on a
// directory it cannot.
func outputUsage() (conversations int, bytes int64) {
	entries, err := os.ReadDir(outputsDir())
	if err != nil {
		return 0, 0
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		files, err := os.ReadDir(filepath.Join(outputsDir(), e.Name()))
		if err != nil {
			continue
		}
		var sub int64
		for _, f := range files {
			if fi, err := f.Info(); err == nil {
				sub += fi.Size()
			}
		}
		if sub > 0 {
			conversations++
			bytes += sub
		}
	}
	return conversations, bytes
}

// humanBytes renders a size the way a person reads it, for doctor's report.
func humanBytes(n int64) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KB", float64(n)/(1<<10))
	default:
		return fmt.Sprintf("%d B", n)
	}
}
