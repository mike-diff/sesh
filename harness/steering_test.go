package harness

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Breaker: break after the first APPEND_SYSTEM.md found, so a project file
// shadows the global layer instead of appending to it.
func TestAppendSystemLayersGlobalThenProject(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	work := t.TempDir()
	old, _ := os.Getwd()
	if err := os.Chdir(work); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chdir(old) })

	os.MkdirAll(filepath.Join(home, ".sesh"), 0o755)
	os.MkdirAll(filepath.Join(work, ".sesh"), 0o755)
	os.WriteFile(filepath.Join(home, ".sesh", "APPEND_SYSTEM.md"), []byte("GLOBAL-RULE: never use em dashes"), 0o644)
	os.WriteFile(filepath.Join(work, ".sesh", "APPEND_SYSTEM.md"), []byte("PROJECT-RULE: answer in French"), 0o644)

	trustProjectMods(t)
	prompt := systemPrompt()
	gi := strings.Index(prompt, "GLOBAL-RULE")
	pi := strings.Index(prompt, "PROJECT-RULE")
	if gi < 0 || pi < 0 {
		t.Fatalf("both APPEND_SYSTEM layers must apply; global at %d, project at %d:\n%s", gi, pi, prompt)
	}
	if gi > pi {
		t.Fatalf("global must come first so the project layer overrides by position (global %d, project %d)", gi, pi)
	}
}

// TestDefaultPromptCarriesFrontendSteering: with no SYSTEM.md override, the
// built-in prompt ships the always-on frontend design guidance, including its
// accessibility commitment. Breaker: drop the <frontend> section, or strip its
// WCAG rule, from defaultPromptTemplate.
func TestDefaultPromptCarriesFrontendSteering(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	work := t.TempDir()
	old, _ := os.Getwd()
	if err := os.Chdir(work); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chdir(old) })

	prompt := systemPrompt()
	if !strings.Contains(prompt, "<frontend>") {
		t.Fatalf("the default prompt must ship the frontend section:\n%s", prompt)
	}
	if !strings.Contains(prompt, "WCAG") {
		t.Fatal("the frontend section must keep its accessibility (WCAG) rule")
	}
}

// TestProjectContextWalksToGitRoot: instruction files from the git root down
// to the working directory all apply, root first, one per directory, and a
// directory above the repository never leaks in. Breakers: read only the
// working directory and the root file is missing; walk past the repository
// root and the outside file appears; reverse the order and root comes last.
func TestProjectContextWalksToGitRoot(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	outer := t.TempDir()
	repo := filepath.Join(outer, "repo")
	sub := filepath.Join(repo, "svc", "api")
	os.MkdirAll(filepath.Join(repo, ".git"), 0o755)
	os.MkdirAll(sub, 0o755)
	os.WriteFile(filepath.Join(outer, "AGENTS.md"), []byte("OUTSIDE-RULE"), 0o644)
	os.WriteFile(filepath.Join(repo, "AGENTS.md"), []byte("ROOT-RULE"), 0o644)
	os.WriteFile(filepath.Join(repo, "CLAUDE.md"), []byte("ROOT-SECOND-FILE"), 0o644)
	os.WriteFile(filepath.Join(sub, "CLAUDE.md"), []byte("LEAF-RULE"), 0o644)
	old, _ := os.Getwd()
	os.Chdir(sub)
	t.Cleanup(func() { os.Chdir(old) })

	got := projectContext()
	ri, li := strings.Index(got, "ROOT-RULE"), strings.Index(got, "LEAF-RULE")
	if ri < 0 || li < 0 || ri > li {
		t.Fatalf("root then leaf expected (root %d, leaf %d):\n%s", ri, li, got)
	}
	if strings.Contains(got, "OUTSIDE-RULE") || strings.Contains(got, "ROOT-SECOND-FILE") {
		t.Fatalf("only one file per directory, and nothing above the repository:\n%s", got)
	}
	if !strings.Contains(got, `path="CLAUDE.md"`) {
		t.Fatalf("the working directory's file keeps its plain path:\n%s", got)
	}

	old2 := tune.ProjectContextMax
	tune.ProjectContextMax = 12
	defer func() { tune.ProjectContextMax = old2 }()
	if got := projectContext(); strings.Contains(got, "ROOT-RULE") || !strings.Contains(got, "LEAF-RULE") || !strings.Contains(got, "omitted") {
		t.Fatalf("over budget, the nearest file wins and the drop is announced:\n%s", got)
	}
}
