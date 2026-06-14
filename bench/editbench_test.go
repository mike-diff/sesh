// The edit/search bench: measures how well a model wields the edit and search
// tools on tasks engineered to exercise exactly the failure modes the research names:
// whitespace/indent drift, invisible CRLF/BOM characters, ambiguous targets,
// multi-site and cross-file renames, deep files, file creation, recovery from a
// stale quote, and search discrimination.
//
// It drives the LIBRARY loop (agent.Run with the real built-in toolset via
// harness.BenchTools), not the binary, one task at a time in a fresh temp
// workdir seeded by a fixture GENERATOR: the fixtures are embedded Go strings
// written at runtime, so CRLF, BOM, and tab bytes cannot be normalized by git.
// Each task has a deterministic verifier over the resulting tree (file-state
// predicates, or `go build` inside the fixture).
//
// Opt-in, exactly like the retention rig (harness/rig_test.go gates on SESH_RIG)
// and the retention bench (bench_test.go gates on SESH_BENCH):
//
//	SESH_EDITBENCH=1 go test ./bench -run TestEditBench -v -timeout 30m
//	SESH_EDITBENCH_PROVIDER=<name>   (default: the configured default provider)
//	SESH_EDITBENCH_MODEL=<id>        (default: the provider's model)
//
// The rig drives the shipped toolset (the 2026-06 matrix that chose the
// hardened edit design; the losing arms were
// deleted). It never runs unless SESH_EDITBENCH is set, so go test ./...
// stays offline.
//
// The KEY is resolved from the environment first (the profile's inline key or
// key_env, then ANTHROPIC_API_KEY / OPENAI_API_KEY / API_KEY), falling back to
// the encrypted credential store via harness.BenchCredential. The rig skips
// with a clear message if no key is reachable.
package bench

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"sesh/agent"
	"sesh/harness"
	"sesh/provider"
)

// ---------------------------------------------------------------------------
// Provider resolution: read providers.json directly (like sandboxProviders in
// bench_test.go), then build the adapter from the provider package with an
// env-resolved key. No harness internals, so the bench package stays a plain
// library consumer.
// ---------------------------------------------------------------------------

// editbenchProvider resolves an agent.Provider and its model id from the user's
// providers.json plus env overrides. It skips (not fails) when nothing is
// configured: a missing provider is a setup gap, not a bug under test.
func editbenchProvider(t *testing.T) (agent.Provider, string) {
	t.Helper()
	home := os.Getenv("HOME")
	b, err := os.ReadFile(filepath.Join(home, ".sesh", "providers.json"))
	if err != nil {
		t.Skipf("no providers.json under %s/.sesh; configure a provider first: %v", home, err)
	}
	var pf providersFile
	if err := json.Unmarshal(b, &pf); err != nil {
		t.Fatalf("parse providers.json: %v", err)
	}

	name := os.Getenv("SESH_EDITBENCH_PROVIDER")
	if name == "" {
		name = pf.Default
	}
	if name == "" && len(pf.Providers) == 1 {
		for only := range pf.Providers {
			name = only
		}
	}
	prof, ok := pf.Providers[name]
	if !ok {
		t.Skipf("provider %q not in providers.json; set SESH_EDITBENCH_PROVIDER", name)
	}

	protocol, _ := prof["protocol"].(string)
	url, _ := prof["url"].(string)
	model, _ := prof["model"].(string)
	if m := os.Getenv("SESH_EDITBENCH_MODEL"); m != "" {
		model = m
	}
	switch protocol {
	case "anthropic":
		if url == "" {
			url = "https://api.anthropic.com"
		}
		if model == "" {
			model = "claude-opus-4-8"
		}
	case "openai":
		if url == "" {
			url = "https://api.openai.com/v1"
		}
		if model == "" {
			t.Skip("openai provider needs a model; set SESH_EDITBENCH_MODEL")
		}
	default:
		t.Skipf("unknown protocol %q in profile %q", protocol, name)
	}

	key := envKey(prof, protocol)
	if key == "" {
		key = harness.BenchCredential(name) // the encrypted store, e.g. zai
	}
	if key == "" && (strings.Contains(url, "api.anthropic.com") || strings.Contains(url, "api.openai.com")) {
		t.Skip("no API key reachable from the environment or the credential store; set the provider's key_env, or ANTHROPIC_API_KEY/OPENAI_API_KEY/API_KEY")
	}

	if protocol == "anthropic" {
		return provider.Anthropic{BaseURL: url, Key: key, Model: model}, model
	}
	return provider.OpenAI{BaseURL: url, Key: key, Model: model}, model
}

