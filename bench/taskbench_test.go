// The task bench: did a real coding task get done, and at what cost. Each
// task in testdata/tasks/<name>/ is a small repository (repo/), the prompt
// the agent gets (TASK.md), and a check the agent never sees (check.sh, plus
// anything in hidden/ it copies in). A run copies the repository, drives a
// freshly built sesh through it headless, then runs the check in the result:
// exit 0 is success. Every run appends one JSON line (outcome, tokens, model
// and tool calls, wall time) so arms of an experiment can be compared.
//
// Opt-in, like the other live benches:
//
//	SESH_TASKBENCH=1 go test ./bench -run TestTaskBench -v -count=1 -timeout 2h
//	SESH_BENCH_PROVIDER=<name> SESH_BENCH_MODEL=<id>   (default: your configured default)
//	SESH_TASKBENCH_RUNS=<n>        runs per task (default 3; differences inside
//	                               overlapping intervals are noise)
//	SESH_TASKBENCH_TASKS=a,b       only these tasks (default: all)
//	SESH_TASKBENCH_ARGS="-max-iters 1"   extra sesh flags: one experiment arm
//	SESH_TASKBENCH_OUT=<file>      JSONL destination (default: a temp file, logged)
package bench

import (
	"bufio"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
)

// taskUsage is print mode's closing usage line, parsed.
type taskUsage struct {
	In         int `json:"in"`
	CacheRead  int `json:"cache_read"`
	CacheWrite int `json:"cache_write"`
	Out        int `json:"out"`
	ModelCalls int `json:"model_calls"`
	ToolCalls  int `json:"tool_calls"`
}

var usageLineRe = regexp.MustCompile(`== usage: in=(\d+) cache_read=(\d+) cache_write=(\d+) out=(\d+) model_calls=(\d+) tool_calls=(\d+)`)

// parseUsage finds the last usage line in a run's stderr.
func parseUsage(stderr string) (taskUsage, bool) {
	all := usageLineRe.FindAllStringSubmatch(stderr, -1)
	if len(all) == 0 {
		return taskUsage{}, false
	}
	m := all[len(all)-1]
	n := func(i int) int { v, _ := strconv.Atoi(m[i]); return v }
	return taskUsage{n(1), n(2), n(3), n(4), n(5), n(6)}, true
}

// wilson is the 95% Wilson score interval for k successes in n runs: honest
// at the small n a live bench can afford, where k/n alone overstates.
func wilson(k, n int) (lo, hi float64) {
	if n == 0 {
		return 0, 1
	}
	const z = 1.96
	p, fn := float64(k)/float64(n), float64(n)
	den := 1 + z*z/fn
	mid := (p + z*z/(2*fn)) / den
	half := z * math.Sqrt(p*(1-p)/fn+z*z/(4*fn*fn)) / den
	return math.Max(0, mid-half), math.Min(1, mid+half)
}

type taskRun struct {
	Task    string    `json:"task"`
	Run     int       `json:"run"`
	Args    string    `json:"args,omitempty"`
	Exit    int       `json:"exit"`  // sesh's drive outcome
	Pass    bool      `json:"pass"`  // the hidden check
	Check   string    `json:"check"` // the check's last output line
	Seconds float64   `json:"seconds"`
	Usage   taskUsage `json:"usage"`
}

