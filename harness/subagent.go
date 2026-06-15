// The task tool: an in-process subagent. A child run of the same core loop
// with a fresh context window, the same brain, and a reduced toolset. This is
// the context firewall: broad searches, bulk reads, and verbose command output
// happen in the child's window, and only its final report enters the parent's.
//
// The child is a reader, not a writer: write and edit are excluded by
// construction, so parallel-context agents can never make conflicting changes;
// mutations stay in the single-threaded parent. bash is included but passes
// through the same gate the parent uses, so the autonomy dial is uniform.
package harness

import (
	"context"
	"encoding/json"
	"os"
	"strings"

	"github.com/mike-diff/sesh/agent"
)

const taskDescription = "Delegate a self-contained, read-only investigation to a subagent with its own fresh context window. " +
	"The subagent can read, search, and run bash here, but cannot write or edit files. " +
	"It does not see this conversation: the prompt must carry every path, fact, and question it needs. " +
	"Use it for broad searches, reading many files, or digesting verbose command output when only the conclusions matter; " +
	"the detail then never enters your context. Returns the subagent's final report."

// taskPromptTemplate is the child's whole persona.
// The output contract is the load-bearing part: the reader has not seen what
// the child saw, so the report must stand alone.
const taskPromptTemplate = `<role>
You are a subagent of sesh, a minimal coding agent, spawned by the
main agent to carry out one self-contained task. You are working in {{cwd}}.
</role>

<constraints>
- You can read, search, and run bash; you cannot write or edit files.
- Never touch paths outside the working directory.
- Do exactly the task you were given; no follow-ups, no scope expansion.
</constraints>

<workflow>
Investigate with search, read, and bash until you can answer fully. When a tool
returns an error, read it and adjust rather than repeating the same call.
</workflow>

<output>
Your final message is returned verbatim to the main agent, which has NOT seen
the files you explored or the conversation you came from. Make it
self-contained: reference files as path:line, quote key code verbatim, and
state conclusions plainly. Dense report, no preamble.
</output>`

// taskTool builds the subagent tool. The provider is fetched per call so a
// /provider or /model switch applies to later spawns; getSess gives children
// recall over the session chain; gate is the same policy the parent runs
// under; report (optional) receives the child's token usage so the caller can
// keep honest totals.
func taskTool(get func() agent.Provider, getSess func() *Session, depth int, unsafePaths bool, gate func(agent.ToolCall) error, report func(agent.Usage)) agent.Tool {
	return agent.Tool{
		Def: agent.ToolDef{
			Name:        "task",
			Description: taskDescription,
			Schema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"prompt": map[string]any{"type": "string",
						"description": "The complete task: objective, relevant paths and facts, and what the report must contain. The subagent knows nothing else."},
				},
				"required": []string{"prompt"},
			},
		},
		Run: func(ctx context.Context, raw json.RawMessage) (string, bool) {
			var in struct {
				Prompt string `json:"prompt"`
			}
			if err := json.Unmarshal(raw, &in); err != nil || strings.TrimSpace(in.Prompt) == "" {
				return "task needs a non-empty prompt describing the investigation", true
			}
			p := get()
			if p == nil {
				return "no active provider to run the subagent on", true
			}
			indent := strings.Repeat("  ", depth+1)
			hooks := agent.Hooks{
				OnToolStart: func(c agent.ToolCall) {
					emit("%s%s> %s %s%s\n", dim, indent, c.Name, compact(string(c.Args)), reset)
				},
				OnToolEnd: func(_ agent.ToolCall, r agent.ToolResult) {
					emit("%s%s< %s%s\n", dim, indent, compact(firstLine(r.Content)), reset)
				},
				Gate: gate,
			}
			cwd, _ := os.Getwd()
			system := render(steerPrompt("task", taskPromptTemplate), map[string]string{"cwd": cwd}) + projectContext()
			out, spent, err := agent.Run(ctx, p, system,
				[]agent.Turn{{Role: "user", Text: in.Prompt}}, childTools(get, getSess, depth, unsafePaths, gate, report), hooks)
			if report != nil {
				report(spent)
			}
			emit("%s%stask used %d in / %d out tokens%s\n", dim, indent, spent.Input, spent.Output, reset)
			if err != nil {
				return "subagent failed: " + err.Error(), true
			}
			if final := lastText(out); final != "" {
				return final, false
			}
			return "subagent finished without a final report; consider doing the work directly", true
		},
		// Children only observe (their writes are excluded by construction), so
		// several investigations can run at once: the fan-out case.
		Parallel: true,
	}
}

// childTools is the subagent's reduced toolset: observation plus gated bash
// and recall over the session chain, never write or edit. Below the depth cap
// the child can spawn its own tasks.
func childTools(get func() agent.Provider, getSess func() *Session, depth int, unsafePaths bool, gate func(agent.ToolCall) error, report func(agent.Usage)) []agent.Tool {
	var out []agent.Tool
	for _, t := range builtinTools(unsafePaths, nil) {
		switch t.Def.Name {
		case "read", "search", "bash":
			out = append(out, t)
		}
	}
	if getSess != nil {
		out = append(out, recallTool(getSess))
	}
	if depth+1 <= tune.TaskDepth { // nesting ends by absence, not a silently filtered promise
		out = append(out, taskTool(get, getSess, depth+1, unsafePaths, gate, report))
	}
	return out
}
