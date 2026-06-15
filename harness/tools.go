package harness

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/mike-diff/sesh/agent"
)

// ---------------------------------------------------------------------------
// Tools: pure actions supplied to the core as values. The schema is the
// contract; read/search observe, write/edit/bash mutate.
// ---------------------------------------------------------------------------

var mutating = map[string]bool{"write": true, "edit": true, "bash": true}

func builtinTools(unsafePaths bool, pm *procManager) []agent.Tool {
	obj := func(required []string, props map[string]any) map[string]any {
		return map[string]any{"type": "object", "properties": props, "required": required}
	}
	str := func(desc string) map[string]any {
		return map[string]any{"type": "string", "description": desc}
	}
	def := func(name, desc string, schema map[string]any, run func(ctx context.Context, in toolInput) (string, bool)) agent.Tool {
		return agent.Tool{
			Def: agent.ToolDef{Name: name, Description: desc, Schema: schema},
			Run: func(ctx context.Context, raw json.RawMessage) (string, bool) {
				var in toolInput
				if err := json.Unmarshal(raw, &in); err != nil {
					return "invalid tool input: " + err.Error(), true
				}
				return run(ctx, in)
			},
			// Observation is parallel-safe; mutation never is. The core runs a
			// batch concurrently only when every call in it qualifies.
			Parallel: !mutating[name],
		}
	}
	tools := []agent.Tool{
		def("read", "Read a file and return its full text.",
			obj([]string{"path"}, map[string]any{"path": str("Path to the file, relative to the working directory.")}),
			func(_ context.Context, in toolInput) (string, bool) { return doRead(in.Path, unsafePaths) }),
		def("search", "Search file contents for a substring, grouped by file. Smart-case: all-lowercase patterns match case-insensitively, any uppercase makes it exact. Respects .gitignore. Very broad queries return per-file match counts instead of lines: narrow the pattern and search again.",
			obj([]string{"pattern"}, map[string]any{"pattern": str("The text to search for (substring; smart-case).")}),
			func(_ context.Context, in toolInput) (string, bool) { return doSearch(in.Pattern) }),
		def("loc", "Count lines of source code, grouped by file extension. Optional dir narrows the count to a subdirectory.",
			obj([]string{}, map[string]any{"dir": str("Directory to count, relative to the working directory (default: the whole project).")}),
			func(_ context.Context, in toolInput) (string, bool) { return doLoc(in.Dir, unsafePaths) }),
		def("write", "Create a file or overwrite it entirely with the given content.",
			obj([]string{"path", "content"}, map[string]any{
				"path": str("Path to the file to create or overwrite."), "content": str("The full content to write."),
			}),
			func(_ context.Context, in toolInput) (string, bool) { return doWrite(in.Path, in.Content, unsafePaths) }),
		hardenedEditTool(unsafePaths),
		def("bash", bashDesc(pm),
			obj([]string{"command"}, map[string]any{"command": str("The command to run.")}),
			func(ctx context.Context, in toolInput) (string, bool) {
				if pm != nil {
					return pm.doBash(ctx, in.Command)
				}
				return boundedBash(ctx, in.Command)
			}),
	}
	// proc is a built-in (it claims its name ahead of tool mods), but only when
	// a supervisor is in play: the top-level session, not subagents or the rig.
	// It is not Parallel: start/stop mutate; the gate treats those two actions
	// as mutating via mutates(), so read-only list/logs never prompt.
	if pm != nil {
		tools = append(tools, agent.Tool{
			Def:      agent.ToolDef{Name: "proc", Description: procToolDesc, Schema: procSchema()},
			Run:      func(_ context.Context, raw json.RawMessage) (string, bool) { return pm.runTool(raw) },
			Parallel: false,
		})
	}
	return tools
}

// bashDesc tailors the bash tool's description to whether background promotion
// is available (it is not in subagents or the rig).
func bashDesc(pm *procManager) string {
	base := "Run a shell command in the working directory and return its combined output. Use for builds, tests, git, and anything the other tools cannot do."
	if pm != nil {
		base += " A command that does not return on its own (a server) is auto-promoted to a background process; prefer proc(action:\"start\") for those."
	}
	return base
}

