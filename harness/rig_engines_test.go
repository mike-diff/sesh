// The engines rig: measures whether the manifest preambles (the model-facing
// prompts the skill and mcp tools carry in their descriptions) actually steer
// a live model. Two questions:
//
//	triggering: given a task matching a skill's description, does the model
//	load that skill before attempting the task, and refrain on tasks no
//	skill covers?
//
//	routing: can the model drive the mcp dispatcher (server, tool, args)
//	correctly from the manifest line alone, and leave it alone otherwise?
//
// Like the retention rig, this is evidence, not pass/fail: scores are logged
// per task and the preamble wording is tuned only on what these runs show.
//
//	SESH_RIG=1 go test -run TestRigEngine -v -timeout 30m
//	SESH_RIG_PROVIDER=<name> SESH_RIG_MODEL=<id>  (default: configured default)
//	SESH_RIG_TRIALS=<n>                           (default 3 per task)
package harness

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"sesh/agent"
)

// recordedCall is one tool invocation observed during a run.
type recordedCall struct {
	tool string
	raw  string
}

// recording wraps a tool so every invocation lands in log before running.
func recording(t agent.Tool, log *[]recordedCall) agent.Tool {
	inner := t.Run
	t.Run = func(ctx context.Context, raw json.RawMessage) (string, bool) {
		*log = append(*log, recordedCall{t.Def.Name, string(raw)})
		return inner(ctx, raw)
	}
	return t
}

// rigSystem deliberately says nothing about skills or MCP: triggering must
// ride on the tool descriptions alone, because that is what ships.
const rigSystem = "You are a coding agent. Complete the user's request. " +
	"Use the available tools when they fit; answer directly when none do."

// observationTools is the read-only slice of the builtin toolset. The
// triggering experiment includes them because a real session always has
// them: a model with only the skill tool pokes it out of desperation on
// file tasks, which inflates false-trigger counts (observed in run 1).
func observationTools() []agent.Tool {
	var out []agent.Tool
	for _, t := range builtinTools(false) {
		switch t.Def.Name {
		case "read", "search", "loc":
			out = append(out, t)
		}
	}
	return out
}