// envKey resolves the API key from the environment in buildProvider's order:
// the profile's inline key, its key_env, API_KEY, then the protocol default.
func envKey(prof map[string]any, protocol string) string {
	if k, _ := prof["key"].(string); k != "" {
		return k
	}
	if ke, _ := prof["key_env"].(string); ke != "" {
		if v := os.Getenv(ke); v != "" {
			return v
		}
	}
	if v := os.Getenv("API_KEY"); v != "" {
		return v
	}
	switch protocol {
	case "anthropic":
		return os.Getenv("ANTHROPIC_API_KEY")
	case "openai":
		return os.Getenv("OPENAI_API_KEY")
	}
	return ""
}

// ---------------------------------------------------------------------------
// Fixture generator. Each fixture is built from embedded strings at runtime so
// CRLF/BOM/tab bytes survive (git would normalize them in committed files).
// ---------------------------------------------------------------------------

const bomBytes = "\uFEFF"

// writeFixture writes one file (content verbatim) under dir, creating parents.
func writeFixture(t *testing.T, dir, name, content string) {
	t.Helper()
	full := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// goModFixture is the minimal module every buildable fixture shares.
const goModFixture = "module fixture\n\ngo 1.22\n"

// fiveSiteFile uses oldName at five call sites in one file: the multi-site
// rename target. Buildable on its own.
func fiveSiteFile(oldName string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "package main\n\nfunc %s(x int) int { return x + 1 }\n\n", oldName)
	b.WriteString("func main() {\n")
	for i := 0; i < 5; i++ {
		fmt.Fprintf(&b, "\t_ = %s(%d)\n", oldName, i)
	}
	b.WriteString("}\n")
	return b.String()
}

// tabbedTrailingFile has a tab-indented block whose target line carries trailing
// spaces: the whitespace-drift target. The `value := 41` line ends in three
// spaces, so a model quoting it cleanly relies on whitespace-tolerant matching.
func tabbedTrailingFile() string {
	return "package main\n\nfunc run() {\n\tif true {\n\t\tvalue := 41   \n\t\t_ = value\n\t}\n}\n"
}

// deepFile generates a ~1500-line buildable file with a unique marker deep
// inside, the needle-in-a-haystack target.
func deepFile(marker string) string {
	var b strings.Builder
	b.WriteString("package main\n\n")
	for i := 0; i < 740; i++ {
		fmt.Fprintf(&b, "func f%d() int { return %d }\n", i, i)
	}
	fmt.Fprintf(&b, "\nfunc %s() int { return 0 }\n\n", marker)
	for i := 740; i < 1480; i++ {
		fmt.Fprintf(&b, "func f%d() int { return %d }\n", i, i)
	}
	b.WriteString("\nfunc main() {}\n")
	return b.String()
}

// ---------------------------------------------------------------------------
// Tasks. Each is a prompt plus a deterministic verifier over the workdir tree.
// ---------------------------------------------------------------------------

type task struct {
	name   string
	setup  func(t *testing.T, dir string) // writes the fixture files
	prompt string
	verify func(dir string) error // nil = success
}

