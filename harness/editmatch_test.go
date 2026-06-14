package harness

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"sesh/agent"
)

// TestMatchLadderTrailingSpace: a needle quoted without the file's trailing
// spaces still matches at rung 2.
// Breaker: drop the trimTrailingPerLine rung from matchLadder and this fails.
func TestMatchLadderTrailingSpace(t *testing.T) {
	text := "func f() {\n\treturn 1   \n}\n" // target line has 3 trailing spaces
	old := "\treturn 1\n}"                   // model quoted it without them
	mr, ok := matchLadder(text, old)
	if !ok {
		t.Fatal("trailing-whitespace-tolerant rung should find the match")
	}
	if mr.count != 1 {
		t.Fatalf("want exactly one match, got %d", mr.count)
	}
	// The matched range must cover the file's real (spaced) bytes so a splice
	// replaces the right region.
	if got := text[mr.start:mr.end]; !strings.Contains(got, "return 1") {
		t.Fatalf("matched range %q does not cover the target", got)
	}
}

// TestMatchLadderIndentOffset: a block quoted at a different uniform indent
// still matches at rung 3.
// Breaker: drop the stripCommonIndent rung from matchLadder and this fails.
func TestMatchLadderIndentOffset(t *testing.T) {
	// File indents the block with two tabs; the model quoted it with none.
	text := "x\n\t\tif a {\n\t\t\tb()\n\t\t}\ny\n"
	old := "if a {\n\tb()\n}"
	mr, ok := matchLadder(text, old)
	if !ok {
		t.Fatal("uniform-indent rung should find the match")
	}
	if mr.count != 1 {
		t.Fatalf("want exactly one match, got %d", mr.count)
	}
	if got := text[mr.start:mr.end]; !strings.Contains(got, "b()") {
		t.Fatalf("matched range %q does not cover the target", got)
	}
}

// TestMatchLadderRejectsFuzzyContent: a needle whose CONTENT differs (a renamed
// identifier, not just whitespace) must NOT match. The ladder is whitespace
// tolerant only; guessing a near-but-wrong region is the unrecoverable failure
// the design forbids.
// Breaker: add any edit-distance / fuzzy content rung to matchLadder and this
// test fails (the near-miss would start matching).
func TestMatchLadderRejectsFuzzyContent(t *testing.T) {
	text := "total := computeSum(items)\n"
	old := "total := computeTotal(items)" // one word differs: not whitespace
	if _, ok := matchLadder(text, old); ok {
		t.Fatal("content that differs by more than whitespace must not match")
	}
}

// TestBOMCRLFRoundTrip: an edit on a CRLF+BOM file must preserve both on write,
// touching only the edited region.
// Breaker: stop calling restoreContent in doEditHardened (write the normalized
// LF/no-BOM text) and this fails.
func TestBOMCRLFRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f.txt")
	original := utf8BOM + "alpha\r\nbeta\r\ngamma\r\n"
	if err := os.WriteFile(path, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}
	mustChdir(t, dir)

	res, isErr := doEditHardened("f.txt", "beta", "BETA", false)
	if isErr {
		t.Fatalf("edit should succeed, got error: %s", res)
	}
	got, err := os.ReadFile("f.txt")
	if err != nil {
		t.Fatal(err)
	}
	want := utf8BOM + "alpha\r\nBETA\r\ngamma\r\n"
	if string(got) != want {
		t.Fatalf("round-trip not preserved.\n got: %q\nwant: %q", string(got), want)
	}
}

// TestHardenedZeroMatchHint: a zero-match error must carry a nearest-miss hint
// so the model can recover. Error quality drives recovery.
// Breaker: drop the nearestMiss call from doEditHardened's zero-match branch
// and this fails.
func TestHardenedZeroMatchHint(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f.go")
	if err := os.WriteFile(path, []byte("func handleRequest() {\n\tlog.Print(\"hi\")\n}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustChdir(t, dir)
	res, isErr := doEditHardened("f.go", "func handleResponse() {", "func handleResponse(ctx) {", false)
	if !isErr {
		t.Fatal("a missing target must be an error")
	}
	if !strings.Contains(res, "nearest similar region") {
		t.Fatalf("zero-match error must include a nearest-miss hint, got: %s", res)
	}
	if !strings.Contains(res, "handleRequest") {
		t.Fatalf("hint must quote the actually-present nearby line, got: %s", res)
	}
}

// TestHardenedMultiMatchCount: an ambiguous target must report the count and
// ask for more context, not silently edit the first.
// Breaker: make doEditHardened edit the first match instead of erroring on
// count>1 and this fails.
func TestHardenedMultiMatchCount(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f.txt")
	if err := os.WriteFile(path, []byte("x = 1\ny = 1\nz = 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustChdir(t, dir)
	res, isErr := doEditHardened("f.txt", "= 1", "= 2", false)
	if !isErr {
		t.Fatal("an ambiguous target must be an error, not a silent first-match edit")
	}
	if !strings.Contains(res, "3") {
		t.Fatalf("multi-match error must report the count (3), got: %s", res)
	}
}

// editToolDesc returns the description of the "edit" tool builtinTools produced,
// so a test can tell which arm the env switch selected without reaching into
// tool internals (each arm has a distinct description).
func editToolDesc(tools []agent.Tool) string {
	for _, tl := range tools {
		if tl.Def.Name == "edit" {
			return tl.Def.Description
		}
	}
	return ""
}

// mustChdir switches into dir for a test and restores the prior cwd after, so
// the file tools (confined to cwd) operate inside the temp tree.
func mustChdir(t *testing.T, dir string) {
	t.Helper()
	prev, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chdir(prev) })
}
