package harness

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"sesh/agent"
)

// writeToolMod drops an executable into the temp HOME's tools dir.
func writeToolMod(t *testing.T, name, body string) {
	t.Helper()
	dir := toolModsDir()
	os.MkdirAll(dir, 0o755)
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
}

const echoTool = `#!/bin/sh
if [ "$1" = "--schema" ]; then
  echo '{"description": "Echo the word field back, uppercased.", "parameters": {"type": "object", "properties": {"word": {"type": "string"}}, "required": ["word"]}}'
  exit 0
fi
sed 's/.*"word":"\([^"]*\)".*/\1/' | tr a-z A-Z
`

func TestToolModRoundTrip(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	writeToolMod(t, "shout", echoTool)

	tools, notes := loadToolMods(map[string]bool{})
	if len(notes) != 0 || len(tools) != 1 {
		t.Fatalf("load: %d tools, notes %v", len(tools), notes)
	}
	tool := tools[0]
	if tool.Def.Name != "shout" || !strings.Contains(tool.Def.Description, "uppercased") {
		t.Fatalf("definition: %+v", tool.Def)
	}
	if !tool.Parallel || mutating["shout"] {
		t.Fatal("a non-mutating mod is parallel and ungated")
	}
	props := tool.Def.Schema["properties"].(map[string]any)
	if _, ok := props["word"]; !ok {
		t.Fatalf("schema parameters must pass through: %v", tool.Def.Schema)
	}
	out, isErr := tool.Run(context.Background(), json.RawMessage(`{"word":"hello"}`))
	if isErr || out != "HELLO" {
		t.Fatalf("run: %q err=%v", out, isErr)
	}
}

func TestToolModMutatingJoinsGate(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Cleanup(func() { delete(mutating, "stamp") })
	writeToolMod(t, "stamp", `#!/bin/sh
if [ "$1" = "--schema" ]; then echo '{"description": "Writes a stamp file.", "mutating": true}'; exit 0; fi
echo stamped
`)
	tools, _ := loadToolMods(map[string]bool{})
	if len(tools) != 1 {
		t.Fatalf("load: %d tools", len(tools))
	}
	if !mutating["stamp"] {
		t.Fatal("a mutating mod must join the gate policy")
	}
	if tools[0].Parallel {
		t.Fatal("mutation is never parallel, whatever the schema claims")
	}
	if err := printGate(false)(agent.ToolCall{Name: "stamp"}); err == nil {
		t.Fatal("print mode must refuse a mutating mod without -yes")
	}
}

func TestToolModFailureIsModelReadable(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	writeToolMod(t, "broken-run", `#!/bin/sh
if [ "$1" = "--schema" ]; then echo '{"description": "Always fails."}'; exit 0; fi
echo "diagnostic detail" >&2
exit 7
`)
	tools, _ := loadToolMods(map[string]bool{})
	out, isErr := tools[0].Run(context.Background(), json.RawMessage(`{}`))
	if !isErr || !strings.Contains(out, "diagnostic detail") || !strings.Contains(out, "exit status 7") {
		t.Fatalf("failure must carry stderr and the exit status: %q err=%v", out, isErr)
	}
}

func TestToolModSkips(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	// shadowing a built-in is refused loudly
	writeToolMod(t, "read", echoTool)
	// an unanswerable --schema is refused
	writeToolMod(t, "no-schema", "#!/bin/sh\nexit 1\n")
	// a non-executable file is refused
	dir := toolModsDir()
	os.WriteFile(filepath.Join(dir, "plain.txt"), []byte("not a tool"), 0o644)
	// documentation is silently ignored, not warned about: the scaffold puts
	// a README here. Breaker: drop the .md skip and this note appears.
	os.WriteFile(filepath.Join(dir, "README.md"), []byte("docs"), 0o644)

	tools, notes := loadToolMods(map[string]bool{"read": true})
	if len(tools) != 0 {
		t.Fatalf("nothing valid should load: %d tools", len(tools))
	}
	all := strings.Join(notes, "\n")
	for _, want := range []string{"shadows a built-in", "--schema failed", "not executable"} {
		if !strings.Contains(all, want) {
			t.Fatalf("skip notes missing %q:\n%s", want, all)
		}
	}
	if strings.Contains(all, "README.md") {
		t.Fatalf("docs must be ignored silently, not warned about:\n%s", all)
	}
}

func TestToolModMissingDirIsQuiet(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	tools, notes := loadToolMods(map[string]bool{})
	if tools != nil || notes != nil {
		t.Fatal("no tools dir means no tools and no noise")
	}
}