// engineRun executes one task against the toolset and reports the calls made
// and the final answer.
func engineRun(t *testing.T, p agent.Provider, tools []agent.Tool, task string) ([]recordedCall, string) {
	t.Helper()
	var calls []recordedCall
	wrapped := make([]agent.Tool, len(tools))
	for i, tl := range tools {
		wrapped[i] = recording(tl, &calls)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	h := []agent.Turn{{Role: "user", Text: task}}
	out, _, err := agent.Run(ctx, p, rigSystem, h, wrapped, agent.Hooks{})
	if err != nil {
		t.Logf("  run errored: %v", err)
		return calls, ""
	}
	return calls, lastText(out)
}

func rigTrials() int {
	if n, err := strconv.Atoi(os.Getenv("SESH_RIG_TRIALS")); err == nil && n > 0 {
		return n
	}
	return 3
}

// skillCalled reports whether any recorded call loaded the named skill.
func skillCalled(calls []recordedCall, name string) bool {
	for _, c := range calls {
		if c.tool != "skill" {
			continue
		}
		var in struct{ Name string }
		if json.Unmarshal([]byte(c.raw), &in) == nil && (name == "" || in.Name == name) {
			return true
		}
	}
	return false
}

// rigSkillFixtures installs three skills whose descriptions follow the
// authoring protocol (imperative, intent-covering, with skip clauses).
func rigSkillFixtures(t *testing.T) {
	t.Helper()
	root := filepath.Join(os.Getenv("HOME"), ".sesh", "skills")
	write := func(name, desc, body string) {
		dir := filepath.Join(root, name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		md := fmt.Sprintf("---\nname: %s\ndescription: %s\n---\n%s", name, desc, body)
		if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(md), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("pdf-extraction",
		"Extract text and tables from PDF files and fill PDF forms. Use when the user works with PDF documents or wants data pulled out of a PDF. Skip for plain text or spreadsheet files.",
		"# PDF extraction\nBegin your final answer with the words: per the pdf-extraction skill.\nThen: 1) identify the target tables, 2) describe the extraction steps, 3) state what to verify.")
	write("csv-analysis",
		"Analyze CSV and tabular data files: summary statistics, derived columns, charts, cleaning. Use when the user has CSV, TSV, or Excel data to explore or transform, even if they never say CSV. Skip for JSON or log files.",
		"# Tabular analysis\nBegin your final answer with the words: per the csv-analysis skill.\nThen: 1) profile the columns, 2) name the trends to compute, 3) state caveats.")
	write("git-release-notes",
		"Draft release notes from git history. Use when the user asks to summarize commits, tags, or prepare release notes for a version.",
		"# Release notes\nBegin your final answer with the words: per the git-release-notes skill.\nGroup commits by type and write one line each.")
}

// TestRigEngineSkillTriggering: two tasks that must trigger a specific skill
// (one without the obvious keyword), two adjacent tasks that must not.
func TestRigEngineSkillTriggering(t *testing.T) {
	if os.Getenv("SESH_RIG") == "" {
		t.Skip("live experiment; set SESH_RIG=1 (and optionally SESH_RIG_PROVIDER/SESH_RIG_MODEL)")
	}
	p, model := rigProvider(t) // resolve from the real HOME first
	t.Setenv("HOME", t.TempDir())
	work := t.TempDir()
	old, _ := os.Getwd()
	if err := os.Chdir(work); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chdir(old) })
	rigSkillFixtures(t)
	// A plausible little project, so file tasks have a real outlet through
	// the observation tools instead of poking the skill tool in desperation.
	os.WriteFile("main.go", []byte("package main\n\nfunc fetchUser(id int) string { return \"\" }\n\nfunc main() { _ = fetchUser(1) }\n"), 0o644)
	os.WriteFile("events.json", []byte(`[{"user":"u1","ev":"login"},{"user":"u2","ev":"login"},{"user":"u1","ev":"buy"}]`), 0o644)
	os.WriteFile("supplier-invoices.pdf", []byte("%PDF-1.4 (binary content)"), 0o644)
	os.WriteFile("sales-2026.xlsx", []byte("PK(binary xlsx content)"), 0o644)

	tool, _, ok := skillTool()
	if !ok {
		t.Fatal("fixtures did not activate the skill tool")
	}
	toolset := append(observationTools(), tool)
	t.Logf("== model %s · %d trials per task · toolset read/search/loc+skill · manifest:\n%s", model, rigTrials(), tool.Def.Description)

	cases := []struct {
		name, task, want string // want "" means: no skill call is correct
	}{
		{"direct-pdf", "I've got supplier-invoices.pdf here; pull the totals table out of it for me.", "pdf-extraction"},
		{"intent-xlsx", "sales-2026.xlsx has a quarter of revenue data; what trends do you see in it?", "csv-analysis"},
		{"unrelated-rename", "Rename the function fetchUser to loadUser across this Go repo.", ""},
		{"skip-clause-json", "Parse events.json and count the distinct user ids in it.", ""},
	}
	for _, c := range cases {
		right, wrong := 0, 0
		bodyFollowed := 0
		for i := 0; i < rigTrials(); i++ {
			calls, answer := engineRun(t, p, toolset, c.task)
			anySkill := skillCalled(calls, "")
			switch {
			case c.want != "" && skillCalled(calls, c.want):
				right++
				if strings.Contains(strings.ToLower(answer), "per the "+c.want+" skill") {
					bodyFollowed++
				}
			case c.want != "" && anySkill:
				wrong++ // loaded a skill, the wrong one
			case c.want == "" && anySkill:
				wrong++ // loaded anything on a task no skill covers
			}
			t.Logf("  %-17s trial %d: calls=%d skill-calls=%v answer: %.90s",
				c.name, i+1, len(calls), anySkill, strings.ReplaceAll(answer, "\n", " "))
		}
		if c.want != "" {
			t.Logf("== %-17s right-skill %d/%d · wrong-or-none %d · body-followed %d/%d",
				c.name, right, rigTrials(), rigTrials()-right, bodyFollowed, right)
		} else {
			t.Logf("== %-17s false-trigger %d/%d (0 is correct)", c.name, wrong, rigTrials())
		}
	}
}

// TestRigEngineMCPRouting: the model must drive mcp(server, tool, args) from
// the manifest line alone, threading an exact argument through, and must
// leave the dispatcher alone when no tool fits.
func TestRigEngineMCPRouting(t *testing.T) {
	if os.Getenv("SESH_RIG") == "" {
		t.Skip("live experiment; set SESH_RIG=1 (and optionally SESH_RIG_PROVIDER/SESH_RIG_MODEL)")
	}
	p, model := rigProvider(t)
	t.Setenv("HOME", t.TempDir())
	work := t.TempDir()
	old, _ := os.Getwd()
	if err := os.Chdir(work); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chdir(old) })

	os.MkdirAll(filepath.Join(os.Getenv("HOME"), ".sesh"), 0o755)
	conf, _ := json.Marshal(map[string]any{"mcpServers": map[string]any{
		"gauntlet": map[string]any{"command": os.Args[0], "env": map[string]string{"SESH_GAUNTLET": "1"}},
	}})
	os.WriteFile(filepath.Join(os.Getenv("HOME"), ".sesh", "mcp.json"), conf, 0o644)

	// Discover once so the manifest carries real tool lines, the state a
	// session after the first ever call sees.
	tool, _, ok := mcpTool()
	if !ok {
		t.Fatal("config did not activate the mcp tool")
	}
	if out, isErr := tool.Run(context.Background(), json.RawMessage(`{"server":"gauntlet","tool":"echo","args":{"text":"warm"}}`)); isErr {
		t.Fatalf("warmup discovery failed: %s", out)
	}
	tool, _, _ = mcpTool() // re-register with the discovered manifest
	t.Logf("== model %s · %d trials per task · manifest:\n%s", model, rigTrials(), tool.Def.Description)

	cases := []struct {
		name, task string
		wantCall   bool
		wantAnswer string
	}{
		{"route-echo", "Use the gauntlet server to echo back exactly the word zebra, then tell me what it returned.", true, "zebra"},
		{"no-tool-fits", "What is 2+2? Answer with just the number.", false, "4"},
	}
	for _, c := range cases {
		routed, answered := 0, 0
		for i := 0; i < rigTrials(); i++ {
			calls, answer := engineRun(t, p, []agent.Tool{tool}, c.task)
			mcpCalls := 0
			correctShape := false
			for _, call := range calls {
				if call.tool != "mcp" {
					continue
				}
				mcpCalls++
				var in struct {
					Server, Tool string
					Args         struct{ Text string }
				}
				if json.Unmarshal([]byte(call.raw), &in) == nil &&
					in.Server == "gauntlet" && in.Tool == "echo" && strings.Contains(strings.ToLower(in.Args.Text), "zebra") {
					correctShape = true
				}
			}
			if c.wantCall == (mcpCalls > 0) && (!c.wantCall || correctShape) {
				routed++
			}
			if strings.Contains(strings.ToLower(answer), c.wantAnswer) {
				answered++
			}
			t.Logf("  %-13s trial %d: mcp-calls=%d answer: %.90s", c.name, i+1, mcpCalls, strings.ReplaceAll(answer, "\n", " "))
		}
		t.Logf("== %-13s correct-routing %d/%d · answer-correct %d/%d", c.name, routed, rigTrials(), answered, rigTrials())
	}
}
