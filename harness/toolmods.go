// Tool mods: executables that become agent tools, completing the mods
// surface. The contract mirrors the statusline: a program, a tiny stdin/stdout
// protocol, no plugin runtime.
//
//	~/.sesh/tools/<name>          executable; <name> is the tool's name
//	<name> --schema                  prints {"description", "parameters",
//	                                 "mutating"?, "parallel"?} once at startup
//	<name>                           tool call: args JSON on stdin, result on
//	                                 stdout; nonzero exit makes it a tool error
//	                                 (stderr is appended for the model to read)
//
// Global mount ONLY, deliberately: a tool mod is code the model can execute,
// and a project-local one in a repo you just cloned would be someone else's
// code running under your permissions. The same trust rule as bash, enforced
// structurally rather than by warning.
package harness

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"sesh/agent"
)

func toolModsDir() string { return filepath.Join(os.Getenv("HOME"), ".sesh", "tools") }

// toolModSchema is what `<tool> --schema` must print: the same fields a
// built-in tool declares in code.
type toolModSchema struct {
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"` // a JSON Schema object; defaults to any-object
	Mutating    bool            `json:"mutating"`   // true = gated like write/edit/bash
	Parallel    *bool           `json:"parallel"`   // default: parallel iff not mutating
}

// loadToolMods discovers and wraps every valid tool mod. taken holds the
// names already claimed (builtins, task, recall): a mod can never shadow a
// built-in, loudly. Returns the tools plus human-readable notes about
// anything skipped; doctor shows the same findings with detail.
func loadToolMods(taken map[string]bool) ([]agent.Tool, []string) {
	entries, err := os.ReadDir(toolModsDir())
	if err != nil {
		return nil, nil
	}
	var tools []agent.Tool
	var notes []string
	for _, e := range entries {
		name := e.Name()
		if strings.HasSuffix(name, ".md") {
			continue // documentation (the scaffold's README lives here), never a tool
		}
		path := filepath.Join(toolModsDir(), name)
		info, err := e.Info()
		if err != nil || !info.Mode().IsRegular() || info.Mode()&0o111 == 0 {
			notes = append(notes, fmt.Sprintf("tool mod %s: not executable; skipped (chmod +x)", name))
			continue
		}
		if taken[name] {
			notes = append(notes, fmt.Sprintf("tool mod %s: shadows a built-in tool; skipped (rename it)", name))
			continue
		}
		schema, err := toolModSchemaOf(path)
		if err != nil {
			notes = append(notes, fmt.Sprintf("tool mod %s: %v; skipped", name, err))
			continue
		}
		if schema.Mutating {
			mutating[name] = true // joins the gate policy like write/edit/bash
		}
		parallel := !schema.Mutating
		if schema.Parallel != nil {
			parallel = *schema.Parallel && !schema.Mutating
		}
		params := map[string]any{"type": "object"}
		if len(schema.Parameters) > 0 {
			var p map[string]any
			if json.Unmarshal(schema.Parameters, &p) == nil {
				params = p
			}
		}
		tools = append(tools, agent.Tool{
			Def:      agent.ToolDef{Name: name, Description: schema.Description, Schema: params},
			Run:      runToolMod(path),
			Parallel: parallel,
		})
		taken[name] = true
	}
	return tools, notes
}

// toolModSchemaOf asks the executable to describe itself. A tool that cannot
// answer --schema quickly and validly does not load.
func toolModSchemaOf(path string) (toolModSchema, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, path, "--schema").Output()
	if err != nil {
		return toolModSchema{}, fmt.Errorf("--schema failed: %v", err)
	}
	var s toolModSchema
	if err := json.Unmarshal(out, &s); err != nil {
		return toolModSchema{}, fmt.Errorf("--schema is not valid JSON: %v", err)
	}
	if strings.TrimSpace(s.Description) == "" {
		return toolModSchema{}, fmt.Errorf("--schema must include a description")
	}
	return s, nil
}

// runToolMod executes one call: args JSON on stdin, stdout is the result,
// stderr is appended on failure so the model can act on the error. The turn's
// context applies (Ctrl-C kills the tool), bounded by the bash timeout.
func runToolMod(path string) func(ctx context.Context, raw json.RawMessage) (string, bool) {
	return func(ctx context.Context, raw json.RawMessage) (string, bool) {
		ctx, cancel := context.WithTimeout(ctx, bashTimeout)
		defer cancel()
		cmd := exec.CommandContext(ctx, path)
		cmd.Stdin = strings.NewReader(string(raw))
		stdout := &cappedBuffer{max: maxBashOutput}
		stderr := &cappedBuffer{max: 1 << 14}
		cmd.Stdout, cmd.Stderr = stdout, stderr
		err := cmd.Run()
		out := strings.TrimSpace(string(stdout.buf))
		if err != nil {
			msg := out
			if errTail := strings.TrimSpace(string(stderr.buf)); errTail != "" {
				msg += "\n" + errTail
			}
			return strings.TrimSpace(msg + "\n" + err.Error()), true
		}
		if out == "" {
			return "(no output)", false
		}
		return out, false
	}
}
