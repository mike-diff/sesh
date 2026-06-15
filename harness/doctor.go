// sesh -doctor: check the whole configuration end to end without spending
// a single paid completion token. Config files parse, every provider's key is
// resolvable, every endpoint answers its models route, the statusline script
// runs, and the sessions directory is writable. Local endpoints additionally
// get a context-truncation probe (their tokens are free, and they are where
// silent truncation happens). Exit 0 when everything holds.
package harness

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/mike-diff/sesh/agent"
)

func runDoctor() int {
	ok := true
	pass := func(format string, a ...any) { fmt.Printf("  ok   "+format+"\n", a...) }
	warn := func(format string, a ...any) {
		fmt.Printf("%s  --   "+format+"%s\n", append([]any{dim}, append(a, reset)...)...)
	}
	failD := func(format string, a ...any) {
		ok = false
		fmt.Printf("%s  FAIL "+format+"%s\n", append([]any{red}, append(a, reset)...)...)
	}

	fmt.Println("config files")
	for _, p := range []string{providersPath(), ".sesh/providers.json"} {
		b, err := os.ReadFile(p)
		if err != nil {
			warn("%s: not present", p)
			continue
		}
		var probe ProvidersConfig
		if jerr := json.Unmarshal(b, &probe); jerr != nil {
			failD("%s: invalid JSON: %v", p, jerr)
		} else {
			pass("%s: %d providers", p, len(probe.Providers))
		}
	}

	cfg := loadProviders()
	creds := loadCredentials()
	fmt.Println("providers")
	if len(cfg.Providers) == 0 {
		failD("none configured (run /provider add)")
	}
	if cfg.Default != "" {
		if _, found := cfg.Providers[cfg.Default]; !found {
			failD("default %q does not exist", cfg.Default)
		}
	}
	names := cfg.names()
	sort.Strings(names)
	for _, name := range names {
		prof := cfg.Providers[name]
		key, keySrc := prof.Key, "inline key"
		if key == "" {
			if key = creds[name]; key != "" {
				keySrc = "stored credential"
			} else if prof.KeyEnv != "" {
				key, keySrc = os.Getenv(prof.KeyEnv), "env "+prof.KeyEnv
				if key == "" {
					keySrc += " (unset)"
				}
			} else {
				keySrc = "no key"
			}
		}
		nurl, nmodel := prof.URL, prof.Model
		if err := resolveDefaults(prof.Protocol, &nurl, &nmodel); err != nil {
			failD("%s: %v", name, err)
			continue
		}
		p, err := buildProvider(prof.Protocol, nurl, nmodel, key, prof.KeyEnv)
		if err != nil {
			failD("%s: %v (%s)", name, err, keySrc)
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		models, lerr := listModels(ctx, p)
		cancel()
		marker := ""
		if name == cfg.Default {
			marker = " (default)"
		}
		switch {
		case lerr != nil:
			failD("%s%s: %s · model %s · endpoint error: %v", name, marker, keySrc, nmodel, lerr)
		case len(models) == 0:
			warn("%s%s: %s · model %s · reachable, no models listed", name, marker, keySrc, nmodel)
		default:
			pass("%s%s: %s · model %s · reachable, %d models", name, marker, keySrc, nmodel, len(models))
		}

		// Local servers (ollama and friends) silently clamp prompts to their
		// configured context and drop the oldest tokens first, which quietly
		// eats the system prompt in long sessions. Catch it with a real call.
		if lerr == nil && isLocalEndpoint(nurl) {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			reported, sent, perr := probeContext(ctx, p)
			cancel()
			switch verdict := truncationVerdict(reported, sent); {
			case perr != nil:
				warn("%s: context probe inconclusive: %v", name, perr)
			case verdict != "":
				failD("%s: %s", name, verdict)
			case reported == 0:
				warn("%s: endpoint did not report usage; cannot verify its context window", name)
			default:
				pass("%s: context probe ok (~%d tokens accepted)", name, reported)
			}
		}
	}

	// Project-local mods are personal customization; if git would commit
	// them, say so before someone's statusline script lands in a PR.
	if fi, err := os.Stat(".sesh"); err == nil && fi.IsDir() {
		if _, gerr := os.Stat(".git"); gerr == nil {
			if exec.Command("git", "check-ignore", "-q", ".sesh").Run() != nil {
				failD(".sesh/ exists but is not gitignored; add '.sesh/' to .gitignore so personal mods stay out of commits")
			} else {
				pass(".sesh/: present and gitignored")
			}
		}
	}

	fmt.Println("tool mods")
	if entries, derr := os.ReadDir(toolModsDir()); derr != nil || len(entries) == 0 {
		warn("none (%s: executables there become agent tools; see ~/.sesh/tools/README.md)", toolModsDir())
	} else {
		taken := map[string]bool{"task": true, "recall": true}
		for _, t := range builtinTools(false, nil) {
			taken[t.Def.Name] = true
		}
		mods, notes := loadToolMods(taken)
		for _, t := range mods {
			kind := "read-only"
			if mutating[t.Def.Name] {
				kind = "mutating (gated)"
			}
			pass("%s: %s · %s", t.Def.Name, kind, compact(t.Def.Description))
		}
		for _, n := range notes {
			failD("%s", n)
		}
	}

	fmt.Println("engines")
	skills, skillNotes := loadSkills()
	for _, e := range skills {
		tag := ""
		if e.project {
			tag = " [project]"
		}
		pass("skill %s%s: %s", e.name, tag, compact(e.desc))
	}
	for _, n := range skillNotes {
		failD("%s", n)
	}
	mcpConf, mcpNotes := loadMCPConfig()
	mcpCache := readMCPManifest()
	mcpNames := make([]string, 0, len(mcpConf))
	for name := range mcpConf {
		mcpNames = append(mcpNames, name)
	}
	sort.Strings(mcpNames)
	for _, name := range mcpNames {
		if tools := mcpCache.Servers[name]; len(tools) > 0 {
			pass("mcp %s: %d tools discovered", name, len(tools))
		} else {
			warn("mcp %s: tools not yet discovered (the first call in a session discovers them)", name)
		}
	}
	for _, n := range mcpNotes {
		failD("%s", n)
	}
	if len(skills) == 0 && len(mcpConf) == 0 && len(skillNotes) == 0 && len(mcpNotes) == 0 {
		warn("none (~/.sesh/skills/ and ~/.sesh/mcp.json activate the skill and mcp tools; see ~/.sesh/skills/README.md)")
	}

	fmt.Println("statusline")
	found := false
	for _, p := range []string{".sesh/statusline", filepath.Join(os.Getenv("HOME"), ".sesh", "statusline")} {
		fi, err := os.Stat(p)
		if err != nil {
			continue
		}
		found = true
		if fi.Mode()&0o111 == 0 {
			failD("%s: present but not executable (chmod +x)", p)
			continue
		}
		if line, err := runStatusScript(p, statusInfo{Provider: "doctor", Model: "check"}); err != nil {
			failD("%s: %v", p, err)
		} else {
			pass("%s: %q", p, line)
		}
	}
	if !found {
		warn("no statusline script; using the built-in default")
	}

	fmt.Println("sessions")
	if err := os.MkdirAll(sessionsDir(), 0o755); err != nil {
		failD("%s: %v", sessionsDir(), err)
	} else if probe := filepath.Join(sessionsDir(), ".doctor"); os.WriteFile(probe, nil, 0o644) != nil {
		failD("%s: not writable", sessionsDir())
	} else {
		os.Remove(probe)
		pass("%s: writable, %d sessions", sessionsDir(), len(allSessions()))
	}

	if ok {
		fmt.Println("\nall checks passed")
		return 0
	}
	fmt.Println("\nsome checks failed")
	return 1
}

// isLocalEndpoint reports whether the URL points at this machine, where a
// completion costs nothing and the truncation probe is safe to run.
func isLocalEndpoint(url string) bool {
	for _, h := range []string{"localhost", "127.0.0.1", "0.0.0.0", "[::1]"} {
		if strings.Contains(url, h) {
			return true
		}
	}
	return false
}

// probePadWords sizes the truncation probe comfortably past ollama's 4096
// default while staying cheap to prefill.
const probePadWords = 6000

// probeContext sends a prompt of roughly probePadWords tokens and returns how
// many the endpoint says it received. A server that clamps reports its ceiling
// instead of the real size.
func probeContext(ctx context.Context, p agent.Provider) (reported, sent int, err error) {
	words := []string{"alpha", "bravo", "carbon", "delta", "ember", "falcon", "garnet", "harbor"}
	var pad strings.Builder
	for i := 0; i < probePadWords; i++ {
		pad.WriteString(words[i%len(words)])
		pad.WriteByte(' ')
	}
	history := []agent.Turn{{Role: "user",
		Text: "Reply with the single word OK and nothing else. Ignore the padding that follows.\n" + pad.String()}}
	reply, err := p.Chat(ctx, "You are a configuration probe. Answer with exactly: OK", history, nil,
		func(string) {}, func(string) {})
	if err != nil {
		return 0, probePadWords, err
	}
	return reply.Usage.Input + reply.Usage.CacheRead, probePadWords, nil
}

// truncationVerdict turns probe numbers into a diagnosis. sent counts padding
// words, a floor on the real token count, so a healthy endpoint reports at
// least that; one that reports meaningfully less is clamping its window.
func truncationVerdict(reported, sent int) string {
	if reported == 0 || reported >= sent*9/10 {
		return ""
	}
	return fmt.Sprintf("prompt truncated: sent ~%d tokens, endpoint accepted %d. "+
		"Long sessions silently lose their oldest context (including the system prompt). "+
		"For ollama: raise OLLAMA_CONTEXT_LENGTH on the server, or create a variant model "+
		"(Modelfile: FROM <model> + PARAMETER num_ctx 32768)", sent, reported)
}

// listModels asks the provider for its model list when it supports discovery.
func listModels(ctx context.Context, p interface{}) ([]string, error) {
	lister, can := p.(interface {
		ListModels(context.Context) ([]string, error)
	})
	if !can {
		return nil, nil
	}
	return lister.ListModels(ctx)
}
