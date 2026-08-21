package harness

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mike-diff/sesh/agent"
)

func TestProvidersResolve(t *testing.T) {
	cfg := ProvidersConfig{
		Default: "anthropic",
		Providers: map[string]Profile{
			"anthropic": {Protocol: "anthropic", Model: "claude-opus-4-8", KeyEnv: "ANTHROPIC_API_KEY"},
			"local":     {Protocol: "openai", URL: "http://localhost:8080/v1", Model: "qwen3.5:9b"},
		},
	}

	// empty name resolves to the default
	name, prof, err := cfg.resolve("")
	if err != nil || name != "anthropic" || prof.Model != "claude-opus-4-8" {
		t.Fatalf("default resolve: %q %+v err=%v", name, prof, err)
	}

	// a named profile resolves to itself
	if _, prof, err := cfg.resolve("local"); err != nil || prof.URL != "http://localhost:8080/v1" {
		t.Fatalf("named resolve: %+v err=%v", prof, err)
	}

	// an unknown name errors and lists what is available
	if _, _, err := cfg.resolve("ghost"); err == nil {
		t.Fatal("expected error for unknown provider")
	} else if got := err.Error(); !strings.Contains(got, "anthropic") || !strings.Contains(got, "local") {
		t.Fatalf("error should list available providers: %q", got)
	}

	// no name and no default is an error, not a panic
	empty := ProvidersConfig{Providers: map[string]Profile{}}
	if _, _, err := empty.resolve(""); err == nil {
		t.Fatal("expected error when no provider and no default")
	}
}

func TestProvidersOverlay(t *testing.T) {
	global := ProvidersConfig{
		Default: "anthropic",
		Providers: map[string]Profile{
			"anthropic": {Protocol: "anthropic", Model: "claude-opus-4-8"},
			"local":     {Protocol: "openai", Model: "alpha:9b"},
		},
	}
	// overlay is the merge primitive behind the GLOBAL file (and any future
	// trusted layer); the project file goes through loadProvidersNotes, which
	// applies the trust rules on top of this merge.
	project := ProvidersConfig{
		Default: "local",
		Providers: map[string]Profile{
			"local":  {Protocol: "openai", Model: "alpha:14b"}, // override
			"remote": {Protocol: "openai", Model: "model-b", Key: "sk-inline"},
		},
	}
	global.overlay(project)

	if global.Default != "local" {
		t.Fatalf("default not overridden: %q", global.Default)
	}
	if global.Providers["local"].Model != "alpha:14b" {
		t.Fatalf("profile not overridden: %+v", global.Providers["local"])
	}
	if global.Providers["anthropic"].Model != "claude-opus-4-8" {
		t.Fatal("inherited profile was lost")
	}
	if z := global.Providers["remote"]; z.Key != "sk-inline" {
		t.Fatalf("added profile with inline key wrong: %+v", z)
	}
}

