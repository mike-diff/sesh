package harness

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// failingRun is a stand-in for the output that motivated the shaper: a long
// verbose test run whose diagnostic sits in the middle and whose verdict is the
// last line. A one-ended cut loses exactly one of the two.
func failingRun(t *testing.T) string {
	t.Helper()
	var b strings.Builder
	for i := 1; i <= 400; i++ {
		fmt.Fprintf(&b, "=== RUN   TestAlpha%d\n--- PASS: TestAlpha%d (0.00s)\n", i, i)
	}
	b.WriteString("    a_test.go:403: boom: nil map write at pkg/store.go:88\n")
	for i := 1; i <= 400; i++ {
		fmt.Fprintf(&b, "=== RUN   TestBeta%d\n--- PASS: TestBeta%d (0.00s)\n", i, i)
	}
	b.WriteString("FAIL\nFAIL\tsigprobe/pkg\t0.013s\n")
	return b.String()
}

// withSpill points the store at a real ~/.sesh/out location under a temp HOME,
// so the confinement carve-out is exercised for real rather than bypassed by a
// bare temp dir.
func withSpill(t *testing.T) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	prev := spill
	spill = newOutStore(outputDir(&Session{ID: "testchain"}))
	t.Cleanup(func() { spill = prev })
}

// A head-only cut keeps the PASS wall and drops the verdict, which is how a
// failing run came to read as a passing one. Both ends must survive.
func TestShapeKeepsDiagnosticAndVerdict(t *testing.T) {
	withSpill(t)
	full := failingRun(t)
	if len(full) <= tune.ResultMaxChars {
		t.Fatalf("fixture must exceed the budget: %d <= %d", len(full), tune.ResultMaxChars)
	}
	got := shape(full)
	if !strings.Contains(got, "FAIL\tsigprobe/pkg") {
		t.Error("shaped result lost the verdict on the final line")
	}
	if !strings.Contains(got, "boom: nil map write") {
		t.Error("shaped result lost the mid-output diagnostic")
	}
}

func TestShapeRespectsBudget(t *testing.T) {
	withSpill(t)
	for _, n := range []int{tune.ResultMaxChars + 1, 200_000, 1 << 20} {
		got := shape(strings.Repeat("x y z\n", n/6))
		if len(got) > tune.ResultMaxChars {
			t.Errorf("input %d: shaped to %d, over the %d budget", n, len(got), tune.ResultMaxChars)
		}
	}
}

func TestShapeLeavesSmallOutputUntouched(t *testing.T) {
	withSpill(t)
	s := "ok\tgithub.com/mike-diff/sesh/harness\t8.0s\n"
	if got := shape(s); got != s {
		t.Errorf("under-budget output was modified:\n%q", got)
	}
}

// The pointer is only useful if the read tool accepts it. Before the carve-out
// every path under ~/.sesh was refused, so the pointer was a dead end.
func TestShapePointerIsReadable(t *testing.T) {
	withSpill(t)
	path, err := spill.put(failingRun(t))
	if err != nil {
		t.Fatalf("spill: %v", err)
	}
	if refusal := confineRead(path, false); refusal != "" {
		t.Fatalf("spilled output must be readable, got refusal: %s", refusal)
	}
	// Mutation must stay refused: only the observation side is carved out.
	if confine(path, false) == "" {
		t.Error("spilled output must not be writable")
	}
}

// The pointer promises an offset. Feeding it back through doRead must land on
// the lines the shaper said it elided.
func TestShapePointerOffsetLandsOnElidedLines(t *testing.T) {
	withSpill(t)
	full := failingRun(t)
	got := shape(full)

	path := betweenPointer(t, got, "full output: ", " (read it with offset ")
	offset := numberAfter(t, got, " (read it with offset ")

	window, isErr := doRead(path, false, offset, 1)
	if isErr {
		t.Fatalf("reading the spilled output failed: %s", window)
	}
	first := strings.SplitN(window, "\n", 2)[0]
	// The line at the promised offset must be the first line NOT in the head.
	head := got[:strings.Index(got, "\n... [")]
	if strings.Contains(head, first) {
		t.Errorf("offset %d landed inside the kept head, not the elided middle:\n%q", offset, first)
	}
	if !strings.Contains(full, first) {
		t.Errorf("offset %d returned a line absent from the full output: %q", offset, first)
	}
}

// Spilling off must still tell the model what it is missing.
func TestShapeWithoutSpillStillReportsElision(t *testing.T) {
	withSpill(t)
	prev := tune.ResultSpillOff
	tune.ResultSpillOff = true
	defer func() { tune.ResultSpillOff = prev }()

	got := shape(failingRun(t))
	if strings.Contains(got, "full output:") {
		t.Error("spill is off; the result must not advertise a path")
	}
	if !strings.Contains(got, "elided") {
		t.Error("the result must still report that output was elided")
	}
}