// copyDir copies a fixture tree recursively.
func copyDir(t *testing.T, dst, src string) {
	t.Helper()
	err := filepath.Walk(src, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		target := filepath.Join(dst, rel)
		if info.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(target, b, info.Mode().Perm())
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestTaskBench(t *testing.T) {
	if os.Getenv("SESH_TASKBENCH") == "" {
		t.Skip("live bench; set SESH_TASKBENCH=1 (see the file comment for the dials)")
	}
	runs := 3
	if v, err := strconv.Atoi(os.Getenv("SESH_TASKBENCH_RUNS")); err == nil && v > 0 {
		runs = v
	}
	root := filepath.Join(repoRoot(t), "bench", "testdata", "tasks")
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	only := map[string]bool{}
	for _, n := range strings.Split(os.Getenv("SESH_TASKBENCH_TASKS"), ",") {
		if n = strings.TrimSpace(n); n != "" {
			only[n] = true
		}
	}
	extra := strings.Fields(os.Getenv("SESH_TASKBENCH_ARGS"))
	outPath := os.Getenv("SESH_TASKBENCH_OUT")
	if outPath == "" {
		outPath = filepath.Join(os.TempDir(), fmt.Sprintf("sesh-taskbench-%s.jsonl", time.Now().Format("20060102-150405")))
	}
	outFile, err := os.OpenFile(outPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer outFile.Close()
	t.Logf("records: %s", outPath)

	bin := buildSesh(t)
	home := t.TempDir()
	provider := sandboxProviders(t, home, 0)

	results := map[string][]taskRun{}
	for _, e := range entries {
		name := e.Name()
		if !e.IsDir() || (len(only) > 0 && !only[name]) {
			continue
		}
		taskDir := filepath.Join(root, name)
		prompt, err := os.ReadFile(filepath.Join(taskDir, "TASK.md"))
		if err != nil {
			t.Fatal(err)
		}
		for i := 1; i <= runs; i++ {
			work := t.TempDir()
			copyDir(t, work, filepath.Join(taskDir, "repo"))
			args := append([]string{"-p", string(prompt), "-yes", "-provider", provider}, extra...)
			cmd := exec.Command(bin, args...)
			cmd.Dir = work
			cmd.Env = append(os.Environ(), "HOME="+home)
			var stderr strings.Builder
			cmd.Stderr = &stderr
			start := time.Now()
			err := cmd.Run()
			rec := taskRun{Task: name, Run: i, Args: strings.Join(extra, " "), Seconds: time.Since(start).Seconds()}
			if ee, ok := err.(*exec.ExitError); ok {
				rec.Exit = ee.ExitCode()
			} else if err != nil {
				t.Fatalf("%s run %d: %v", name, i, err)
			}
			rec.Usage, _ = parseUsage(stderr.String())

			check := exec.Command("sh", filepath.Join(taskDir, "check.sh"), taskDir)
			check.Dir = work
			out, cerr := check.CombinedOutput()
			rec.Pass = cerr == nil
			if lines := strings.Split(strings.TrimSpace(string(out)), "\n"); len(lines) > 0 {
				rec.Check = lines[len(lines)-1]
			}
			b, _ := json.Marshal(rec)
			fmt.Fprintln(outFile, string(b))
			results[name] = append(results[name], rec)
			t.Logf("%s run %d: pass=%v exit=%d %.0fs in=%d cached=%d out=%d calls=%d tools=%d",
				name, i, rec.Pass, rec.Exit, rec.Seconds, rec.Usage.In, rec.Usage.CacheRead, rec.Usage.Out, rec.Usage.ModelCalls, rec.Usage.ToolCalls)
		}
	}

	names := make([]string, 0, len(results))
	for n := range results {
		names = append(names, n)
	}
	sort.Strings(names)
	var w strings.Builder
	bw := bufio.NewWriter(&w)
	for _, n := range names {
		rs := results[n]
		k := 0
		var tokens []int
		for _, r := range rs {
			if r.Pass {
				k++
				tokens = append(tokens, r.Usage.In+r.Usage.CacheRead+r.Usage.CacheWrite+r.Usage.Out)
			}
		}
		lo, hi := wilson(k, len(rs))
		med := 0
		if len(tokens) > 0 {
			sort.Ints(tokens)
			med = tokens[len(tokens)/2]
		}
		fmt.Fprintf(bw, "%-16s %d/%d pass (95%% CI %.2f-%.2f) median tokens per success %d\n", n, k, len(rs), lo, hi, med)
	}
	bw.Flush()
	t.Logf("summary:\n%s", w.String())
}

// TestParseUsage: the bench reads print mode's last usage line exactly.
// Breakers: take the first match instead of the last (a stray earlier line
// wins), or misorder the fields, and the struct comparison fails.
func TestParseUsage(t *testing.T) {
	stderr := "== usage: in=1 cache_read=1 cache_write=1 out=1 model_calls=1 tool_calls=1\nnoise\n" +
		"== usage: in=120 cache_read=4000 cache_write=300 out=55 model_calls=6 tool_calls=9\n"
	got, ok := parseUsage(stderr)
	if want := (taskUsage{120, 4000, 300, 55, 6, 9}); !ok || got != want {
		t.Fatalf("got %+v ok=%v, want %+v", got, ok, want)
	}
	if _, ok := parseUsage("no usage here"); ok {
		t.Fatal("a run that printed no usage line must report it missing")
	}
}

// TestWilson: the interval brackets the observed rate and is wide at small n.
// Breaker: return k/n for both ends and the width check fails.
func TestWilson(t *testing.T) {
	lo, hi := wilson(3, 3)
	if hi != 1 || lo > 0.5 || lo < 0.4 {
		t.Fatalf("3/3 must not read as certain: [%.3f, %.3f]", lo, hi)
	}
	lo, hi = wilson(50, 100)
	if lo > 0.5 || hi < 0.5 || hi-lo > 0.2 {
		t.Fatalf("50/100: [%.3f, %.3f]", lo, hi)
	}
}