type toolInput struct {
	Path, Content, Old, New, Pattern, Command, Dir string
}

// sourceExts is what loc counts, mirroring the tool mod it graduated from
// (loc began as a tool mod before becoming a built-in).
var sourceExts = map[string]bool{
	".go": true, ".py": true, ".ts": true, ".js": true,
	".sh": true, ".rs": true, ".c": true, ".h": true,
}

// doLoc counts source lines under dir, grouped by extension. Read-only and
// confined like read; .git and node_modules are skipped.
func doLoc(dir string, unsafe bool) (string, bool) {
	if dir == "" {
		dir = "."
	}
	if msg := confineRead(dir, unsafe); msg != "" {
		return msg, true
	}
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		return "no such directory: " + dir, true
	}
	total := 0
	byExt := map[string]int{} // extension -> file count
	filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if n := d.Name(); n == ".git" || n == "node_modules" {
				return fs.SkipDir
			}
			return nil
		}
		ext := filepath.Ext(path)
		if !sourceExts[ext] {
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		total += bytes.Count(b, []byte{'\n'})
		byExt[ext]++
		return nil
	})
	if len(byExt) == 0 {
		return "no source files under " + dir, false
	}
	exts := make([]string, 0, len(byExt))
	for e := range byExt {
		exts = append(exts, e)
	}
	sort.Strings(exts)
	var b strings.Builder
	fmt.Fprintf(&b, "total lines: %d\nfiles by extension:\n", total)
	for _, e := range exts {
		fmt.Fprintf(&b, "  %4d %s\n", byExt[e], strings.TrimPrefix(e, "."))
	}
	return strings.TrimRight(b.String(), "\n"), false
}

// ---------------------------------------------------------------------------
// The execution environment: where tool calls actually land.
// ---------------------------------------------------------------------------

// seshDir is ~/.sesh, where the master key and encrypted credentials
// live. The read/search tools refuse it so the agent cannot leak its own
// secrets into a transcript during ordinary work. (bash remains a hole; the
// encryption is what protects the key if it does escape.)
func seshDir() string { return filepath.Join(os.Getenv("HOME"), ".sesh") }

func underSeshDir(path string) bool {
	abs, err := filepath.Abs(path)
	if err != nil {
		return false
	}
	hd, err := filepath.Abs(seshDir())
	if err != nil {
		return false
	}
	return abs == hd || strings.HasPrefix(abs, hd+string(os.PathSeparator))
}

// withinWorkdir reports whether path resolves to a location inside the working
// directory. The system prompt asks the model to stay inside the project; this
// is the check that makes that a boundary instead of a request. Symlinks are
// resolved so a link inside the tree cannot point file operations outside it.
func withinWorkdir(path string) bool {
	cwd, err := os.Getwd()
	if err != nil {
		return false
	}
	if cwd, err = filepath.EvalSymlinks(cwd); err != nil {
		return false
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return false
	}
	resolved := resolveExistingPrefix(abs)
	return resolved == cwd || strings.HasPrefix(resolved, cwd+string(os.PathSeparator))
}

// resolveExistingPrefix resolves symlinks on the longest prefix of abs that
// exists, then reattaches the remainder, so paths about to be created still
// resolve correctly.
func resolveExistingPrefix(abs string) string {
	prefix, rest := abs, ""
	for {
		if r, err := filepath.EvalSymlinks(prefix); err == nil {
			return filepath.Join(r, rest)
		}
		parent := filepath.Dir(prefix)
		if parent == prefix { // hit the root without finding an existing path
			return abs
		}
		rest = filepath.Join(filepath.Base(prefix), rest)
		prefix = parent
	}
}

