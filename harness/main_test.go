package harness

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"sesh/agent"
)

// yes approves one mutating-tool gate prompt.
func yes() *bufio.Reader { return bufio.NewReader(strings.NewReader("y\n")) }

// chtmp runs the test inside a fresh temp dir (t.Chdir needs Go 1.24+).
func chtmp(t *testing.T) {
	t.Helper()
	old, _ := os.Getwd()
	if err := os.Chdir(t.TempDir()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chdir(old) })
}

func args(t *testing.T, v any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// runTool replicates what the core does for one call: apply the gate, then run
// the tool. It exercises the product's two policies (tools-as-values and the
// gate) without standing up a provider.
func runTool(t *testing.T, name string, raw json.RawMessage, stdin *bufio.Reader, ask bool) (string, bool) {
	t.Helper()
	tool := toolByName(t, name, false)
	if err := gate(&plainConsole{in: stdin}, ask)(agent.ToolCall{Name: name, Args: raw}); err != nil {
		return err.Error(), true
	}
	return tool.Run(context.Background(), raw)
}

func toolByName(t *testing.T, name string, unsafePaths bool) agent.Tool {
	t.Helper()
	for _, tl := range builtinTools(unsafePaths) {
		if tl.Def.Name == name {
			return tl
		}
	}
	t.Fatalf("no such tool: %s", name)
	return agent.Tool{}
}

func TestToolsRoundTrip(t *testing.T) {
	t.Setenv("HOME", t.TempDir()) // a real ~/.sesh/gate must not leak in
	chtmp(t)

	// write then read: the default gate never touches the console (nil stdin
	// proves it; restoring prompt-by-default would read and fail here)
	out, isErr := runTool(t, "write", args(t, map[string]string{"path": "a/b.txt", "content": "alpha beta"}), nil, false)
	if isErr {
		t.Fatalf("write failed: %s", out)
	}
	out, isErr = runTool(t, "read", args(t, map[string]string{"path": "a/b.txt"}), nil, false)
	if isErr || out != "alpha beta" {
		t.Fatalf("read got %q (err=%v)", out, isErr)
	}

	// edit: unique replacement succeeds
	out, isErr = runTool(t, "edit", args(t, map[string]string{"path": "a/b.txt", "old": "beta", "new": "gamma"}), nil, false)
	if isErr {
		t.Fatalf("edit failed: %s", out)
	}
	b, _ := os.ReadFile("a/b.txt")
	if string(b) != "alpha gamma" {
		t.Fatalf("file after edit: %q", b)
	}

	// edit: missing and ambiguous text return recoverable errors
	if out, isErr = runTool(t, "edit", args(t, map[string]string{"path": "a/b.txt", "old": "zzz", "new": "x"}), nil, false); !isErr || !strings.Contains(out, "not found") {
		t.Fatalf("missing-text edit: %q err=%v", out, isErr)
	}
	os.WriteFile("a/b.txt", []byte("dup dup"), 0o644)
	if out, isErr = runTool(t, "edit", args(t, map[string]string{"path": "a/b.txt", "old": "dup", "new": "x"}), nil, false); !isErr || !strings.Contains(out, "matches 2 places") {
		t.Fatalf("ambiguous edit: %q err=%v", out, isErr)
	}

	// bash runs and reports output
	if out, isErr = runTool(t, "bash", args(t, map[string]string{"command": "echo hi"}), nil, false); isErr || strings.TrimSpace(out) != "hi" {
		t.Fatalf("bash: %q err=%v", out, isErr)
	}

	// -ask prompts: declined returns a model-readable error, approved runs
	deny := bufio.NewReader(strings.NewReader("n\n"))
	if out, isErr = runTool(t, "bash", args(t, map[string]string{"command": "true"}), deny, true); !isErr || !strings.Contains(out, "declined") {
		t.Fatalf("declined -ask gate: %q err=%v", out, isErr)
	}
	if out, isErr = runTool(t, "bash", args(t, map[string]string{"command": "echo asked"}), yes(), true); isErr || strings.TrimSpace(out) != "asked" {
		t.Fatalf("approved -ask gate: %q err=%v", out, isErr)
	}
}

// TestLoc: the graduated loc built-in counts source lines, prunes .git and
// node_modules, and is confined like read. Breakers: drop the SkipDir prune
// and the total counts the .git file; drop the confineRead call and /etc
// counts instead of refusing.
func TestLoc(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	chtmp(t)
	os.MkdirAll("sub/.git", 0o755)
	os.WriteFile("a.go", []byte("l1\nl2\nl3\n"), 0o644)
	os.WriteFile("sub/b.sh", []byte("x\n"), 0o644)
	os.WriteFile("sub/.git/c.go", []byte("skip\nskip\n"), 0o644)
	os.WriteFile("notes.txt", []byte("not code\n"), 0o644)

	out, isErr := runTool(t, "loc", args(t, map[string]string{}), nil, false)
	if isErr || !strings.Contains(out, "total lines: 4") {
		t.Fatalf("loc: %q err=%v", out, isErr)
	}
	out, isErr = runTool(t, "loc", args(t, map[string]string{"dir": "sub"}), nil, false)
	if isErr || !strings.Contains(out, "total lines: 1") {
		t.Fatalf("loc sub: %q err=%v", out, isErr)
	}
	if out, isErr = runTool(t, "loc", args(t, map[string]string{"dir": "/etc"}), nil, false); !isErr {
		t.Fatalf("loc must be confined like read, got %q", out)
	}
}

// TestGateMod: the executable gate mod rules on mutating calls in code: exit
// nonzero denies with the first stdout line as the model-readable reason,
// exit 0 allows, and a mod that cannot run fails CLOSED. Breakers: skip the
// mod in gate() and the deny case passes a locked-down bash; fail open on
// exec error and the broken-mod case mutates anyway.
func TestGateMod(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	chtmp(t)
	os.MkdirAll(".sesh", 0o755)
	mod := []byte("#!/bin/sh\nif grep -q '\"tool\":\"bash\"' -; then echo \"bash is locked down here\"; exit 1; fi\nexit 0\n")
	if err := os.WriteFile(".sesh/gate", mod, 0o755); err != nil {
		t.Fatal(err)
	}

	// denied tool: refused with the mod's reason, before any prompt logic
	out, isErr := runTool(t, "bash", args(t, map[string]string{"command": "echo never"}), nil, false)
	if !isErr || !strings.Contains(out, "bash is locked down here") {
		t.Fatalf("gate mod deny: %q err=%v", out, isErr)
	}

	// allowed tool: passes through without a prompt
	if out, isErr = runTool(t, "write", args(t, map[string]string{"path": "ok.txt", "content": "x"}), nil, false); isErr {
		t.Fatalf("gate mod allow: %q", out)
	}

	// observation is never the mod's business
	if out, isErr = runTool(t, "read", args(t, map[string]string{"path": "ok.txt"}), nil, false); isErr {
		t.Fatalf("read must bypass the gate mod: %q", out)
	}

	// a broken mod denies rather than silently unlocking the boundary
	os.WriteFile(".sesh/gate", []byte("#!/no/such/interpreter\n"), 0o755)
	if out, isErr = runTool(t, "bash", args(t, map[string]string{"command": "true"}), nil, false); !isErr || !strings.Contains(out, "denied bash") {
		t.Fatalf("broken gate mod must fail closed: %q err=%v", out, isErr)
	}
}

// TestPathConfinement: the file tools refuse paths outside the working
// directory (including via symlinks), -unsafe-paths opts out, and the
// harness's own secrets directory is refused even then.
func TestPathConfinement(t *testing.T) {
	t.Setenv("HOME", t.TempDir()) // a real ~/.sesh/gate must not leak in
	chtmp(t)
	outside := t.TempDir() // a real directory that is NOT under the cwd

	// relative escape is refused
	out, isErr := runTool(t, "write", args(t, map[string]string{"path": "../escape.txt", "content": "x"}), nil, false)
	if !isErr || !strings.Contains(out, "outside the working directory") {
		t.Fatalf("relative escape: %q err=%v", out, isErr)
	}

	// absolute escape is refused, on reads too
	out, isErr = runTool(t, "read", args(t, map[string]string{"path": "/etc/hostname"}), nil, false)
	if !isErr || !strings.Contains(out, "outside the working directory") {
		t.Fatalf("absolute escape: %q err=%v", out, isErr)
	}

	// a symlink inside the tree pointing outside is refused
	if err := os.Symlink(outside, "link"); err != nil {
		t.Fatal(err)
	}
	out, isErr = runTool(t, "write", args(t, map[string]string{"path": "link/f.txt", "content": "x"}), nil, false)
	if !isErr || !strings.Contains(out, "outside the working directory") {
		t.Fatalf("symlink escape: %q err=%v", out, isErr)
	}

	// inside the tree still works
	if out, isErr = runTool(t, "write", args(t, map[string]string{"path": "ok.txt", "content": "fine"}), nil, false); isErr {
		t.Fatalf("in-tree write refused: %s", out)
	}

	// -unsafe-paths allows leaving the tree
	wr := toolByName(t, "write", true)
	if out, isErr := wr.Run(context.Background(), args(t, map[string]string{"path": filepath.Join(outside, "ok.txt"), "content": "y"})); isErr {
		t.Fatalf("unsafe-paths write refused: %s", out)
	}
	if b, _ := os.ReadFile(filepath.Join(outside, "ok.txt")); string(b) != "y" {
		t.Fatal("unsafe-paths write did not land")
	}

	// ~/.sesh is refused even with -unsafe-paths
	t.Setenv("HOME", outside)
	if out, isErr := wr.Run(context.Background(), args(t, map[string]string{"path": filepath.Join(outside, ".sesh", "key"), "content": "evil"})); !isErr || !strings.Contains(out, "refusing") {
		t.Fatalf("sesh dir not protected: %q err=%v", out, isErr)
	}
}

// TestPrintGate: print mode is read-only unless -yes was given, and the gate
// mod still rules when -yes does allow mutation. Breaker: drop the
// runGateMod call from printGate and an unattended -yes run mutates straight
// past an installed boundary.
func TestPrintGate(t *testing.T) {
	t.Setenv("HOME", t.TempDir()) // no gate mod in scope yet
	chtmp(t)

	deny := printGate(false)
	if err := deny(agent.ToolCall{Name: "bash"}); err == nil || !strings.Contains(err.Error(), "read-only") {
		t.Fatalf("bash should be refused in print mode: %v", err)
	}
	if err := deny(agent.ToolCall{Name: "read"}); err != nil {
		t.Fatalf("read should be allowed in print mode: %v", err)
	}
	if err := printGate(true)(agent.ToolCall{Name: "bash"}); err != nil {
		t.Fatalf("-yes with no gate mod should unlock mutation: %v", err)
	}

	// With a gate mod installed, -yes still must not bypass it: the boundary
	// holds in unattended runs, which is exactly where it matters most.
	os.MkdirAll(".sesh", 0o755)
	mod := []byte("#!/bin/sh\nif grep -q '\"tool\":\"bash\"' -; then echo \"bash is locked down here\"; exit 1; fi\nexit 0\n")
	if err := os.WriteFile(".sesh/gate", mod, 0o755); err != nil {
		t.Fatal(err)
	}
	gated := printGate(true)
	if err := gated(agent.ToolCall{Name: "bash"}); err == nil || !strings.Contains(err.Error(), "locked down") {
		t.Fatalf("print mode -yes must still consult the gate mod: %v", err)
	}
	if err := gated(agent.ToolCall{Name: "write"}); err != nil {
		t.Fatalf("the gate mod allows write, so print mode must too: %v", err)
	}
	if err := gated(agent.ToolCall{Name: "read"}); err != nil {
		t.Fatalf("observation must never reach the gate mod: %v", err)
	}
}

// TestBashOutputCap: a command that prints more than the cap is bounded in
// memory and the result says how much was dropped.
func TestBashOutputCap(t *testing.T) {
	t.Setenv("HOME", t.TempDir()) // a real ~/.sesh/gate must not leak in
	chtmp(t)
	out, isErr := runTool(t, "bash",
		args(t, map[string]string{"command": "head -c 2000000 /dev/zero | tr '\\0' 'a'"}), nil, false)
	if isErr {
		t.Fatalf("bash failed: %s", out)
	}
	if len(out) > maxBashOutput+128 {
		t.Fatalf("output not capped: %d bytes", len(out))
	}
	if !strings.Contains(out, "output capped") {
		t.Fatal("cap marker missing from result")
	}

	// a failing command still reports its (capped) output plus the error
	out, isErr = runTool(t, "bash", args(t, map[string]string{"command": "echo oops >&2; exit 3"}), nil, false)
	if !isErr || !strings.Contains(out, "oops") || !strings.Contains(out, "exit status 3") {
		t.Fatalf("failure reporting: %q err=%v", out, isErr)
	}
}

func TestCappedBuffer(t *testing.T) {
	c := &cappedBuffer{max: 5}
	for _, chunk := range []string{"ab", "cd", "efgh"} {
		if n, err := c.Write([]byte(chunk)); err != nil || n != len(chunk) {
			t.Fatalf("write must report full consumption: n=%d err=%v", n, err)
		}
	}
	if string(c.buf) != "abcde" || c.dropped != 3 {
		t.Fatalf("buf=%q dropped=%d", c.buf, c.dropped)
	}
}

// TestSearchCapAndSkips: past the cap, search suppresses match lines in favor
// of true counts and narrowing guidance (silent first-N keeps are the design
// the evidence killed), and the 1MB size guard still
// filters oversized files. Breakers: restore the silent 50-line keep and the
// suppression assertion fails; drop the size guard and big.txt joins the
// counts.
func TestSearchCapAndSkips(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	chtmp(t)

	// 60 matching files: more matches than the 50-line display cap
	for i := 0; i < 60; i++ {
		os.WriteFile(string(rune('a'+i%26))+strings.Repeat("x", i/26)+".txt", []byte("needle here"), 0o644)
	}
	// a file over the 1MB guard must be skipped even though it matches
	os.WriteFile("big.txt", append(make([]byte, 1<<20), []byte("needle here")...), 0o644)

	out, isErr := runTool(t, "search", args(t, map[string]string{"pattern": "needle"}), nil, false)
	if isErr {
		t.Fatalf("search failed: %s", out)
	}
	if !strings.Contains(out, "60 matches in 60 files") || !strings.Contains(out, "too many to show") {
		t.Fatalf("over-cap summary missing:\n%s", out)
	}
	if strings.Contains(out, "needle here") {
		t.Fatalf("over-cap output must suppress match lines:\n%s", out)
	}
	if strings.Contains(out, "big.txt") {
		t.Fatal("oversized file should have been skipped")
	}
}