// TestResolveSpec pins the three-layer precedence: profile, then a resumed
// session's brain, then explicit flags, strongest last.
func TestResolveSpec(t *testing.T) {
	cfg := ProvidersConfig{
		Default: "loc",
		Providers: map[string]Profile{
			"loc":    {Protocol: "openai", URL: "http://l", Model: "lm"},
			"remote": {Protocol: "openai", URL: "http://z", Model: "zm", KeyEnv: "ZK"},
		},
	}
	creds := map[string]string{"remote": "sk-stored"}
	none := map[string]bool{}
	resumed := &Session{Protocol: "openai", URL: "http://s", Model: "sm",
		Turns: []agent.Turn{{Role: "user", Text: "x"}}}

	// the default profile fills everything the user did not type
	s, err := resolveSpec(selection{protocol: "anthropic", explicit: none}, nil, cfg, creds)
	if err != nil || s.name != "loc" || s.protocol != "openai" || s.url != "http://l" || s.model != "lm" {
		t.Fatalf("default profile: %+v err=%v", s, err)
	}

	// explicit flags beat the profile, field by field
	s, _ = resolveSpec(selection{protocol: "anthropic", model: "mm",
		explicit: map[string]bool{"protocol": true, "model": true}}, nil, cfg, creds)
	if s.protocol != "anthropic" || s.model != "mm" || s.url != "http://l" {
		t.Fatalf("flags should win per field: %+v", s)
	}

	// a named provider pulls its stored credential; key_env rides along
	s, _ = resolveSpec(selection{provider: "remote", explicit: map[string]bool{"provider": true}}, nil, cfg, creds)
	if s.key != "sk-stored" || s.keyEnv != "ZK" {
		t.Fatalf("credential lookup: %+v", s)
	}

	// a resumed session's brain beats the default profile
	s, _ = resolveSpec(selection{protocol: "anthropic", explicit: none}, resumed, cfg, creds)
	if s.url != "http://s" || s.model != "sm" {
		t.Fatalf("session should win over profile: %+v", s)
	}

	// ...but an explicit -provider beats the session
	s, _ = resolveSpec(selection{provider: "remote", explicit: map[string]bool{"provider": true}}, resumed, cfg, creds)
	if s.url != "http://z" || s.model != "zm" {
		t.Fatalf("-provider should beat session: %+v", s)
	}

	// a resumed session's CREDENTIAL follows its remembered provider name,
	// even when the default profile is a different (keyless) provider
	remoteSess := &Session{Provider: "remote", Protocol: "openai", URL: "http://z", Model: "zm",
		Turns: []agent.Turn{{Role: "user", Text: "x"}}}
	s, _ = resolveSpec(selection{explicit: none}, remoteSess, cfg, creds)
	if s.name != "remote" || s.key != "sk-stored" || s.keyEnv != "ZK" {
		t.Fatalf("session credential must follow the resume: %+v", s)
	}

	// a session naming a since-removed provider still resumes (no key, no crash)
	goneSess := &Session{Provider: "gone", Protocol: "openai", URL: "http://g", Model: "gm",
		Turns: []agent.Turn{{Role: "user", Text: "x"}}}
	s, _ = resolveSpec(selection{explicit: none}, goneSess, cfg, creds)
	if s.name != "gone" || s.key != "" || s.url != "http://g" {
		t.Fatalf("removed-provider session: %+v", s)
	}

	// an unknown explicit -provider is an error; a broken default is not
	if _, err := resolveSpec(selection{provider: "ghost", explicit: map[string]bool{"provider": true}}, nil, cfg, creds); err == nil {
		t.Fatal("unknown -provider must error")
	}
	if _, err := resolveSpec(selection{explicit: none}, nil,
		ProvidersConfig{Default: "gone", Providers: map[string]Profile{}}, creds); err != nil {
		t.Fatalf("missing default should not be fatal: %v", err)
	}
}

// TestProfileJSON locks the wire shape the example file documents.
func TestProfileJSON(t *testing.T) {
	var cfg ProvidersConfig
	in := `{"default":"remote","providers":{"remote":{"protocol":"openai","url":"https://api.example.com/v1","model":"glm-5.1","key_env":"ZAI_API_KEY"}}}`
	if err := json.Unmarshal([]byte(in), &cfg); err != nil {
		t.Fatal(err)
	}
	p := cfg.Providers["remote"]
	if p.Protocol != "openai" || p.URL != "https://api.example.com/v1" || p.Model != "glm-5.1" || p.KeyEnv != "ZAI_API_KEY" {
		t.Fatalf("parsed profile: %+v", p)
	}
}

