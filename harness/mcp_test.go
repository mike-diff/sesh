package harness

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// The gauntlet: a fixture MCP server whose every tool attacks one engine
// guarantee. It runs as this test binary re-exec'd with SESH_GAUNTLET=1
// (helper-process pattern), so the tests need no external runtime.
// ---------------------------------------------------------------------------

func TestMain(m *testing.M) {
	if os.Getenv("SESH_GAUNTLET") == "1" {
		gauntletMain()
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func gauntletMain() {
	if os.Getenv("SESH_GAUNTLET_BANNER") == "1" {
		fmt.Println("gauntlet server v1 starting up...") // the classic stdout sin
	}
	send := func(v any) {
		b, _ := json.Marshal(v)
		fmt.Println(string(b))
	}
	type tool map[string]any
	in := bufio.NewReader(os.Stdin)
	for {
		line, err := in.ReadString('\n')
		if err != nil {
			return // EOF: exit cleanly, per the protocol doc
		}
		var msg struct {
			ID     *int   `json:"id"`
			Method string `json:"method"`
			Params struct {
				Name      string          `json:"name"`
				Arguments json.RawMessage `json:"arguments"`
			} `json:"params"`
		}
		if json.Unmarshal([]byte(line), &msg) != nil || msg.ID == nil && msg.Method != "" && strings.HasPrefix(msg.Method, "notifications/") {
			continue
		}
		switch msg.Method {
		case "initialize":
			send(map[string]any{"jsonrpc": "2.0", "id": *msg.ID, "result": map[string]any{
				"protocolVersion": "2025-11-25",
				"capabilities":    map[string]any{"tools": map[string]any{}},
				"serverInfo":      map[string]any{"name": "gauntlet", "version": "1"},
			}})
		case "tools/list":
			send(map[string]any{"jsonrpc": "2.0", "id": *msg.ID, "result": map[string]any{"tools": []tool{
				{"name": "echo", "description": "echo text back", "inputSchema": map[string]any{"type": "object"}, "annotations": map[string]any{"readOnlyHint": true}},
				{"name": "fails", "description": "always fails", "inputSchema": map[string]any{"type": "object"}},
				{"name": "env_echo", "description": "report GAUNTLET_SECRET", "inputSchema": map[string]any{"type": "object"}},
				{"name": "big", "description": "2MB of output", "inputSchema": map[string]any{"type": "object"}},
				{"name": "dies", "description": "exit mid-call", "inputSchema": map[string]any{"type": "object"}},
			}}})
		case "tools/call":
			text := func(s string, isErr bool) {
				send(map[string]any{"jsonrpc": "2.0", "id": *msg.ID, "result": map[string]any{
					"content": []map[string]any{{"type": "text", "text": s}}, "isError": isErr,
				}})
			}
			switch msg.Params.Name {
			case "echo":
				var a struct {
					Text string `json:"text"`
				}
				json.Unmarshal(msg.Params.Arguments, &a)
				text(a.Text, false)
			case "fails":
				text("the widget is sideways; pass upright=true", true)
			case "env_echo":
				text(os.Getenv("GAUNTLET_SECRET"), false)
			case "big":
				text(strings.Repeat("x", 2<<20), false)
			case "dies":
				os.Exit(3)
			}
		}
	}
}

// ---------------------------------------------------------------------------
// Helpers.
// ---------------------------------------------------------------------------

// mcpHome isolates HOME and the working directory, writes the global config,
// and returns the temp project dir for overlay tests.
func mcpHome(t *testing.T, servers map[string]mcpServerConf) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	work := t.TempDir()
	old, _ := os.Getwd()
	if err := os.Chdir(work); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chdir(old) })
	os.MkdirAll(filepath.Join(home, ".sesh"), 0o755)
	b, _ := json.Marshal(map[string]any{"mcpServers": servers})
	os.WriteFile(filepath.Join(home, ".sesh", "mcp.json"), b, 0o644)
	return work
}

func gauntletConf(env map[string]string) mcpServerConf {
	full := map[string]string{"SESH_GAUNTLET": "1"}
	for k, v := range env {
		full[k] = v
	}
	return mcpServerConf{Command: os.Args[0], Env: full}
}

func mcpCall(t *testing.T, tool string, args string) (string, bool) {
	t.Helper()
	tl, _, ok := mcpTool()
	if !ok {
		t.Fatal("mcp tool did not activate")
	}
	raw := json.RawMessage(fmt.Sprintf(`{"server":"gauntlet","tool":%q,"args":%s}`, tool, args))
	return tl.Run(context.Background(), raw)
}

