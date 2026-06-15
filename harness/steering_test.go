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
