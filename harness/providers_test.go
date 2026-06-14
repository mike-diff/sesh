package harness

import (
	"encoding/json"
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
	// a project config overrides the default and one profile, adds another,
	// and inherits the rest.
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
