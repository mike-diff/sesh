// Provider profiles: the named brains a user switches between. They live in
// providers.json and are selected with -provider or the /provider command.
//
// A profile is just an endpoint: protocol, URL, a default model, and a key.
// Models are not listed here; the harness discovers them from the endpoint (see
// provider.ListModels), so /model lists and switches among whatever a provider
// serves. This is the provider side of the same user-space config idea as the
// SYSTEM.md steering chain: behavior lives in files you edit, not in the binary.
package harness

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Profile is one named provider. The key may be inline (key) or, preferably,
// the name of an env var that holds it (key_env), which keeps the secret out of
// the file so a project providers.json is safe to commit. Context is the
// model's context window in tokens; when set, the harness tracks context
// pressure and hands off near the limit (pressure management is not optional;
// the tuning.json dials move its thresholds).
type Profile struct {
	Protocol string `json:"protocol"`
	URL      string `json:"url,omitempty"`
	Model    string `json:"model,omitempty"`
	Key      string `json:"key,omitempty"`
	KeyEnv   string `json:"key_env,omitempty"`
	Context  int    `json:"context,omitempty"`
	// CustomModel is one user-added model the endpoint did not list, remembered
	// per provider so it persists and stays in /model.
	CustomModel string `json:"custom_model,omitempty"`
	// Vision is the tri-state override for whether this profile's model can see
	// images: nil leaves it to the model-name heuristic, true or false forces it.
	// It is the escape hatch named in the paste-blocked guidance.
	Vision *bool `json:"vision,omitempty"`
	// NoTools, when true, sends no tool definitions to this profile's model. A
	// tools-less model (such as a local vision model used only to read images)
	// rejects any tools array, so the agent runs as plain conversation.
	NoTools bool `json:"no_tools,omitempty"`
}

// ProvidersConfig is a parsed providers.json: a set of named profiles and the
// one to use when none is named on the command line.
type ProvidersConfig struct {
	Default   string             `json:"default"`
	Providers map[string]Profile `json:"providers"`
}

// names lists the profile names in sorted order, for listings and error hints.
func (c ProvidersConfig) names() []string {
	out := make([]string, 0, len(c.Providers))
	for n := range c.Providers {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// overlay folds another config on top of this one: a non-empty default wins,
// and profiles merge by name. This lets a project providers.json add or
// override individual brains while inheriting the rest from the global one.
func (c *ProvidersConfig) overlay(got ProvidersConfig) {
	if got.Default != "" {
		c.Default = got.Default
	}
	if c.Providers == nil {
		c.Providers = map[string]Profile{}
	}
	for name, prof := range got.Providers {
		c.Providers[name] = prof
	}
}

// resolve looks up a profile by name, or the configured default when name is
// empty. The error names what is available so a typo is easy to fix.
func (c ProvidersConfig) resolve(name string) (string, Profile, error) {
	if name == "" {
		name = c.Default
	}
	if name == "" {
		return "", Profile{}, fmt.Errorf("no provider named and no default set in providers.json")
	}
	prof, ok := c.Providers[name]
	if !ok {
		avail := strings.Join(c.names(), ", ")
		if avail == "" {
			avail = "none configured"
		}
		return "", Profile{}, fmt.Errorf("unknown provider %q (available: %s)", name, avail)
	}
	return name, prof, nil
}

// providerSpec is the fully layered provider choice, ready to build.
type providerSpec struct {
	name                 string // profile name; "" when no profile applied
	protocol, url, model string
	key, keyEnv          string
	ctxLimit             int
}

// selection carries the raw command-line choice into resolveSpec. explicit
// records which flags the user actually typed, so layering knows what wins.
type selection struct {
	provider, protocol, url, model string
	explicit                       map[string]bool
}

// resolveSpec layers the provider settings, weakest first:
//  1. the selected profile in providers.json (-provider, or its default)
//  2. a resumed session's saved brain (unless -provider was given)
//  3. explicit command-line flags
//
// The only error is a provider named explicitly on the command line that does
// not exist; a missing or unresolvable default is not fatal.
func resolveSpec(sel selection, sess *Session, cfg ProvidersConfig, creds map[string]string) (providerSpec, error) {
	s := providerSpec{protocol: sel.protocol, url: sel.url, model: sel.model}
	if name, prof, err := cfg.resolve(sel.provider); err == nil {
		s.name = name
		if !sel.explicit["protocol"] && prof.Protocol != "" {
			s.protocol = prof.Protocol
		}
		if !sel.explicit["url"] && prof.URL != "" {
			s.url = prof.URL
		}
		if !sel.explicit["model"] && prof.Model != "" {
			s.model = prof.Model
		}
		s.keyEnv = prof.KeyEnv
		s.key = prof.Key // inline key wins; otherwise the saved credential
		if s.key == "" {
			s.key = creds[name]
		}
		s.ctxLimit = prof.Context
	} else if sel.explicit["provider"] {
		return s, err
	}
	// A resumed session brings its own brain along, unless -provider overrides.
	if sess != nil && len(sess.Turns) > 0 && !sel.explicit["provider"] {
		if !sel.explicit["protocol"] && sess.Protocol != "" {
			s.protocol = sess.Protocol
		}
		if !sel.explicit["url"] && sess.URL != "" {
			s.url = sess.URL
		}
		if !sel.explicit["model"] && sess.Model != "" {
			s.model = sess.Model
		}
		// The session also remembers which provider it was, so its credential
		// follows the resume. Without this, resuming a keyed session under a
		// different default profile authenticates with the wrong (or no) key.
		if sess.Provider != "" {
			s.name = sess.Provider
			s.key, s.keyEnv = "", ""
			s.ctxLimit = 0
			if prof, ok := cfg.Providers[sess.Provider]; ok {
				s.key, s.keyEnv = prof.Key, prof.KeyEnv
				s.ctxLimit = prof.Context
			}
			if s.key == "" {
				s.key = creds[sess.Provider]
			}
		}
	}
	return s, nil
}

func providersPath() string {
	return filepath.Join(os.Getenv("HOME"), ".sesh", "providers.json")
}

// loadProviders reads the global providers.json, then overlays the project one.
// A missing or unparseable file is skipped silently: the harness still works on
// flags alone, so providers.json is purely additive.
func loadProviders() ProvidersConfig {
	cfg := ProvidersConfig{Providers: map[string]Profile{}}
	for _, p := range []string{
		providersPath(),        // global
		".sesh/providers.json", // project overlay
	} {
		b, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		var got ProvidersConfig
		if json.Unmarshal(b, &got) != nil {
			continue
		}
		cfg.overlay(got)
	}
	return cfg
}

// loadGlobalProviders reads only the global file, the one /provider add and
// /provider remove write back to. (The project overlay, if any, is read-only
// from the harness's point of view.)
func loadGlobalProviders() ProvidersConfig {
	cfg := ProvidersConfig{Providers: map[string]Profile{}}
	if b, err := os.ReadFile(providersPath()); err == nil {
		json.Unmarshal(b, &cfg)
		if cfg.Providers == nil {
			cfg.Providers = map[string]Profile{}
		}
	}
	return cfg
}

// saveGlobalProviders writes the global providers.json atomically. It holds no
// secrets (keys live encrypted in credentials.json), so 0644 is fine.
func saveGlobalProviders(cfg ProvidersConfig) error {
	if err := os.MkdirAll(filepath.Dir(providersPath()), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	tmp := providersPath() + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, providersPath())
}
