package harness

import (
	"os"
	"strconv"
	"strings"
	"syscall"
	"testing"
)

// TestWritePreservesMode: overwriting an existing file must keep its
// permissions; write-then-rename replaces the inode, so the tmp file has to
// carry the target's mode (an explicit chmod, not just OpenFile, because
// umask would otherwise eat bits like the exec bit).
// Breaker: drop the explicit chmod in doWrite and both halves fail (755
// becomes 644; 600 becomes 644 under the default umask).
func TestWritePreservesMode(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	chtmp(t)
	os.WriteFile("run.sh", []byte("#!/bin/sh\necho old\n"), 0o755)
	os.Chmod("run.sh", 0o755) // set exactly: WriteFile's mode is umask-filtered
	os.WriteFile("secret.conf", []byte("old"), 0o600)
	os.Chmod("secret.conf", 0o600)

	// Under umask 022 OpenFile alone would preserve these modes; a private
	// umask strips bits, so the test runs under 077 to pin the exact-preserve
	// behavior (the explicit chmod in doWrite).
	oldUmask := syscall.Umask(0o077)
	t.Cleanup(func() { syscall.Umask(oldUmask) })
	if out, isErr := doWrite("run.sh", "#!/bin/sh\necho new\n", false); isErr {
		t.Fatalf("write to script failed: %s", out)
	}
	if out, isErr := doWrite("secret.conf", "new", false); isErr {
		t.Fatalf("write to secret failed: %s", out)
	}
	for _, c := range []struct {
		path string
		want os.FileMode
	}{{"run.sh", 0o755}, {"secret.conf", 0o600}} {
		fi, err := os.Stat(c.path)
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm() != c.want {
			t.Fatalf("%s: mode is %o, want %o", c.path, fi.Mode().Perm(), c.want)
		}
	}
}

// TestWriteNewFileDefaultMode: a file that did not exist is created 0644,
// like every plain os.WriteFile path in the harness.
// Breaker: hardcode a different mode for new files and this fails.
func TestWriteNewFileDefaultMode(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	chtmp(t)
	if out, isErr := doWrite("fresh.txt", "hi", false); isErr {
		t.Fatalf("write failed: %s", out)
	}
	fi, err := os.Stat("fresh.txt")
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o644 {
		t.Fatalf("new file mode is %o, want 644", fi.Mode().Perm())
	}
}

// TestReadPagesByLine: offset (1-based line) and limit window a read so the
// model can reach any part of a file larger than the context cap, and every
// paged read says exactly which slice it got.
// Breaker: remove the offset/limit handling in doRead and the window and the
// "lines X-Y of Z" footer vanish.
func TestReadPagesByLine(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	chtmp(t)
	var lines []string
	for i := 1; i <= 100; i++ {
		lines = append(lines, "padding "+strconv.Itoa(i))
	}
	os.WriteFile("paged.txt", []byte(strings.Join(lines, "\n")+"\n"), 0o644)

	out, isErr := doRead("paged.txt", false, 90, 5)
	if isErr {
		t.Fatalf("paged read errored: %s", out)
	}
	if !strings.Contains(out, "padding 90") || strings.Contains(out, "padding 89") || strings.Contains(out, "padding 95") {
		t.Fatalf("window must be lines 90-94, got:\n%s", out)
	}
	if !strings.Contains(out, "lines 90-94 of 100") {
		t.Fatalf("footer must state the window, got:\n%s", out)
	}
}

// TestReadOffsetPastEnd: an offset beyond the file is a model-readable error,
// not an empty success.
// Breaker: clamp instead of refusing and the error disappears.
func TestReadOffsetPastEnd(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	chtmp(t)
	os.WriteFile("tiny.txt", []byte("one\ntwo\n"), 0o644)
	out, isErr := doRead("tiny.txt", false, 500, 10)
	if !isErr || !strings.Contains(out, "past the end") {
		t.Fatalf("offset past EOF must be a recoverable error, got %q err=%v", out, isErr)
	}
}

// TestReadTruncationFooter: an unpaged read of a file over the cap keeps the
// head but must say how to continue (total lines and the next offset), instead
// of the core's generic byte-count truncation.
// Breaker: drop the footer and the "re-read with offset" guidance is gone.
func TestReadTruncationFooter(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	chtmp(t)
	var lines []string
	for i := 1; i <= 3000; i++ {
		lines = append(lines, "filler "+strconv.Itoa(i))
	}
	os.WriteFile("big.txt", []byte(strings.Join(lines, "\n")+"\n"), 0o644)

	out, isErr := doRead("big.txt", false, 0, 0)
	if isErr {
		t.Fatalf("big read errored: %s", out)
	}
	if !strings.Contains(out, "of 3000 lines") || !strings.Contains(out, "re-read with offset") {
		t.Fatalf("truncated read must disclose the total and how to page, got tail:\n%s", out[len(out)-300:])
	}
}

// TestReadOffsetWithoutLimit: offset alone continues from a line, returning as
// much as fits; the natural follow-up to the truncation footer.
// Breaker: require both parameters and this errors.
func TestReadOffsetWithoutLimit(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	chtmp(t)
	var lines []string
	for i := 1; i <= 10; i++ {
		lines = append(lines, "row "+strconv.Itoa(i))
	}
	os.WriteFile("rows.txt", []byte(strings.Join(lines, "\n")+"\n"), 0o644)
	out, isErr := doRead("rows.txt", false, 8, 0)
	if isErr || !strings.Contains(out, "row 8") || !strings.Contains(out, "row 10") || strings.Contains(out, "row 7") {
		t.Fatalf("offset-only read must run to the end, got %q err=%v", out, isErr)
	}
}

// TestLocCountsMainstreamCodeAndDisclosesTheRest: mainstream code extensions
// are counted; whatever is not counted is disclosed by extension instead of
// silently shrinking the total.
// Breakers: drop .java from sourceExts and the total loses 4 lines; drop the
// disclosure footer and the md mention vanishes.
func TestLocCountsMainstreamCodeAndDisclosesTheRest(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	chtmp(t)
	os.WriteFile("a.py", []byte("1\n2\n3\n"), 0o644)
	os.WriteFile("c.java", []byte("1\n2\n3\n4\n"), 0o644)
	os.WriteFile("b.md", []byte("1\n2\n3\n4\n5\n"), 0o644)

	out, isErr := doLoc("", false)
	if isErr {
		t.Fatalf("loc errored: %s", out)
	}
	if !strings.Contains(out, "total lines: 7") {
		t.Fatalf("java must be counted, got:\n%s", out)
	}
	if !strings.Contains(out, "not counted") || !strings.Contains(out, "md") {
		t.Fatalf("uncounted extensions must be disclosed, got:\n%s", out)
	}
}