// fileContains asserts substr appears in the named file.
func fileContains(dir, name, substr string) error {
	b, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		return err
	}
	if !strings.Contains(string(b), substr) {
		return fmt.Errorf("%s does not contain %q", name, substr)
	}
	return nil
}

// fileAbsent asserts substr does NOT appear in the named file.
func fileAbsent(dir, name, substr string) error {
	b, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		return err
	}
	if strings.Contains(string(b), substr) {
		return fmt.Errorf("%s still contains %q", name, substr)
	}
	return nil
}

// goBuilds runs `go build ./...` in dir; a clean build is the strongest
// verifier (it proves every rename site was updated, not just the declaration).
func goBuilds(dir string) error {
	cmd := exec.Command("go", "build", "./...")
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("go build failed: %v\n%s", err, out)
	}
	return nil
}

// fileHasExactBytes asserts the file's bytes equal want exactly: the CRLF/BOM
// preservation check, where any normalization is a failure.
func fileHasExactBytes(dir, name, want string) error {
	b, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		return err
	}
	if string(b) != want {
		return fmt.Errorf("%s bytes differ.\n got: %q\nwant: %q", name, string(b), want)
	}
	return nil
}

func editBenchTasks() []task {
	return []task{
		{
			name: "multi-site-rename",
			setup: func(t *testing.T, dir string) {
				writeFixture(t, dir, "go.mod", goModFixture)
				writeFixture(t, dir, "main.go", fiveSiteFile("compute"))
			},
			prompt: "In main.go, rename the function `compute` to `calculate` everywhere it is used (the declaration and all call sites). Then make sure the package builds.",
			verify: func(dir string) error {
				if err := fileAbsent(dir, "main.go", "compute"); err != nil {
					return err
				}
				if err := fileContains(dir, "main.go", "calculate"); err != nil {
					return err
				}
				return goBuilds(dir)
			},
		},
		{
			name: "cross-file-rename",
			setup: func(t *testing.T, dir string) {
				writeFixture(t, dir, "go.mod", goModFixture)
				writeFixture(t, dir, "core.go", "package main\n\nfunc Helper() string { return \"x\" }\n")
				writeFixture(t, dir, "a.go", "package main\n\nfunc useA() string { return Helper() }\n")
				writeFixture(t, dir, "b.go", "package main\n\nfunc useB() string { return Helper() + Helper() }\n\nfunc main() { _ = useA(); _ = useB() }\n")
			},
			prompt: "Rename the function `Helper` to `Format` across the whole project (it is declared in one file and called in others). Search first to find every reference, then update them all and confirm the build is clean.",
			verify: func(dir string) error {
				for _, f := range []string{"core.go", "a.go", "b.go"} {
					if err := fileAbsent(dir, f, "Helper"); err != nil {
						return err
					}
				}
				return goBuilds(dir)
			},
		},
		{
			name: "tab-trailing-whitespace",
			setup: func(t *testing.T, dir string) {
				writeFixture(t, dir, "go.mod", goModFixture)
				writeFixture(t, dir, "run.go", tabbedTrailingFile())
			},
			prompt: "In run.go there is a line that sets `value` to 41 inside a tab-indented block. Change the 41 to 42. Do not touch anything else.",
			verify: func(dir string) error {
				if err := fileContains(dir, "run.go", "value := 42"); err != nil {
					return err
				}
				return fileAbsent(dir, "run.go", "value := 41")
			},
		},
		{
			name: "crlf-and-bom",
			setup: func(t *testing.T, dir string) {
				writeFixture(t, dir, "crlf.txt", "first line\r\nTODO: change me\r\nlast line\r\n")
				writeFixture(t, dir, "bom.txt", bomBytes+"alpha\nbeta\ngamma\n")
			},
			prompt: "Two edits: in crlf.txt replace the text `change me` with `changed`. In bom.txt replace `beta` with `BETA`. Make only those changes.",
			verify: func(dir string) error {
				// CRLF file: edited content, line endings preserved.
				if err := fileHasExactBytes(dir, "crlf.txt", "first line\r\nTODO: changed\r\nlast line\r\n"); err != nil {
					return err
				}
				// BOM file: edited content, BOM preserved.
				return fileHasExactBytes(dir, "bom.txt", bomBytes+"alpha\nBETA\ngamma\n")
			},
		},
		{
			name: "disambiguate-repeated",
			setup: func(t *testing.T, dir string) {
				writeFixture(t, dir, "config.txt", "host = localhost\nport = 8080\nname = service\nport = 9090\nretries = 3\nport = 7070\n")
			},
			prompt: "In config.txt there are three `port = ` lines: 8080, 9090, and 7070. Change ONLY the middle one (9090) to 9091. Leave the other two exactly as they are.",
			verify: func(dir string) error {
				want := "host = localhost\nport = 8080\nname = service\nport = 9091\nretries = 3\nport = 7070\n"
				return fileHasExactBytes(dir, "config.txt", want)
			},
		},
		{
			name: "deep-file-edit",
			setup: func(t *testing.T, dir string) {
				writeFixture(t, dir, "go.mod", goModFixture)
				writeFixture(t, dir, "big.go", deepFile("targetMarker"))
			},
			prompt: "In big.go there is exactly one function named `targetMarker`. Change its body so it returns 7 instead of 0. Keep the package building.",
			verify: func(dir string) error {
				if err := fileContains(dir, "big.go", "func targetMarker() int { return 7 }"); err != nil {
					return err
				}
				return goBuilds(dir)
			},
		},
		{
			name: "create-and-reference",
			setup: func(t *testing.T, dir string) {
				writeFixture(t, dir, "go.mod", goModFixture)
				writeFixture(t, dir, "main.go", "package main\n\nfunc main() { _ = greeting() }\n")
			},
			prompt: "Create a new file greet.go in package main that defines `func greeting() string { return \"hi\" }`, so that main.go (which already calls greeting()) builds. Then verify the build.",
			verify: func(dir string) error {
				if err := fileContains(dir, "greet.go", "func greeting() string"); err != nil {
					return err
				}
				return goBuilds(dir)
			},
		},
		{
			name: "recovery-stale-quote",
			setup: func(t *testing.T, dir string) {
				// The actual line differs from what the prompt quotes, forcing a
				// first-attempt miss the model must recover from.
				writeFixture(t, dir, "settings.go", "package main\n\nvar timeout = 30 // seconds\n\nfunc main() { _ = timeout }\n")
				writeFixture(t, dir, "go.mod", goModFixture)
			},
			prompt: "In settings.go change the timeout. The current line reads `var timeout = 60 // secs` and I want it to be 120. Update it.",
			verify: func(dir string) error {
				// Success is the timeout becoming 120, however the model phrased
				// the final edit, with the package still building.
				if err := fileContains(dir, "settings.go", "120"); err != nil {
					return err
				}
				if err := fileAbsent(dir, "settings.go", "= 30"); err != nil {
					return err
				}
				return goBuilds(dir)
			},
		},
		{
			name: "search-discrimination",
			setup: func(t *testing.T, dir string) {
				writeFixture(t, dir, "handlers.go", "package svc\n\nfunc GetUser() {}\nfunc GetOrder() {}\nfunc DeleteUser() {}\n")
				writeFixture(t, dir, "config_loader.go", "package svc\n\nfunc Load() {}\n")
				writeFixture(t, dir, "notes.md", "GetUser and GetOrder are read handlers.\n")
			},
			prompt: "Two questions, answer both in your final message. (1) Which functions match the pattern `Get(User|Order)` in the Go source (give their names)? (2) Which file in this project has a name ending in `_loader.go`? Use search to find out, do not guess.",
			verify: func(dir string) error {
				// A read-only discrimination task: the tree must be unchanged and
				// the answer is graded from the transcript by the caller (it gets
				// the final text). We only verify nothing was mutated here.
				if err := fileContains(dir, "handlers.go", "func GetUser() {}"); err != nil {
					return err
				}
				return fileContains(dir, "config_loader.go", "func Load() {}")
			},
		},
		{
			// The case-mismatch trap: the natural query is the wrong case, so a
			// case-sensitive search misses and the model must flail or recover;
			// smart-case hits first try.
			name: "case-mismatch-find",
			setup: func(t *testing.T, dir string) {
				writeFixture(t, dir, "go.mod", goModFixture)
				writeFixture(t, dir, "config.go", "package main\n\nvar DefaultPort = 8080\n\nfunc main() { _ = DefaultPort }\n")
				writeFixture(t, dir, "notes.txt", "service configuration notes\n")
			},
			prompt: "Somewhere in this project a default port of 8080 is defined; I believe the variable is called defaultport or something close. Use the search tool to locate it, then change the port to 9090.",
			verify: func(dir string) error {
				if err := fileContains(dir, "config.go", "9090"); err != nil {
					return err
				}
				if err := fileAbsent(dir, "config.go", "8080"); err != nil {
					return err
				}
				return goBuilds(dir)
			},
		},
		{
			// The fan-out trap: the obvious query has far more hits than the cap,
			// so the shaped search returns counts plus narrowing guidance while
			// the legacy search silently keeps an arbitrary first 50.
			name: "fanout-narrow",
			setup: func(t *testing.T, dir string) {
				writeFixture(t, dir, "go.mod", goModFixture)
				for i := 0; i < 30; i++ {
					writeFixture(t, dir, fmt.Sprintf("decoy_%02d.go", i),
						fmt.Sprintf("package main\n\n// handler %d wires the handler table\nfunc handler%02d() {}\n", i, i))
				}
				writeFixture(t, dir, "purge.go", "package main\n\n// the deletion handler\nfunc purgeTicket() string { return \"purging ticket\" }\n\nfunc main() { _ = purgeTicket() }\n")
			},
			prompt: "One function in this project returns the log message \"purging ticket\". Find it with the search tool (the word handler appears everywhere, so expect to narrow your query) and change the message to \"removing ticket\".",
			verify: func(dir string) error {
				if err := fileContains(dir, "purge.go", "removing ticket"); err != nil {
					return err
				}
				return goBuilds(dir)
			},
		},
	}
}

