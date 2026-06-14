package harness

import (
	"fmt"
	"os"
	"strings"
	"testing"
)

// TestSearchSmartCase: an all-lowercase pattern matches case-insensitively;
// any uppercase in the pattern restores exact matching. Breakers: drop the
// fold and the lowercase query misses DefaultPort; fold unconditionally and
// the uppercase query stops being exact.
func TestSearchSmartCase(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	chtmp(t)
	os.WriteFile("config.go", []byte("package main\n\nvar DefaultPort = 8080\n"), 0o644)

	out, isErr := doSearch("defaultport")
	if isErr || !strings.Contains(out, "DefaultPort") {
		t.Fatalf("lowercase pattern must smart-case match: %q err=%v", out, isErr)
	}
	out, _ = doSearch("DEFAULTPORT")
	if out != "no matches" {
		t.Fatalf("uppercase pattern must stay exact, got %q", out)
	}
}

// TestSearchGroupedOutput: hits are grouped under one path header with a
// match-and-file count line, instead of repeating the path per hit. Breaker:
// revert to flat path:line output and the header and grouping assertions fail.
func TestSearchGroupedOutput(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	chtmp(t)
	os.WriteFile("a.go", []byte("needle one\nneedle two\n"), 0o644)
	os.WriteFile("b.go", []byte("needle three\n"), 0o644)

	out, isErr := doSearch("needle")
	if isErr {
		t.Fatalf("search errored: %s", out)
	}
	if !strings.Contains(out, "3 matches in 2 files") {
		t.Fatalf("count header missing:\n%s", out)
	}
	if strings.Count(out, "a.go") != 1 || strings.Count(out, "b.go") != 1 {
		t.Fatalf("each path must appear once as a group header:\n%s", out)
	}
	if !strings.Contains(out, "  1: needle one") || !strings.Contains(out, "  2: needle two") {
		t.Fatalf("grouped numbered lines missing:\n%s", out)
	}
}

// TestSearchGitignore: paths matched by the root .gitignore subset (dir rules,
// basename globs, anchored rules) are not searched. Breaker: drop the
// loadGitignore call from doSearch and the ignored hits surface.
func TestSearchGitignore(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	chtmp(t)
	os.WriteFile(".gitignore", []byte("# junk\nvendor/\n*.log\n/build\n"), 0o644)
	os.MkdirAll("vendor/dep", 0o755)
	os.MkdirAll("build", 0o755)
	os.WriteFile("vendor/dep/x.go", []byte("needle in vendor\n"), 0o644)
	os.WriteFile("trace.log", []byte("needle in log\n"), 0o644)
	os.WriteFile("build/out.go", []byte("needle in build\n"), 0o644)
	os.WriteFile("keep.go", []byte("needle kept\n"), 0o644)

	out, _ := doSearch("needle")
	for _, banned := range []string{"vendor", "trace.log", "build"} {
		if strings.Contains(out, banned) {
			t.Fatalf("gitignored path %q surfaced:\n%s", banned, out)
		}
	}
	if !strings.Contains(out, "keep.go") {
		t.Fatalf("non-ignored hit missing:\n%s", out)
	}
}

// TestSearchOverCapSuppresses: past the cap, match lines are SUPPRESSED in
// favor of counts, densest files, and narrowing guidance, never a silent
// first-N keep. Breaker: return the collected lines when over the cap and the
// no-match-lines assertion fails.
func TestSearchOverCapSuppresses(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	chtmp(t)
	for i := 0; i < 8; i++ {
		var b strings.Builder
		for j := 0; j < 10; j++ {
			fmt.Fprintf(&b, "common line %d-%d\n", i, j)
		}
		os.WriteFile(fmt.Sprintf("f%d.txt", i), []byte(b.String()), 0o644)
	}

	out, isErr := doSearch("common")
	if isErr {
		t.Fatalf("search errored: %s", out)
	}
	if !strings.Contains(out, "too many to show") || !strings.Contains(out, "Narrow the pattern") {
		t.Fatalf("over-cap guidance missing:\n%s", out)
	}
	if !strings.Contains(out, "80 matches in 8 files") {
		t.Fatalf("true counts missing:\n%s", out)
	}
	if !strings.Contains(out, "densest files") || !strings.Contains(out, "f0.txt") {
		t.Fatalf("per-file summary missing:\n%s", out)
	}
	if strings.Contains(out, "common line") {
		t.Fatalf("over-cap output must suppress match lines, not keep the first N:\n%s", out)
	}
}

// TestIgnoredBy: the documented .gitignore subset semantics, rule by rule.
// Breakers: drop the anchored prefix check and /build stops pruning its
// subtree; drop segment matching and a nested vendor dir surfaces; treat
// dirOnly rules as file rules and trace.log-style names get pruned as dirs.
func TestIgnoredBy(t *testing.T) {
	rules := loadGitignoreFrom(t, "vendor/\n*.log\n/build\n")
	cases := []struct {
		rel   string
		isDir bool
		want  bool
	}{
		{"vendor", true, true},
		{"x/vendor", true, true},
		{"vendor", false, false}, // dirOnly rule never matches a file
		{"trace.log", false, true},
		{"deep/trace.log", false, true},
		{"build", true, true},
		{"build/sub", true, true},
		{"x/build", true, false}, // anchored: only the root build
	}
	for _, c := range cases {
		if got := ignoredBy(rules, c.rel, c.isDir); got != c.want {
			t.Fatalf("ignoredBy(%q, dir=%v) = %v, want %v", c.rel, c.isDir, got, c.want)
		}
	}
}

func loadGitignoreFrom(t *testing.T, content string) []ignoreRule {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(dir+"/.gitignore", []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return loadGitignore(dir)
}
