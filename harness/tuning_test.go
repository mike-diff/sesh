package harness

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestTuningOverlayChain: defaults hold when nothing is stated; the global
// file overrides only its stated fields; the project file overrides the
// global; invalid JSON is skipped, not fatal.
func TestTuningOverlayChain(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	chtmp(t)

	if got := loadTuning(); got != defaultTuning() {
		t.Fatalf("no files must mean pure defaults: %+v", got)
	}

	os.MkdirAll(filepath.Join(home, ".sesh"), 0o755)
	os.WriteFile(filepath.Join(home, ".sesh", "tuning.json"),
		[]byte(`{"handoff_pct": 70, "task_depth": 5}`), 0o644)
	got := loadTuning()
	if got.HandoffPct != 70 || got.TaskDepth != 5 {
		t.Fatalf("global overrides not applied: %+v", got)
	}
	if got.HardPct != 90 || got.StuckAfter != 3 {
		t.Fatalf("unstated fields must keep defaults: %+v", got)
	}

	os.MkdirAll(".sesh", 0o755)
	os.WriteFile(".sesh/tuning.json", []byte(`{"handoff_pct": 60}`), 0o644)
	got = loadTuning()
	if got.HandoffPct != 60 {
		t.Fatalf("project must beat global: %d", got.HandoffPct)
	}
	if got.TaskDepth != 5 {
		t.Fatalf("project must not erase global fields it does not state: %d", got.TaskDepth)
	}

	os.WriteFile(".sesh/tuning.json", []byte(`{broken`), 0o644)
	if got = loadTuning(); got.HandoffPct != 70 {
		t.Fatalf("invalid project file must be skipped, keeping global: %d", got.HandoffPct)
	}
}

// TestTuningBriefDials: the string dials overlay like the numeric ones: stated
// fields land, unstated fields keep their layer's value. Breaker: drop the
// string setter from overlayTuning and brief_model never leaves the file.
func TestTuningBriefDials(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	chtmp(t)

	os.MkdirAll(filepath.Join(home, ".sesh"), 0o755)
	os.WriteFile(filepath.Join(home, ".sesh", "tuning.json"),
		[]byte(`{"brief_provider": "ollama", "brief_model": "qwen-rig"}`), 0o644)
	got := loadTuning()
	if got.BriefProvider != "ollama" || got.BriefModel != "qwen-rig" {
		t.Fatalf("brief dials not applied: %+v", got)
	}

	os.MkdirAll(".sesh", 0o755)
	os.WriteFile(".sesh/tuning.json", []byte(`{"brief_model": "other"}`), 0o644)
	got = loadTuning()
	if got.BriefModel != "other" {
		t.Fatalf("project must beat global: %q", got.BriefModel)
	}
	if got.BriefProvider != "ollama" {
		t.Fatalf("project must not erase global fields it does not state: %q", got.BriefProvider)
	}
}

// TestRender: placeholders substitute, repeats included; unknown placeholders
// survive untouched (a template typo must stay visible, never vanish).
func TestRender(t *testing.T) {
	got := render("hi {{name}}, {{name}}: {{unknown}}", map[string]string{"name": "x"})
	if got != "hi x, x: {{unknown}}" {
		t.Fatalf("render: %q", got)
	}
}

// TestSteerPromptChain: project file beats global beats built-in; empty files
// fall through to the built-in.
func TestSteerPromptChain(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	chtmp(t)

	if got := steerPrompt("judge", "BUILTIN"); got != "BUILTIN" {
		t.Fatalf("no files: %q", got)
	}
	os.MkdirAll(filepath.Join(home, ".sesh", "prompts"), 0o755)
	os.WriteFile(filepath.Join(home, ".sesh", "prompts", "judge.md"), []byte("GLOBAL"), 0o644)
	if got := steerPrompt("judge", "BUILTIN"); got != "GLOBAL" {
		t.Fatalf("global override: %q", got)
	}
	os.MkdirAll(".sesh/prompts", 0o755)
	os.WriteFile(".sesh/prompts/judge.md", []byte("PROJECT"), 0o644)
	if got := steerPrompt("judge", "BUILTIN"); got != "PROJECT" {
		t.Fatalf("project override: %q", got)
	}
}

// TestJudgeUsesPromptMod: the override actually reaches the model call, end
// to end through judgeGoal (breaker: drop steerPrompt from judgeGoal).
func TestJudgeUsesPromptMod(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	chtmp(t)
	os.MkdirAll(".sesh/prompts", 0o755)
	os.WriteFile(".sesh/prompts/judge.md", []byte("CUSTOM-JUDGE-RULES reply with JSON"), 0o644)

	var sawPrompt string
	p := promptSpy{saw: &sawPrompt, reply: `{"done": true, "blocked": false, "reason": "ok"}`}
	if _, _, err := judgeGoal(context.Background(), p, "req", "transcript"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(sawPrompt, "CUSTOM-JUDGE-RULES") {
		t.Fatal("the judge must run under the prompt mod")
	}
}

// TestUsageCoversSurface: -help is the agent-facing feature index; every
// load-bearing capability must be discoverable from it (breaker: delete any
// section from usageText).
func TestUsageCoversSurface(t *testing.T) {
	got := usageText()
	for _, want := range []string{
		"-p ", "-continue", "-resume", "-doctor", "-yes", "-max-iters", "-max-tools",
		"judge", "handoff", "/chain", "/context", "/provider", "/model",
		"task", "recall", "tuning.json", "prompts/<name>.md", "SYSTEM.md",
		"EXIT CODES", "sessions/", "chains/",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("usage missing %q: an agent cannot discover that feature", want)
		}
	}
}