// ---------------------------------------------------------------------------
// Tests. Each names the one-line breaker that fails it.
// ---------------------------------------------------------------------------

// Breaker: dial servers inside mcpTool to build the manifest.
func TestMCPRegistrationNeverDials(t *testing.T) {
	mcpHome(t, map[string]mcpServerConf{"dead": {Command: "/nonexistent/never-starts"}})
	t0 := time.Now()
	tl, _, ok := mcpTool()
	if !ok {
		t.Fatal("mcp tool did not activate")
	}
	if d := time.Since(t0); d > time.Second {
		t.Fatalf("registration took %v; it must never dial a server", d)
	}
	if !strings.Contains(tl.Def.Description, "not yet discovered") {
		t.Fatalf("undiscovered server must still appear in the manifest:\n%s", tl.Def.Description)
	}
}

// Breaker: treat a non-JSON stdout line as a fatal protocol error.
func TestMCPToleratesStartupBanner(t *testing.T) {
	mcpHome(t, map[string]mcpServerConf{"gauntlet": gauntletConf(map[string]string{"SESH_GAUNTLET_BANNER": "1"})})
	out, isErr := mcpCall(t, "echo", `{"text":"hello"}`)
	if isErr || out != "hello" {
		t.Fatalf("echo through a banner-printing server failed: %q err=%v", out, isErr)
	}
}

// Breaker: return a bare "not found" without naming the server's tools.
func TestMCPUnknownToolNamesValidOnes(t *testing.T) {
	mcpHome(t, map[string]mcpServerConf{"gauntlet": gauntletConf(nil)})
	out, isErr := mcpCall(t, "bogus", `{}`)
	if !isErr || !strings.Contains(out, "echo") || !strings.Contains(out, "fails") {
		t.Fatalf("unknown-tool error must list the server's tools: %q", out)
	}
}

// Breaker: ignore isError and return the result as success.
func TestMCPIsErrorSurfacesAsToolError(t *testing.T) {
	mcpHome(t, map[string]mcpServerConf{"gauntlet": gauntletConf(nil)})
	out, isErr := mcpCall(t, "fails", `{}`)
	if !isErr || !strings.Contains(out, "sideways") {
		t.Fatalf("isError result must surface as an actionable tool error: %q err=%v", out, isErr)
	}
}

// Breaker: expand ${VAR} into the manifest or skip expansion at spawn.
func TestMCPEnvExpansionReachesServerNotManifest(t *testing.T) {
	t.Setenv("TESTSECRET", "hunter2")
	mcpHome(t, map[string]mcpServerConf{"gauntlet": gauntletConf(map[string]string{"GAUNTLET_SECRET": "${TESTSECRET}"})})
	out, isErr := mcpCall(t, "env_echo", `{}`)
	if isErr || out != "hunter2" {
		t.Fatalf("env expansion must reach the server: %q err=%v", out, isErr)
	}
	tl, _, _ := mcpTool()
	if strings.Contains(tl.Def.Description, "hunter2") {
		t.Fatal("the secret leaked into the manifest description")
	}
	if b, err := os.ReadFile(mcpCachePath()); err == nil && strings.Contains(string(b), "hunter2") {
		t.Fatal("the secret leaked into the manifest cache")
	}
}

// Breaker: merge project-defined servers into the active set.
func TestMCPOverlayCannotDefineServers(t *testing.T) {
	work := mcpHome(t, map[string]mcpServerConf{"gauntlet": gauntletConf(nil)})
	os.MkdirAll(filepath.Join(work, ".sesh"), 0o755)
	smuggle := `{"mcpServers": {"evil": {"command": "/bin/evil"}}, "servers": ["gauntlet"]}`
	os.WriteFile(filepath.Join(work, ".sesh", "mcp.json"), []byte(smuggle), 0o644)

	active, notes := loadMCPConfig()
	if _, ok := active["evil"]; ok {
		t.Fatal("a project overlay defined a server and it was accepted")
	}
	joined := strings.Join(notes, "\n")
	if !strings.Contains(joined, "never define") {
		t.Fatalf("the smuggled definition must be a loud validation error, notes: %v", notes)
	}
}

// Breaker: skip unknown overlay names silently.
func TestMCPOverlayUnknownNameIsLoud(t *testing.T) {
	work := mcpHome(t, map[string]mcpServerConf{"gauntlet": gauntletConf(nil)})
	os.MkdirAll(filepath.Join(work, ".sesh"), 0o755)
	os.WriteFile(filepath.Join(work, ".sesh", "mcp.json"), []byte(`{"servers": ["gauntlt"]}`), 0o644)

	_, notes := loadMCPConfig()
	if !strings.Contains(strings.Join(notes, "\n"), "gauntlt") {
		t.Fatalf("a typo'd selection must be named, not silently ignored: %v", notes)
	}
}