// confine is the shared path policy for the file tools: always refuse the
// harness's own secrets, and refuse paths outside the working directory unless
// -unsafe-paths opted out. Returns a model-readable refusal, or "".
func confine(path string, unsafe bool) string {
	if underSeshDir(path) {
		return "refusing to touch the harness key/credentials directory"
	}
	if !unsafe && !withinWorkdir(path) {
		return fmt.Sprintf("path %s is outside the working directory; stay within the project (the user can rerun with -unsafe-paths to allow this)", path)
	}
	return ""
}

// readableSeshData carves the archive out of the ~/.sesh refusal:
// sessions and chain ledgers are exactly the files context gets offloaded to,
// so the agent (and its subagents) must always be able to find them again.
// The key and credentials stay refused; mutation stays refused everywhere
// under ~/.sesh (write/edit/bash use confine, not confineRead).
func readableSeshData(path string) bool {
	abs, err := filepath.Abs(path)
	if err != nil {
		return false
	}
	for _, dir := range []string{sessionsDir(), chainsDir()} {
		d, err := filepath.Abs(dir)
		if err != nil {
			continue
		}
		if abs == d || strings.HasPrefix(abs, d+string(os.PathSeparator)) {
			return true
		}
	}
	return false
}

// confineRead is the observation-side path policy: confine, with the archive
// readable.
func confineRead(path string, unsafe bool) string {
	if readableSeshData(path) {
		return ""
	}
	return confine(path, unsafe)
}

func doRead(path string, unsafe bool) (string, bool) {
	if msg := confineRead(path, unsafe); msg != "" {
		return msg, true
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return err.Error(), true
	}
	return string(b), false
}

func doWrite(path, content string, unsafe bool) (string, bool) {
	if msg := confine(path, unsafe); msg != "" {
		return msg, true
	}
	// Overwriting is the risky case: capture what is being replaced so the
	// result can show the change, not just the byte count.
	prev, hadPrev := "", false
	if b, err := os.ReadFile(path); err == nil {
		prev, hadPrev = string(b), true
	}
	os.MkdirAll(filepath.Dir(path), 0o755)
	// Write-then-rename, same as the harness's own state files: a crash
	// mid-write must not leave the user's file truncated.
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(content), 0o644); err != nil {
		return err.Error(), true
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err.Error(), true
	}
	res := fmt.Sprintf("wrote %s (%d bytes)", path, len(content))
	if hadPrev {
		if d := diffBlock(prev, content, tune.DiffLines); d != "" {
			res += "\n" + d
		}
	}
	return res, false
}

// maxBashOutput bounds what one command can buffer in memory. The core later
// truncates further for the model; this cap is what protects the process from
// a command that prints gigabytes. It is also the proc ring's size.
const maxBashOutput = 1 << 20

// cappedBuffer keeps at most max bytes and counts what it had to drop. It
// never errors, so the command runs to completion (or timeout) regardless.
type cappedBuffer struct {
	buf     []byte
	max     int
	dropped int
}

func (c *cappedBuffer) Write(p []byte) (int, error) {
	if room := c.max - len(c.buf); room > 0 {
		if len(p) <= room {
			c.buf = append(c.buf, p...)
			return len(p), nil
		}
		c.buf = append(c.buf, p[:room]...)
		c.dropped += len(p) - room
		return len(p), nil
	}
	c.dropped += len(p)
	return len(p), nil
}

// boundedBash is the simple, kill-at-timeout shell used where no process
// supervisor is in play: subagents, doctor, and the bench rig. The top-level
// session's bash goes through procManager.doBash instead, which promotes a
// long-lived command to a tracked background process rather than killing it.
func boundedBash(ctx context.Context, command string) (string, bool) {
	ctx, cancel := context.WithTimeout(ctx, bashTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "bash", "-c", command)
	out := &cappedBuffer{max: maxBashOutput}
	cmd.Stdout, cmd.Stderr = out, out
	err := cmd.Run()
	s := string(out.buf)
	if out.dropped > 0 {
		s += fmt.Sprintf("\n... [output capped: %d more bytes dropped]", out.dropped)
	}
	if err != nil {
		return strings.TrimSpace(s + "\n" + err.Error()), true
	}
	if len(s) == 0 {
		return "(no output)", false
	}
	return s, false
}
