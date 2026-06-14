// The live retention bench: the end-to-end half of this package. It drives a
// real sesh binary through the scripted session in inputs.txt against the
// ticketsvc fixture, lets the context pressure force real handoffs, then scores
// what survived with the pure helpers in score.go. It is the user-facing layer
// the unit tests cannot reach: AGENTS.md asks that the pieces be proven in
// units first and the whole exercised the way a user runs it, and this is that
// second layer.
//
// Opt-in, exactly like the retention rig (rig_test.go gates on SESH_RIG):
//
//	SESH_BENCH=1 go test ./bench -run TestBenchE2E -v -timeout 30m
//	SESH_BENCH_PROVIDER=<name>   (default: the configured default provider)
//	SESH_BENCH_MODEL=<id>        (default: the provider's model)
//	SESH_BENCH_CTX=<tokens>      (default 6000: small, to force handoffs)
//
// It builds sesh, copies the fixture to a temp dir, sandboxes HOME (copying in
// the real providers.json, credentials.json, and key so the provider resolves
// and its encrypted key decrypts), pipes inputs.txt into the binary, and scores
// the resulting chain. It never runs unless SESH_BENCH is set, so go test ./...
// stays offline by default, like the rig.
package bench

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

// realSeshHome is where the user's own config lives. The bench copies from here
// into a sandbox so it can resolve a provider without touching the real
// sessions or chains.
func realSeshHome() string {
	return filepath.Join(os.Getenv("HOME"), ".sesh")
}

// copyFile copies one file if it exists; a missing source is not an error (a
// provider may use an env-var key with no credentials.json, say).
func copyFile(dst, src string, mode os.FileMode) error {
	b, err := os.ReadFile(src)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	return os.WriteFile(dst, b, mode)
}

// copyTree copies a directory's regular files one level deep: enough for the
// flat fixture. Subdirectories (the fixture's own .git, if any) are skipped.
func copyTree(t *testing.T, dst, src string) {
	t.Helper()
	entries, err := os.ReadDir(src)
	if err != nil {
		t.Fatalf("read fixture %s: %v", src, err)
	}
	if err := os.MkdirAll(dst, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		b, err := os.ReadFile(filepath.Join(src, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dst, e.Name()), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// providersFile is the minimal shape the bench reads and rewrites. It mirrors
// harness's providers.json without importing the product.
type providersFile struct {
	Default   string                    `json:"default,omitempty"`
	Providers map[string]map[string]any `json:"providers"`
}

// sandboxProviders copies the real providers.json into homeDir's .sesh, then
// pins a small context on the chosen provider so the scripted session is
// forced to hand off mid-run. Returns the provider name to pass with
// -provider. The credential and master key are copied alongside so an encrypted
// inline-or-stored key still decrypts under the sandbox HOME.
func sandboxProviders(t *testing.T, homeDir string, ctxTokens int) string {
	t.Helper()
	src := realSeshHome()
	b, err := os.ReadFile(filepath.Join(src, "providers.json"))
	if err != nil {
		t.Skipf("no providers.json at %s; configure a provider before running the bench: %v", src, err)
	}
	var pf providersFile
	if err := json.Unmarshal(b, &pf); err != nil {
		t.Fatalf("parse providers.json: %v", err)
	}

	name := os.Getenv("SESH_BENCH_PROVIDER")
	if name == "" {
		name = pf.Default
	}
	if name == "" {
		// no default and none named: take the sole provider if there is one
		if len(pf.Providers) == 1 {
			for only := range pf.Providers {
				name = only
			}
		}
	}
	prof, ok := pf.Providers[name]
	if !ok {
		t.Skipf("provider %q not found in %s/providers.json; set SESH_BENCH_PROVIDER", name, src)
	}
	// Force handoffs: a small window hands off near its limit.
	prof["context"] = ctxTokens
	if m := os.Getenv("SESH_BENCH_MODEL"); m != "" {
		prof["model"] = m
	}
	pf.Providers[name] = prof
	pf.Default = name

	dstDir := filepath.Join(homeDir, ".sesh")
	if err := os.MkdirAll(dstDir, 0o700); err != nil {
		t.Fatal(err)
	}
	out, err := json.MarshalIndent(pf, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dstDir, "providers.json"), out, 0o600); err != nil {
		t.Fatal(err)
	}
	// The encrypted credential and its master key travel together, or a stored
	// key cannot be decrypted in the sandbox.
	if err := copyFile(filepath.Join(dstDir, "credentials.json"), filepath.Join(src, "credentials.json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := copyFile(filepath.Join(dstDir, "key"), filepath.Join(src, "key"), 0o600); err != nil {
		t.Fatal(err)
	}
	return name
}

// buildSesh compiles the binary under test from this module, so the bench
// always measures the working tree, never a stale install.
func buildSesh(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "sesh")
	cmd := exec.Command("go", "build", "-o", bin, "./cmd/sesh")
	cmd.Dir = repoRoot(t)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build sesh: %v\n%s", err, out)
	}
	return bin
}

// repoRoot returns the module root (the dir holding go.mod) by walking up from
// this test file's directory, so the bench works from any working directory.
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd() // .../bench
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("could not find go.mod above the bench directory")
		}
		dir = parent
	}
}

