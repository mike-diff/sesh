package harness

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

// httpMCPServer is a Streamable HTTP MCP server for tests. It speaks the
// parts the engine depends on: a session id minted at initialize and
// required thereafter, a JSON response for initialize, and an SSE response
// for tools/list (so both response paths are exercised). echo round-trips.
type httpMCPServer struct {
	requireSession bool
	sawSession     atomic.Bool // a non-initialize request arrived with the id
}

func (s *httpMCPServer) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST only", http.StatusMethodNotAllowed)
			return
		}
		body, _ := io.ReadAll(r.Body)
		var msg rpcMsg
		json.Unmarshal(body, &msg)

		if msg.Method == "initialize" {
			w.Header().Set("Mcp-Session-Id", "sess-xyz")
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%d,"result":{"protocolVersion":"2025-11-25","capabilities":{},"serverInfo":{"name":"http-fixture","version":"1"}}}`, *msg.ID)
			return
		}
		// Every later request must carry the session id.
		if r.Header.Get("Mcp-Session-Id") != "sess-xyz" {
			http.Error(w, "missing session", http.StatusBadRequest)
			return
		}
		s.sawSession.Store(true)
		if msg.ID == nil {
			w.WriteHeader(http.StatusAccepted) // a notification
			return
		}
		switch msg.Method {
		case "tools/list":
			// Answer over SSE to exercise that parse path.
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			fmt.Fprintf(w, "event: message\ndata: {\"jsonrpc\":\"2.0\",\"id\":%d,\"result\":{\"tools\":[{\"name\":\"echo\",\"description\":\"echo text back\",\"inputSchema\":{\"type\":\"object\"}}]}}\n\n", *msg.ID)
		case "tools/call":
			var full struct {
				Params struct {
					Arguments struct {
						Text string `json:"text"`
					} `json:"arguments"`
				} `json:"params"`
			}
			json.Unmarshal(body, &full)
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%d,"result":{"content":[{"type":"text","text":%q}],"isError":false}}`, *msg.ID, full.Params.Arguments.Text)
		default:
			http.Error(w, "unknown method", http.StatusBadRequest)
		}
	}
}

// httpMCPHome writes a url-transport config pointing at the test server.
func httpMCPHome(t *testing.T, url string, headers map[string]string) {
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
	server := map[string]any{"url": url}
	if headers != nil {
		server["headers"] = headers
	}
	b, _ := json.Marshal(map[string]any{"mcpServers": map[string]any{"remote": server}})
	os.WriteFile(filepath.Join(home, ".sesh", "mcp.json"), b, 0o644)
}

// Breaker: route url servers anywhere but the HTTP transport (e.g. the old
// "url not served yet" rejection), or fail to parse the SSE tools/list.
func TestMCPHTTPTransportRoundTrips(t *testing.T) {
	srv := &httpMCPServer{}
	ts := httptest.NewServer(srv.handler())
	defer ts.Close()
	httpMCPHome(t, ts.URL, nil)

	out, isErr := mcpCallServer(t, "remote", "echo", `{"text":"over-http"}`)
	if isErr || out != "over-http" {
		t.Fatalf("HTTP echo failed: %q err=%v", out, isErr)
	}
	if !srv.sawSession.Load() {
		t.Fatal("the server never saw a post-initialize request carrying the session id")
	}
}

// Breaker: drop the Mcp-Session-Id capture or stop echoing it on later
// requests; the fixture 400s without it and the call fails.
func TestMCPHTTPSessionIdPropagates(t *testing.T) {
	srv := &httpMCPServer{}
	ts := httptest.NewServer(srv.handler())
	defer ts.Close()
	httpMCPHome(t, ts.URL, nil)

	// Registration never dials; a call triggers connect + handshake, where
	// tools/list (SSE) and tools/call (JSON) both ride after initialize and so
	// both must carry the session id, or the fixture 400s them.
	if out, isErr := mcpCallServer(t, "remote", "echo", `{"text":"x"}`); isErr {
		t.Fatalf("call failed, so a later request lost the session id: %q", out)
	}
	tool, _, _ := mcpTool() // re-register: the manifest now reflects discovery
	if !strings.Contains(tool.Def.Description, "remote:echo") {
		t.Fatalf("HTTP tools/list (SSE) was not discovered into the manifest:\n%s", tool.Def.Description)
	}
}

// Breaker: stop expanding ${VAR} in headers, or leak the value into config.
func TestMCPHTTPHeadersExpandEnv(t *testing.T) {
	var gotAuth string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if a := r.Header.Get("Authorization"); a != "" {
			gotAuth = a
		}
		w.Header().Set("Mcp-Session-Id", "sess-xyz")
		w.Header().Set("Content-Type", "application/json")
		var msg rpcMsg
		b, _ := io.ReadAll(r.Body)
		json.Unmarshal(b, &msg)
		if msg.Method == "initialize" {
			fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%d,"result":{}}`, *msg.ID)
			return
		}
		if msg.ID != nil {
			fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%d,"result":{"tools":[]}}`, *msg.ID)
		} else {
			w.WriteHeader(http.StatusAccepted)
		}
	}))
	defer ts.Close()

	t.Setenv("MY_MCP_TOKEN", "secret-bearer")
	httpMCPHome(t, ts.URL, map[string]string{"Authorization": "Bearer ${MY_MCP_TOKEN}"})

	// A call triggers the connection; registration alone never dials.
	mcpCallServer(t, "remote", "echo", `{}`)
	if gotAuth != "Bearer secret-bearer" {
		t.Fatalf("expanded auth header did not reach the server, got %q", gotAuth)
	}
	tool, _, _ := mcpTool()
	if strings.Contains(tool.Def.Description, "secret-bearer") {
		t.Fatal("the token leaked into the manifest description")
	}
	if b, err := os.ReadFile(mcpCachePath()); err == nil && strings.Contains(string(b), "secret-bearer") {
		t.Fatal("the token leaked into the manifest cache")
	}
}

// mcpCallServer drives one call against a named server through the tool.
func mcpCallServer(t *testing.T, server, tool, args string) (string, bool) {
	t.Helper()
	tl, _, ok := mcpTool()
	if !ok {
		t.Fatal("mcp tool did not activate")
	}
	raw := json.RawMessage(fmt.Sprintf(`{"server":%q,"tool":%q,"args":%s}`, server, tool, args))
	return tl.Run(context.Background(), raw)
}