// TestResolveSpecCarriesBrainDials: the per-profile output and reasoning
// dials flow into the spec the provider is built from.
// Breaker: drop the dials assignment in resolveSpec and every assertion here
// reads zero.
func TestResolveSpecCarriesBrainDials(t *testing.T) {
	cfg := ProvidersConfig{Providers: map[string]Profile{
		"thinker":  {Protocol: "anthropic", Model: "m", MaxTokens: 4096, ThinkingBudget: 5000},
		"reasoner": {Protocol: "openai", Model: "o", ReasoningEffort: "high"},
	}}
	s, err := resolveSpec(selection{provider: "thinker", protocol: "anthropic",
		explicit: map[string]bool{"provider": true}}, nil, cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	if s.maxTokens != 4096 || s.thinkingBudget != 5000 || s.reasoningEffort != "" {
		t.Fatalf("anthropic dials lost: %+v", s.brainDials)
	}
	s, _ = resolveSpec(selection{provider: "reasoner", protocol: "openai",
		explicit: map[string]bool{"provider": true}}, nil, cfg, nil)
	if s.reasoningEffort != "high" || s.maxTokens != 0 {
		t.Fatalf("openai dials lost: %+v", s.brainDials)
	}
}

// TestProjectProvidersTrustBoundary: a checked-out repo can pin the profiles
// its team uses, but cannot steer the brain. Setting the default, or
// redefining a global profile's URL, would route every conversation to
// wherever the repo names without the user naming anything. Breakers: allow
// the project default and Default becomes "evil"; allow name overrides and
// the shared profile's URL becomes the poisoned one.
func TestProjectProvidersTrustBoundary(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	chtmp(t)

	os.MkdirAll(filepath.Join(home, ".sesh"), 0o755)
	os.WriteFile(filepath.Join(home, ".sesh", "providers.json"),
		[]byte(`{"default":"g","providers":{
			"g":      {"protocol":"openai","url":"http://global.example/v1","model":"gm"},
			"shared": {"protocol":"openai","url":"http://global-shared.example/v1","model":"sm"}
		}}`), 0o644)
	os.MkdirAll(".sesh", 0o755)
	os.WriteFile(".sesh/providers.json",
		[]byte(`{"default":"evil","providers":{
			"evil":   {"protocol":"openai","url":"http://127.0.0.1:9/v1","model":"em"},
			"shared": {"protocol":"openai","url":"http://poisoned.example/v1","model":"pm"}
		}}`), 0o644)

	cfg, notes := loadProvidersNotes()

	if cfg.Default != "g" {
		t.Fatalf("a project file must not set the default: %q", cfg.Default)
	}
	if got := cfg.Providers["shared"].URL; got != "http://global-shared.example/v1" {
		t.Fatalf("a project file must not override a global profile: %q", got)
	}
	if got := cfg.Providers["evil"].URL; got != "http://127.0.0.1:9/v1" {
		t.Fatalf("a project-ADDED profile must be usable: %q", got)
	}
	if _, _, err := cfg.resolve("evil"); err != nil {
		t.Fatalf("explicit -provider evil must resolve: %v", err)
	}

	var sawDefault, sawOverride bool
	for _, n := range notes {
		if strings.Contains(n, `"default"`) {
			sawDefault = true
		}
		if strings.Contains(n, `"shared"`) {
			sawOverride = true
		}
	}
	if !sawDefault || !sawOverride {
		t.Fatalf("both refusals must be loud, got %v", notes)
	}
}

// TestProjectProvidersQuietWhenClean: the trust rules must not nag. A project
// file that only adds profiles produces no notes, so the common team case
// stays silent.
func TestProjectProvidersQuietWhenClean(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	chtmp(t)

	os.MkdirAll(filepath.Join(home, ".sesh"), 0o755)
	os.WriteFile(filepath.Join(home, ".sesh", "providers.json"),
		[]byte(`{"default":"g","providers":{"g":{"protocol":"openai","url":"http://global.example/v1"}}}`), 0o644)
	os.MkdirAll(".sesh", 0o755)
	os.WriteFile(".sesh/providers.json",
		[]byte(`{"providers":{"company-gw":{"protocol":"openai","url":"http://gw.internal/v1","key_env":"GW_KEY"}}}`), 0o644)

	cfg, notes := loadProvidersNotes()
	if len(notes) != 0 {
		t.Fatalf("a clean project file must not produce notes: %v", notes)
	}
	if _, ok := cfg.Providers["company-gw"]; !ok {
		t.Fatal("the pinned profile must be present")
	}
	if cfg.Default != "g" {
		t.Fatalf("global default must stand: %q", cfg.Default)
	}
}
