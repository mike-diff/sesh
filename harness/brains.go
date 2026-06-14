package harness

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"sesh/agent"
	"sesh/provider"
)

// ---------------------------------------------------------------------------
// Provider selection: same harness, any brain. A resumed session brings its
// saved protocol/url/model unless overridden on the command line.
// ---------------------------------------------------------------------------

func resolveDefaults(protocol string, url, model *string) error {
	switch protocol {
	case "anthropic":
		if *url == "" {
			*url = "https://api.anthropic.com"
		}
		if *model == "" {
			*model = "claude-opus-4-8"
		}
	case "openai":
		if *url == "" {
			*url = "https://api.openai.com/v1"
		}
		if *model == "" {
			return fmt.Errorf("-model is required with -protocol openai")
		}
	default:
		return fmt.Errorf("unknown protocol %q (anthropic or openai)", protocol)
	}
	return nil
}

// buildProvider resolves the API key and constructs the adapter. The key is
// taken in this order: the inline key, the env var named by keyEnv, API_KEY,
// then the protocol's conventional env var. Both key and keyEnv may be empty
// (local servers need no key).
func buildProvider(protocol, url, model, key, keyEnv string) (agent.Provider, error) {
	if key == "" && keyEnv != "" {
		key = os.Getenv(keyEnv)
	}
	if key == "" {
		key = os.Getenv("API_KEY")
	}
	hint := func(def string) string {
		if keyEnv != "" {
			return keyEnv
		}
		return def + " (or API_KEY)"
	}
	switch protocol {
	case "anthropic":
		if key == "" {
			key = os.Getenv("ANTHROPIC_API_KEY")
		}
		if key == "" && strings.Contains(url, "api.anthropic.com") {
			return nil, fmt.Errorf("set %s", hint("ANTHROPIC_API_KEY"))
		}
		return provider.Anthropic{BaseURL: url, Key: key, Model: model}, nil
	default: // resolveDefaults already rejected anything but openai
		if key == "" {
			key = os.Getenv("OPENAI_API_KEY")
		}
		if key == "" && strings.Contains(url, "api.openai.com") {
			return nil, fmt.Errorf("set %s", hint("OPENAI_API_KEY"))
		}
		return provider.OpenAI{BaseURL: url, Key: key, Model: model}, nil
	}
}

// discoverModels asks the endpoint what models it serves, for the /model
// command, plus any context lengths the endpoint publishes (vLLM, OpenRouter,
// and gateways do; the OpenAI API and ollama do not). Best-effort: any error
// (including an endpoint with no models route, or no key) yields nothing and
// the harness carries on.
func discoverModels(p agent.Provider) ([]string, map[string]int) {
	lister, ok := p.(interface {
		ListModelInfos(context.Context) ([]provider.ModelInfo, error)
	})
	if !ok {
		return nil, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	infos, err := lister.ListModelInfos(ctx)
	if err != nil {
		return nil, nil
	}
	models := make([]string, 0, len(infos))
	ctxs := map[string]int{}
	for _, m := range infos {
		models = append(models, m.ID)
		if m.Context > 0 {
			ctxs[m.ID] = m.Context
		}
	}
	return models, ctxs
}