// ---------------------------------------------------------------------------
// Run accounting: counts derived from the returned history, never from tool
// internals.
// ---------------------------------------------------------------------------

type runStats struct {
	turns      int // assistant turns (model calls that produced a reply)
	editCalls  int
	failedEdit int
	searchCall int
	tokens     int // input+output across the run
}

// accountHistory walks the post-run history and tallies tool usage. An edit call
// is failed when its tool result is an error; retries are simply failedEdit
// (each failed edit is a retry the model had to recover from).
func accountHistory(history []agent.Turn, usage agent.Usage) runStats {
	var s runStats
	s.tokens = usage.Input + usage.Output
	// Map each tool call ID to its name so we can read the matching result.
	callName := map[string]string{}
	for _, turn := range history {
		switch turn.Role {
		case "assistant":
			if turn.Text != "" || len(turn.Calls) > 0 {
				s.turns++
			}
			for _, c := range turn.Calls {
				callName[c.ID] = c.Name
				switch c.Name {
				case "edit":
					s.editCalls++
				case "search":
					s.searchCall++
				}
			}
		case "tool":
			for _, r := range turn.Results {
				if callName[r.ID] == "edit" && r.IsError {
					s.failedEdit++
				}
			}
		}
	}
	return s
}

// ---------------------------------------------------------------------------
// The bench.
// ---------------------------------------------------------------------------

