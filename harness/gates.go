package harness

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/mike-diff/sesh/agent"
)

// ---------------------------------------------------------------------------
// Oversight: the gate is a policy the product injects into the core.
//
// Interactive default: every tool runs without prompting. The human watching
// the transcript is the oversight, and ctrl-c is the gate; pre-approving each
// write would be the harness pre-judging the model, the same mistake as
// iteration caps. The dial still moves deliberately: -ask restores per-call
// prompting, and a gate mod locks the dial down in code.
// ---------------------------------------------------------------------------

// gate is the interactive oversight policy. Mutating calls consult the gate
// mod (when installed), then prompt only under -ask. Returns a model-readable
// error when an action is denied. The mutex serializes prompts: parallel task
// subagents may each want a bash run, and two prompts must not interleave.
func gate(c console, ask bool) func(agent.ToolCall) error {
	mod := findGateMod()
	var mu sync.Mutex
	return func(tc agent.ToolCall) error {
		if !mutates(tc) {
			return nil
		}
		if err := runGateMod(mod, tc); err != nil {
			return err
		}
		if !ask {
			return nil
		}
		mu.Lock()
		defer mu.Unlock()
		key, _ := c.ReadKey(fmt.Sprintf("%s  approve %s? [y/N] %s", yellow, tc.Name, reset))
		if key != 'y' && key != 'Y' {
			return fmt.Errorf("the user declined this action; ask them how to proceed instead")
		}
		return nil
	}
}

// budgetGate wraps a gate with a per-run tool budget for unattended runs: a
// model stuck repeating tool calls otherwise loops forever with nobody at the
// terminal to notice. The budget spans the whole tree (subagents run under
// the same gate). 0 means no budget, the default: the loop stays free, and
// capping it is the user's explicit choice, never the harness's. The refusal
// is model-readable so the model closes with what it has instead of dying.
func budgetGate(n int, inner func(agent.ToolCall) error) func(agent.ToolCall) error {
	if n <= 0 {
		return inner
	}
	var mu sync.Mutex
	used := 0
	return func(c agent.ToolCall) error {
		mu.Lock()
		defer mu.Unlock()
		if used >= n {
			return fmt.Errorf("the tool budget for this run (%d calls) is exhausted; produce your final answer now from what you already have", n)
		}
		used++
		return inner(c)
	}
}

// printGate is the print-mode policy: read-only unless -yes was given. There
// is no one at the terminal watching to interrupt, so mutation must be opted
// into explicitly rather than granted by virtue of being unattended. When -yes
// does allow mutation, the gate mod still rules: a boundary installed in code
// must hold in unattended runs too (it is exactly where it matters most), and
// it fails closed like everywhere else.
func printGate(autoYes bool) func(agent.ToolCall) error {
	mod := findGateMod()
	return func(c agent.ToolCall) error {
		if !mutates(c) {
			return nil
		}
		if !autoYes {
			return fmt.Errorf("print mode is read-only: %s is disabled; tell the user to rerun with -yes to allow changes", c.Name)
		}
		return runGateMod(mod, c)
	}
}

// ---------------------------------------------------------------------------
// The gate mod: a boundary in code, owned by user space. An executable on the
// usual chain rules on every mutating call; absent means allow. This is how a
// user locks the open-by-default dial down without recompiling.
// ---------------------------------------------------------------------------

// gateInfo is the context handed to a gate mod as JSON.
type gateInfo struct {
	Tool string          `json:"tool"`
	Args json.RawMessage `json:"args"`
	Cwd  string          `json:"cwd"`
}

// findGateMod resolves the executable gate mod: project, then global, then
// none. Resolved once per session, like the system prompt.
func findGateMod() string {
	for _, p := range []string{
		".sesh/gate", // project
		filepath.Join(os.Getenv("HOME"), ".sesh", "gate"), // global
	} {
		if fi, err := os.Stat(p); err == nil && fi.Mode()&0o111 != 0 {
			return p
		}
	}
	return ""
}

// runGateMod asks the mod to rule on one mutating call: gateInfo JSON on
// stdin, exit 0 allows, nonzero denies with the first stdout line as the
// model-readable reason. A mod that breaks (cannot start, hangs past the
// timeout) DENIES: someone who installed a boundary gets the locked failure
// mode, never the open one.
func runGateMod(mod string, tc agent.ToolCall) error {
	if mod == "" {
		return nil
	}
	cwd, _ := os.Getwd()
	b, err := json.Marshal(gateInfo{Tool: tc.Name, Args: tc.Args, Cwd: cwd})
	if err != nil {
		return fmt.Errorf("the gate mod could not rule on %s (%v); ask the user how to proceed", tc.Name, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, mod)
	cmd.Stdin = bytes.NewReader(b)
	out, err := cmd.Output()
	if err == nil {
		return nil
	}
	if reason := strings.TrimSpace(firstLine(string(out))); reason != "" {
		return fmt.Errorf("the gate mod denied %s: %s", tc.Name, reason)
	}
	return fmt.Errorf("the gate mod denied %s (%v); ask the user how to proceed", tc.Name, err)
}
