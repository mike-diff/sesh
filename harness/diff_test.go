package harness

import (
	"os"
	"strings"
	"testing"
)

// TestDiffBlock: the trim diff shows the removed and added lines with one
// unchanged context line on each side, and nothing at all when nothing
// changed or the dial disables it. Breakers: drop the suffix trim and the
// whole tail renders as changed; drop the context append and the "  " lines
// vanish; drop the limit<=0 guard and the disabled case still diffs.
func TestDiffBlock(t *testing.T) {
	before := "a\nb\nc\nd\n"
	after := "a\nB2\nc\nd\n"
	got := diffBlock(before, after, 40)
	want := "  a\n- b\n+ B2\n  c"
	if got != want {
		t.Fatalf("diff:\n%q\nwant:\n%q", got, want)
	}
	if d := diffBlock(before, before, 40); d != "" {
		t.Fatalf("no change must produce no diff, got %q", d)
	}
	if d := diffBlock(before, after, -1); d != "" {
		t.Fatalf("-1 must disable the diff, got %q", d)
	}
}

// TestDiffBlockCap: a change larger than the cap truncates with an explicit
// dropped-line count, never an unbounded block. Breaker: remove the cap and
// the marker line never appears.
func TestDiffBlockCap(t *testing.T) {
	var a, b strings.Builder
	for i := 0; i < 30; i++ {
		a.WriteString("old\n")
		b.WriteString("new\n")
	}
	got := diffBlock(a.String(), b.String(), 10)
	if !strings.Contains(got, "more diff lines)") {
		t.Fatalf("cap marker missing:\n%s", got)
	}
	if n := strings.Count(got, "\n"); n > 11 {
		t.Fatalf("capped diff still has %d lines", n)
	}
}

// TestEditResultCarriesDiff: a successful edit's result includes the applied
// change, and the -1 dial returns it to the bare summary. Breakers: drop the
// diffBlock append from doEditHardened and the "- "/"+ " lines vanish; ignore
// tune.DiffLines and the disabled case still carries a diff.
func TestEditResultCarriesDiff(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	chtmp(t)
	old := tune
	t.Cleanup(func() { tune = old })

	writeAndEdit := func() string {
		if err := os.WriteFile("x.go", []byte("package main\n\nvar v = 1\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		res, isErr := doEditHardened("x.go", "var v = 1", "var v = 2", false)
		if isErr {
			t.Fatalf("edit failed: %s", res)
		}
		return res
	}

	tune.DiffLines = 40
	res := writeAndEdit()
	if !strings.Contains(res, "- var v = 1") || !strings.Contains(res, "+ var v = 2") {
		t.Fatalf("edit result must carry the diff, got %q", res)
	}

	tune.DiffLines = -1
	if res := writeAndEdit(); strings.Contains(res, "\n") {
		t.Fatalf("diff_lines=-1 must leave the bare summary, got %q", res)
	}
}

// TestWriteResultCarriesDiff: overwriting a file diffs against what it
// replaced; creating a new file does not pretend to. Breaker: drop the
// pre-read in doWrite and the overwrite case loses its diff.
func TestWriteResultCarriesDiff(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	chtmp(t)
	old := tune
	t.Cleanup(func() { tune = old })
	tune.DiffLines = 40

	res, isErr := doWrite("n.txt", "fresh\n", false)
	if isErr || strings.Contains(res, "\n") {
		t.Fatalf("new file must report bytes only, got %q err=%v", res, isErr)
	}
	res, isErr = doWrite("n.txt", "rewritten\n", false)
	if isErr || !strings.Contains(res, "- fresh") || !strings.Contains(res, "+ rewritten") {
		t.Fatalf("overwrite must diff against the old content, got %q err=%v", res, isErr)
	}
}