// A conversation's key must not move when it hands off, or every pointer
// already in the transcript stops resolving.
func TestOutputDirSurvivesHandoff(t *testing.T) {
	s := &Session{ID: "gen0", Cwd: t.TempDir()}
	want := outputDir(s)
	for i := 1; i <= 3; i++ {
		s = seedChain(s, "brief", "ledger", "mech", nil)
		if got := outputDir(s); got != want {
			t.Fatalf("hop %d moved the output dir: %s != %s", i, got, want)
		}
	}
}

// A reply's parallel tool calls and concurrent subagents shape at once.
func TestOutStoreAllocatesDistinctIDsConcurrently(t *testing.T) {
	o := newOutStore(t.TempDir())
	const n = 8
	paths := make([]string, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			p, err := o.put("body " + strconv.Itoa(i))
			if err != nil {
				t.Errorf("put: %v", err)
				return
			}
			paths[i] = p
		}(i)
	}
	wg.Wait()
	seen := map[string]bool{}
	for _, p := range paths {
		if p == "" {
			continue
		}
		if seen[p] {
			t.Errorf("duplicate spill path handed out: %s", p)
		}
		seen[p] = true
	}
	if len(seen) != n {
		t.Errorf("got %d distinct paths, want %d", len(seen), n)
	}
}

// A resumed conversation must not hand out ids that overwrite the last run.
func TestOutStoreResumesSequence(t *testing.T) {
	dir := t.TempDir()
	o := newOutStore(dir)
	first, _ := o.put("a")
	reopened := newOutStore(dir)
	second, _ := reopened.put("b")
	if first == second {
		t.Fatalf("reopened store reused %s", first)
	}
	if body, _ := os.ReadFile(first); string(body) != "a" {
		t.Errorf("first spill was overwritten: %q", body)
	}
}

func TestGcOutputKeepsFreshAndCurrent(t *testing.T) {
	base := t.TempDir()
	t.Setenv("HOME", base)
	root := filepath.Join(base, ".sesh", "out")

	mk := func(name string, age time.Duration) string {
		d := filepath.Join(root, name)
		os.MkdirAll(d, 0o700)
		f := filepath.Join(d, "out-1.log")
		os.WriteFile(f, []byte("body"), 0o600)
		when := time.Now().Add(-age)
		os.Chtimes(f, when, when)
		os.Chtimes(d, when, when)
		return d
	}
	fresh := mk("fresh", time.Hour)
	stale := mk("stale", 30*24*time.Hour)
	current := mk("current", 30*24*time.Hour)

	gcOutput("current")

	if _, err := os.Stat(fresh); err != nil {
		t.Error("a recently written conversation must be kept")
	}
	if _, err := os.Stat(current); err != nil {
		t.Error("the current conversation must never be collected")
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Error("output past the age floor must be collected")
	}
}

// The judge rules done on this text. A head-only elision fed it a wall of PASS
// lines from a run that failed.
func TestTranscriptElisionKeepsVerdict(t *testing.T) {
	full := failingRun(t)
	got := elideText(full, tune.TranscriptResult)
	if len(got) > tune.TranscriptResult {
		t.Errorf("elided to %d, over the %d budget", len(got), tune.TranscriptResult)
	}
	if !strings.Contains(got, "FAIL") {
		t.Errorf("the judge must see the verdict; got:\n%q", got)
	}
}

func TestCappedBufferKeepsTail(t *testing.T) {
	c := &cappedBuffer{max: 64}
	c.Write([]byte(strings.Repeat("o", 200)))
	c.Write([]byte("VERDICT"))
	if !strings.HasSuffix(string(c.buf), "VERDICT") {
		t.Errorf("cappedBuffer must keep the tail, got %q", c.buf)
	}
	if len(c.buf) > 64 {
		t.Errorf("cappedBuffer over cap: %d", len(c.buf))
	}
	if c.dropped == 0 {
		t.Error("dropped bytes must be counted")
	}
}

func betweenPointer(t *testing.T, s, after, before string) string {
	t.Helper()
	i := strings.Index(s, after)
	if i < 0 {
		t.Fatalf("pointer prefix %q missing from:\n%s", after, s)
	}
	rest := s[i+len(after):]
	j := strings.Index(rest, before)
	if j < 0 {
		t.Fatalf("pointer suffix %q missing from:\n%s", before, rest)
	}
	return rest[:j]
}

func numberAfter(t *testing.T, s, after string) int {
	t.Helper()
	i := strings.Index(s, after)
	if i < 0 {
		t.Fatalf("marker %q missing", after)
	}
	rest := s[i+len(after):]
	end := 0
	for end < len(rest) && rest[end] >= '0' && rest[end] <= '9' {
		end++
	}
	n, err := strconv.Atoi(rest[:end])
	if err != nil {
		t.Fatalf("no number after %q: %v", after, err)
	}
	return n
}