// Breaker: let the overlay's absence of a server keep it active, or let a
// selection fail to re-enable a globally disabled one.
func TestMCPOverlaySelectsExactly(t *testing.T) {
	work := mcpHome(t, map[string]mcpServerConf{
		"on":       gauntletConf(nil),
		"off":      {Command: "/bin/true", Disabled: true},
		"unlisted": gauntletConf(nil),
	})
	os.MkdirAll(filepath.Join(work, ".sesh"), 0o755)
	os.WriteFile(filepath.Join(work, ".sesh", "mcp.json"), []byte(`{"servers": ["on", "off"]}`), 0o644)

	active, _ := loadMCPConfig()
	if _, ok := active["unlisted"]; ok {
		t.Fatal("a server unlisted by the overlay stayed active")
	}
	if _, ok := active["off"]; !ok {
		t.Fatal("explicit selection must beat a global disabled default")
	}
	if len(active) != 2 {
		t.Fatalf("want exactly the selected set, got %v", active)
	}
}

// Breaker: ignore the disabled flag in loadMCPConfig.
func TestMCPDisabledServerDeactivates(t *testing.T) {
	mcpHome(t, map[string]mcpServerConf{"gauntlet": {Command: os.Args[0], Disabled: true}})
	if _, _, ok := mcpTool(); ok {
		t.Fatal("a config with only disabled servers must not register the mcp tool")
	}
}

// Breaker: never write the manifest cache after discovery.
func TestMCPManifestCacheFeedsNextSession(t *testing.T) {
	mcpHome(t, map[string]mcpServerConf{"gauntlet": gauntletConf(nil)})
	tl, _, _ := mcpTool()
	if strings.Contains(tl.Def.Description, "gauntlet:echo") {
		t.Fatal("precondition: no cache should exist before the first call")
	}
	if out, isErr := mcpCall(t, "echo", `{"text":"warm"}`); isErr || out != "warm" {
		t.Fatalf("echo failed: %q", out)
	}
	tl2, _, _ := mcpTool() // a fresh registration, as the next session would do
	if !strings.Contains(tl2.Def.Description, "gauntlet:echo") || !strings.Contains(tl2.Def.Description, "echo text back") {
		t.Fatalf("the next session's manifest must list discovered tools:\n%s", tl2.Def.Description)
	}
}

// Breaker: return server output unbounded.
func TestMCPBigOutputTruncatedLoudly(t *testing.T) {
	mcpHome(t, map[string]mcpServerConf{"gauntlet": gauntletConf(nil)})
	out, _ := mcpCall(t, "big", `{}`)
	if len(out) > maxBashOutput+100 {
		t.Fatalf("output not capped: %d bytes", len(out))
	}
	if !strings.Contains(out, "[output truncated") {
		t.Fatal("truncation must be loud, not silent")
	}
}

// Breaker: keep the dead connection in the pool after a transport error.
func TestMCPServerDeathResetsAndRespawns(t *testing.T) {
	mcpHome(t, map[string]mcpServerConf{"gauntlet": gauntletConf(nil)})
	tl, _, ok := mcpTool()
	if !ok {
		t.Fatal("mcp tool did not activate")
	}
	run := func(tool, args string) (string, bool) {
		raw := json.RawMessage(fmt.Sprintf(`{"server":"gauntlet","tool":%q,"args":%s}`, tool, args))
		return tl.Run(context.Background(), raw)
	}
	if out, isErr := run("dies", `{}`); !isErr || !strings.Contains(out, "reconnect") {
		t.Fatalf("mid-call death must error and promise a reconnect: %q", out)
	}
	if out, isErr := run("echo", `{"text":"alive"}`); isErr || out != "alive" {
		t.Fatalf("the respawned server must serve again: %q err=%v", out, isErr)
	}
}

// Breaker: register the mcp tool without marking it mutating.
func TestMCPJoinsTheGate(t *testing.T) {
	mcpHome(t, map[string]mcpServerConf{"gauntlet": gauntletConf(nil)})
	if _, _, ok := mcpTool(); !ok {
		t.Fatal("mcp tool did not activate")
	}
	if !mutating["mcp"] {
		t.Fatal("mcp must join the gate policy like write/edit/bash")
	}
}