func TestBenchE2E(t *testing.T) {
	if os.Getenv("SESH_BENCH") == "" {
		t.Skip("live bench; set SESH_BENCH=1 (and optionally SESH_BENCH_PROVIDER/SESH_BENCH_MODEL)")
	}

	ctxTokens := 6000
	if v := os.Getenv("SESH_BENCH_CTX"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			ctxTokens = n
		}
	}

	root := repoRoot(t)
	inputs, err := os.ReadFile(filepath.Join(root, "bench", "inputs.txt"))
	if err != nil {
		t.Fatalf("read inputs.txt: %v", err)
	}

	bin := buildSesh(t)

	// Sandbox HOME so the bench never touches the user's real sessions or
	// chains; resolve the provider config before HOME is swapped underfoot.
	home := t.TempDir()
	provider := sandboxProviders(t, home, ctxTokens)

	// Copy the fixture (source only: no binary, no goal.md, both excluded from
	// the committed fixture already) into a scratch workdir the agent can read.
	work := filepath.Join(t.TempDir(), "ticketsvc")
	copyTree(t, work, filepath.Join(root, "bench", "fixture"))

	// Drive sesh through the scripted session. -yes auto-approves the reads the
	// script asks for; the small context forces handoffs as work accumulates.
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "-yes", "-provider", provider)
	cmd.Dir = work
	cmd.Env = append(os.Environ(), "HOME="+home)
	cmd.Stdin = bytes.NewReader(inputs)
	var outBuf bytes.Buffer
	cmd.Stdout = &outBuf
	cmd.Stderr = &outBuf
	runErr := cmd.Run()
	logText := outBuf.String()
	if runErr != nil {
		// A nonzero exit is worth seeing but not necessarily fatal: the script
		// ends with "exit", and the transcript still scores. Log and continue.
		t.Logf("sesh exited with: %v", runErr)
	}

	log := parseLog(logText)
	t.Logf("== provider %s · %d handoffs in log · final session %s", provider, len(log.Handoffs), log.FinalID)
	if log.FinalID == "" {
		t.Fatalf("no final session id in the transcript; the run did not reach the exit hint. tail:\n%s", tail(logText, 2000))
	}

	sessionsDir := filepath.Join(home, ".sesh", "sessions")
	chain, err := walkChain(sessionsDir, log.FinalID)
	if err != nil {
		t.Fatalf("walk chain from %s: %v", log.FinalID, err)
	}

	ret := scoreRetention(chain, defaultProbes)

	// Handoff economics from the chain ledger sesh persisted. The root names
	// the ledger file; for a single-link chain there is none, and economics is
	// simply zero handoffs.
	root0 := chain[0].ID
	recs, _ := readLedger(filepath.Join(home, ".sesh", "chains"), root0)
	econ := summarizeEconomics(recs)

	t.Log("\n" + renderReport(provider, log, chain, ret, econ))

	// The bench is a measurement, not a pass/fail gate on the model: a low score
	// is a real result, not a test bug. We fail only on a broken run: no chain,
	// or a chain that recorded none of the planted probes at all (which means
	// the scripted session never executed, not that the model forgot).
	if len(chain) == 0 {
		t.Fatal("empty chain")
	}
	asked := 0
	for _, p := range ret.Probes {
		if p.Link > 0 {
			asked++
		}
	}
	if asked == 0 {
		t.Fatalf("no probe was even asked in the transcript; inputs.txt did not drive the session. tail:\n%s", tail(logText, 2000))
	}
}

// tail returns the last n bytes of s, for trimming a long transcript in a
// failure message.
func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return "..." + s[len(s)-n:]
}
