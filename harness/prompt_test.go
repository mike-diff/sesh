package harness

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"sesh/agent"
)

// TestSystemPromptChain pins the steering resolution: the guide-structured
// default, SYSTEM.md replacement, APPEND_SYSTEM.md on top of either, and the
// XML-wrapped project context.
func TestSystemPromptChain(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	chtmp(t)
	cwd, _ := os.Getwd()

	// default: names the harness, includes the cwd, follows the guide's shape
	got := systemPrompt()
	for _, want := range []string{"operating inside sesh", cwd, "<role>", "<constraints>", "<workflow>", "<output>"} {
		if !strings.Contains(got, want) {
			t.Fatalf("default prompt missing %q", want)
		}
	}

	// APPEND_SYSTEM.md rides on top of the default
	os.MkdirAll(".sesh", 0o755)
	os.WriteFile(".sesh/APPEND_SYSTEM.md", []byte("EXTRA-RULE-42"), 0o644)
	got = systemPrompt()
	if !strings.Contains(got, "<role>") || !strings.Contains(got, "EXTRA-RULE-42") {
		t.Fatalf("append should add to the default, got: %.80s...", got)
	}

	// AGENTS.md is wrapped in a project_context tag with its path
	os.WriteFile("AGENTS.md", []byte("project beliefs"), 0o644)
	got = systemPrompt()
	if !strings.Contains(got, `<project_context path="AGENTS.md">`) || !strings.Contains(got, "project beliefs") {
		t.Fatalf("project context wrapping wrong: %.120s...", got)
	}

	// SYSTEM.md replaces the default entirely, but append and context remain
	os.WriteFile(".sesh/SYSTEM.md", []byte("CUSTOM PERSONA"), 0o644)
	got = systemPrompt()
	if strings.Contains(got, "<role>") {
		t.Fatal("custom SYSTEM.md must replace the default")
	}
	for _, want := range []string{"CUSTOM PERSONA", "EXTRA-RULE-42", "project beliefs"} {
		if !strings.Contains(got, want) {
			t.Fatalf("custom prompt missing %q", want)
		}
	}

	// the global append is the fallback when no project append exists
	os.Remove(".sesh/APPEND_SYSTEM.md")
	os.MkdirAll(filepath.Join(os.Getenv("HOME"), ".sesh"), 0o755)
	os.WriteFile(filepath.Join(os.Getenv("HOME"), ".sesh", "APPEND_SYSTEM.md"), []byte("GLOBAL-RULE"), 0o644)
	if got = systemPrompt(); !strings.Contains(got, "GLOBAL-RULE") {
		t.Fatal("global APPEND_SYSTEM.md should apply")
	}
}

// TestIdentityBlock: the model is told exactly what serves it, and a
// mid-conversation switch is noted so it can account for foreign turns.
func TestIdentityBlock(t *testing.T) {
	got := identityBlock("remote", "model-b", "openai", false)
	for _, want := range []string{"<identity>", `"model-b"`, `"remote"`, "do not rely on internal beliefs"} {
		if !strings.Contains(got, want) {
			t.Fatalf("identity block missing %q: %s", want, got)
		}
	}
	if strings.Contains(got, "changed during this conversation") {
		t.Fatal("unswitched block must not mention a switch")
	}
	got = identityBlock("", "m", "openai", true)
	if !strings.Contains(got, "changed during this conversation") || !strings.Contains(got, "ad hoc") {
		t.Fatalf("switched/ad hoc block wrong: %s", got)
	}
}

// TestReplSwitchUpdatesIdentity: switching models mid-conversation rebuilds
// the system prompt with the new identity and the switch note.
func TestReplSwitchUpdatesIdentity(t *testing.T) {
	r := newTestRepl(t)
	r.history = []agent.Turn{{Role: "user", Text: "x"}, {Role: "assistant", Text: "y"}}
	r.command("/model m2")
	if !strings.Contains(r.system, `"m2"`) {
		t.Fatalf("system prompt should carry the new model: %.120s", r.system)
	}
	if !strings.Contains(r.system, "changed during this conversation") {
		t.Fatal("mid-conversation switch should be noted")
	}
}