const editBenchSystem = "You are a precise coding agent. Use the provided tools to complete the task. " +
	"Prefer the edit tool over rewriting whole files. When a tool returns an error, read it and adjust rather than repeating the same call. " +
	"Verify with the bash tool when the task asks you to confirm a build. Stop when the task is done."

func TestEditBench(t *testing.T) {
	if os.Getenv("SESH_EDITBENCH") == "" {
		t.Skip("live edit/search bench; set SESH_EDITBENCH=1 (and optionally SESH_EDITBENCH_PROVIDER/SESH_EDITBENCH_MODEL)")
	}
	// SESH_EDITBENCH_DIFF overrides the diff_lines dial for A/B runs
	// (e.g. -1 to measure the no-diff control).
	if v := os.Getenv("SESH_EDITBENCH_DIFF"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n != 0 {
			harness.BenchSetDiffLines(n)
		}
	}

	p, model := editbenchProvider(t)
	t.Logf("== editbench model=%s", model)

	var (
		passed    int
		total     runStats
		taskCount int
	)
	for _, tk := range editBenchTasks() {
		taskCount++
		// Fresh workdir per task; chdir so the cwd-confined tools land inside it.
		work := t.TempDir()
		tk.setup(t, work)
		stats, ok, finalText := runOneTask(t, p, work, tk)

		passed += boolToInt(ok)
		total.turns += stats.turns
		total.editCalls += stats.editCalls
		total.failedEdit += stats.failedEdit
		total.searchCall += stats.searchCall
		total.tokens += stats.tokens

		// One greppable summary line per task.
		t.Logf("EDITBENCH task=%s pass=%t turns=%d edits=%d failed_edits=%d searches=%d tokens=%d",
			tk.name, ok, stats.turns, stats.editCalls, stats.failedEdit, stats.searchCall, stats.tokens)
		if !ok {
			t.Logf("   task %s final answer: %.200s", tk.name, strings.ReplaceAll(finalText, "\n", " "))
		}
	}

	// Final summary table.
	t.Logf("\n== EDITBENCH SUMMARY model=%s\n"+
		"   tasks: %d  passed: %d  pass_rate: %.0f%%\n"+
		"   edit calls: %d  failed edits: %d  searches: %d  turns: %d  tokens: %d",
		model, taskCount, passed, 100*float64(passed)/float64(taskCount),
		total.editCalls, total.failedEdit, total.searchCall, total.turns, total.tokens)
}

