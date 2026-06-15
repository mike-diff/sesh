package harness

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRenderStatusDefault(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	chtmp(t) // no project statusline script either

	got := renderStatus(statusInfo{Provider: "remote", Model: "model-b",
		Session: "s1", InputTokens: 48210, OutputTokens: 950})
	for _, want := range []string{"remote", "model-b", "s1", "48.2k", "950"} {
		if !strings.Contains(got, want) {
			t.Fatalf("default status missing %q: %q", want, got)
		}
	}

	// with no provider name, the protocol stands in
	got = renderStatus(statusInfo{Protocol: "openai", Model: "m"})
	if !strings.Contains(got, "openai") {
		t.Fatalf("protocol fallback: %q", got)
	}

	// context pressure renders as a percentage when the limit is known
	got = renderStatus(statusInfo{Provider: "a", Model: "m", ContextTokens: 4000, ContextLimit: 8000})
	if !strings.Contains(got, "ctx 4.0k/8.0k (50%)") {
		t.Fatalf("context percent: %q", got)
	}
	got = renderStatus(statusInfo{Provider: "a", Model: "m", ContextTokens: 4000})
	if !strings.Contains(got, "ctx 4.0k") || strings.Contains(got, "%") {
		t.Fatalf("context without limit: %q", got)
	}
}

// TestRenderStatusScript: an executable statusline script receives the JSON
// context and its first stdout line wins over the built-in default.
func TestRenderStatusScript(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	chtmp(t)

	script := filepath.Join(home, ".sesh", "statusline")
	os.MkdirAll(filepath.Dir(script), 0o755)
	// echo a line built from the JSON it receives, plus a second line that
	// must be ignored
	body := "#!/bin/sh\nmodel=$(sed 's/.*\"model\":\"\\([^\"]*\\)\".*/\\1/')\necho \"CUSTOM $model\"\necho IGNORED-SECOND-LINE\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}

	got := renderStatus(statusInfo{Provider: "remote", Model: "model-b", Session: "s1"})
	if got != "CUSTOM model-b" {
		t.Fatalf("script output: %q", got)
	}

	// a non-executable file is ignored: back to the default
	os.Chmod(script, 0o644)
	if got := renderStatus(statusInfo{Provider: "remote", Model: "model-b", Session: "s1"}); strings.Contains(got, "CUSTOM") {
		t.Fatalf("non-executable script should be ignored: %q", got)
	}

	// a failing script falls back to the default instead of a blank footer
	os.WriteFile(script, []byte("#!/bin/sh\nexit 1\n"), 0o755)
	if got := renderStatus(statusInfo{Provider: "remote", Model: "model-b", Session: "s1"}); !strings.Contains(got, "remote") {
		t.Fatalf("failing script must fall back: %q", got)
	}
}

func TestKTokens(t *testing.T) {
	if kTokens(950) != "950" || kTokens(48210) != "48.2k" {
		t.Fatalf("kTokens: %q %q", kTokens(950), kTokens(48210))
	}
}

// TestRenderStatusNoProvider: with no active provider, the status line says so
// and never names a resolved-default model. Breaker: drop the NoProvider branch
// and it falls through to formatting the empty provider/model fields.
func TestRenderStatusNoProvider(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	chtmp(t)
	got := renderStatus(statusInfo{Session: "s1", NoProvider: true})
	if !strings.Contains(got, "no provider") || !strings.Contains(got, "s1") {
		t.Fatalf("no-provider status must say so and name the session: %q", got)
	}
	if strings.Contains(got, "claude") || strings.Contains(got, "anthropic") {
		t.Fatalf("must not advertise a model the user never configured: %q", got)
	}
}