// runOneTask drives one task to completion in its own workdir and verifies the
// result. It returns the run accounting, whether the verifier passed, and the
// model's final text (used to grade the read-only search task and to log
// failures).
func runOneTask(t *testing.T, p agent.Provider, work string, tk task) (runStats, bool, string) {
	t.Helper()
	prev, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(work); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(prev)

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()

	tools := harness.BenchTools(false)
	history := []agent.Turn{{Role: "user", Text: tk.prompt}}
	out, usage, err := agent.Run(ctx, p, editBenchSystem, history, tools, agent.Hooks{})
	if err != nil {
		t.Logf("task %s run errored: %v", tk.name, err)
	}
	stats := accountHistory(out, usage)
	finalText := lastAssistantText(out)
	if err == nil && finalText == "" {
		t.Logf("task %s produced no assistant output (empty reply from the provider?)", tk.name)
	}

	verr := tk.verify(work)
	ok := verr == nil
	if verr != nil {
		t.Logf("   task %s verify: %v", tk.name, verr)
	}
	// The search-discrimination task also needs its answer checked against the
	// transcript, since the tree is read-only.
	if ok && tk.name == "search-discrimination" {
		ok = strings.Contains(finalText, "GetUser") &&
			strings.Contains(finalText, "GetOrder") &&
			strings.Contains(finalText, "config_loader.go")
	}
	return stats, ok, finalText
}

func lastAssistantText(turns []agent.Turn) string {
	for i := len(turns) - 1; i >= 0; i-- {
		if turns[i].Role == "assistant" && turns[i].Text != "" {
			return turns[i].Text
		}
	}
	return ""
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
