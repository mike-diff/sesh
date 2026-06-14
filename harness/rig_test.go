// The retention rig: measures what survives a context boundary (compaction,
// handoff) by planting facts early in a synthetic session, inflating it with
// realistic filler, applying the boundary, and probing late. This is the
// empirical ground the continuity design stands on: a boundary mechanism is
// kept only if the rig shows it preserves what the published numbers say
// in-place compaction loses (constraints, rationale, failed approaches).
//
// The pure helpers are unit-tested below as usual. The live experiment needs a
// real provider and is opt-in:
//
//	SESH_RIG=1 go test -run TestRigRetention -v -timeout 30m
//	SESH_RIG_PROVIDER=<name> SESH_RIG_MODEL=<id>   (default: your configured default provider)
//	SESH_RIG_BRIEF_PROVIDER=<name> SESH_RIG_BRIEF_MODEL=<id>
//	    (chain condition only: write the handoff briefs with a different,
//	    typically cheaper, brain than the worker; default: the worker itself)
package harness

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/mike-diff/sesh/agent"
)

// fact is one planted piece of session knowledge and the probe that checks it
// survived. Keywords score the probe answer: each is a |-separated alternative
// set, satisfied if any alternative appears (case-insensitive) in the answer.
type fact struct {
	name     string
	plant    string // the user turn that establishes it
	ack      string // synthetic assistant acknowledgment
	probe    string
	keywords []string
}

// plantedFacts cover the categories the research says compaction loses first:
// negative constraints with rationale, decisions with rationale, failed
// approaches, environment facts, and verbatim details.
var plantedFacts = []fact{
	{
		name: "constraint",
		plant: "Hard constraint for this project: never add a Redis dependency. " +
			"The ops team enforces a 64MB memory ceiling on the cache boxes, so all caching must stay in-process.",
		ack:      "Understood: no Redis, in-process caching only.",
		probe:    "What caching technology is forbidden in this project, and why exactly?",
		keywords: []string{"redis", "64", "in-process|in process"},
	},
	{
		name: "decision",
		plant: "Decision: history filenames are hashed with FNV-32a rather than SHA-256, " +
			"because hash/fnv is in the standard library and we do not need cryptographic strength.",
		ack:      "Noted: FNV-32a for history filenames.",
		probe:    "Which hash function do we use for history filenames, and why was it chosen over the alternative?",
		keywords: []string{"fnv", "standard library|stdlib", "cryptographic|crypto"},
	},
	{
		name: "failed-approach",
		plant: "We already tried DECSTBM scroll regions for the footer and abandoned them: " +
			"lines scrolled inside a region never reach the terminal scrollback. Do not try scroll regions again.",
		ack:      "Got it: scroll regions are a dead end because of scrollback loss.",
		probe:    "What footer approach was already tried and abandoned, and why must we not retry it?",
		keywords: []string{"scroll region|DECSTBM", "scrollback"},
	},
	{
		name:     "environment",
		plant:    "To test this project run: go test -tags rigfixture ./...  (the rigfixture build tag wires in the fake billing backend).",
		ack:      "Noted the test command and the rigfixture tag.",
		probe:    "What is the exact command to run this project's tests, including any build tags?",
		keywords: []string{"rigfixture", "go test"},
	},
	{
		name:     "verbatim",
		plant:    "Careful with environments: the staging gateway listens on port 7443, not 8443; 8443 is production.",
		ack:      "Noted: staging is 7443, production is 8443.",
		probe:    "Which port does the staging gateway listen on?",
		keywords: []string{"7443"},
	},
}

// negativeProbes ask about facts never established; the only honest answer is
// unknown. A chain that invents one has converted brief hallucination into a
// durable false memory, the failure mode the carry-forward rule could amplify,
// so this is measured alongside retention: keeping real facts is worthless if
// fake ones ride along.
var negativeProbes = []fact{
	{name: "neg-database", probe: "Which database did we choose for the ticket index?"},
	{name: "neg-deadline", probe: "What deadline did the user set for this work?"},
	{name: "neg-library", probe: "Which HTTP client library did we standardize on?"},
}

// scoreNegative gives credit only for an honest refusal.
func scoreNegative(answer string) float64 {
	low := strings.ToLower(answer)
	for _, ok := range []string{"unknown", "not specified", "no informat", "not establ", "was not", "wasn't",
		"did not", "didn't", "none", "no decision", "not mentioned", "not discussed", "no deadline", "not set"} {
		if strings.Contains(low, ok) {
			return 1
		}
	}
	return 0
}

// plantHistory turns the facts into the opening exchanges of a session.
func plantHistory(facts []fact) []agent.Turn {
	var h []agent.Turn
	for _, f := range facts {
		h = append(h,
			agent.Turn{Role: "user", Text: f.plant},
			agent.Turn{Role: "assistant", Text: f.ack},
		)
	}
	return h
}

// fillerTopics seed the synthetic work that buries the planted facts. They are
// deliberately plausible coding-session material (the summarizer must compete
// for space) and deliberately unrelated to every planted fact.
var fillerTopics = []string{
	"the invoice renderer's date formatting",
	"flaky retries in the webhook dispatcher",
	"pagination cursors in the admin list view",
	"the CSV importer's quoting rules",
	"timezone handling in the report scheduler",
	"slug collisions in the article store",
	"the image thumbnailer's EXIF rotation",
	"rate limiting on the public search endpoint",
}

// fillerExchange fabricates one deterministic user/assistant exchange of
// roughly 1.5k characters. Varying the shape by index avoids training the
// summarizer on a uniform pattern it could trivially collapse.
func fillerExchange(seed, i int) (string, string) {
	topic := fillerTopics[(seed+i)%len(fillerTopics)]
	user := fmt.Sprintf("While you are at it, take a look at %s; ticket %d-%d says it misbehaves under load.", topic, seed, i)
	var b strings.Builder
	fmt.Fprintf(&b, "I traced the issue with %s. The handler in module m%d builds its state at request time, "+
		"and under load the pool in worker_%d.go is exhausted before the queue drains. ", topic, i, (seed+i)%7)
	for p := 0; p < 6; p++ {
		fmt.Fprintf(&b, "Step %d: inspected frame %d-%d, found counter at %d and latency near %dms, which rules out the cache layer. ",
			p+1, i, p, (i*131+p*17)%9973, (i*37+p*11)%450+20)
	}
	fmt.Fprintf(&b, "Proposed fix: precompute the table for %s at startup and bound the worker pool at %d. "+
		"I verified with a synthetic load of %d requests; p99 dropped to %dms.",
		topic, (i%8)+4, (i*211)%5000+500, (i*13)%80+15)
	return user, b.String()
}

// addFiller appends n synthetic exchanges to the history.
func addFiller(h []agent.Turn, n, seed int) []agent.Turn {
	for i := 0; i < n; i++ {
		u, a := fillerExchange(seed, i)
		h = append(h, agent.Turn{Role: "user", Text: u}, agent.Turn{Role: "assistant", Text: a})
	}
	return h
}

// scoreAnswer is the fraction of keyword sets satisfied by the answer. Each
// keyword is a |-separated list of alternatives; any one of them counts.
func scoreAnswer(answer string, keywords []string) float64 {
	if len(keywords) == 0 {
		return 0
	}
	low := strings.ToLower(answer)
	hit := 0
	for _, kw := range keywords {
		for _, alt := range strings.Split(kw, "|") {
			if strings.Contains(low, strings.ToLower(alt)) {
				hit++
				break
			}
		}
	}
	return float64(hit) / float64(len(keywords))
}

// ---------------------------------------------------------------------------
// Unit tests for the rig itself (always run).
// ---------------------------------------------------------------------------

func TestScoreAnswer(t *testing.T) {
	if got := scoreAnswer("we use FNV-32a from the stdlib, not crypto", []string{"fnv", "standard library|stdlib", "cryptographic|crypto"}); got != 1.0 {
		t.Fatalf("alternatives and case folding should all hit, got %v", got)
	}
	if got := scoreAnswer("port is 8443", []string{"7443"}); got != 0 {
		t.Fatalf("miss should score 0, got %v", got)
	}
	if got := scoreAnswer("no redis allowed", []string{"redis", "64", "in-process|in process"}); got < 0.32 || got > 0.34 {
		t.Fatalf("partial recall should score 1/3, got %v", got)
	}
}

func TestRigHistoryShape(t *testing.T) {
	h := addFiller(plantHistory(plantedFacts), 10, 1)
	for i, turn := range h {
		want := "user"
		if i%2 == 1 {
			want = "assistant"
		}
		if turn.Role != want {
			t.Fatalf("turn %d role %q, want %q: probes must append cleanly after an assistant turn", i, turn.Role, want)
		}
	}
	if h[len(h)-1].Role != "assistant" {
		t.Fatal("history must end with an assistant turn")
	}
	u1, a1 := fillerExchange(3, 5)
	u2, a2 := fillerExchange(3, 5)
	if u1 != u2 || a1 != a2 {
		t.Fatal("filler must be deterministic for reproducible runs")
	}
	if len(a1) < 600 {
		t.Fatalf("filler exchanges should carry real bulk, got %d chars", len(a1))
	}
}

// ---------------------------------------------------------------------------
// The live experiment (opt-in: needs a configured provider).
// ---------------------------------------------------------------------------

// rigProvider builds a real provider from the user's own configuration, like
// the harness would: profile, stored credential, flag-style overrides via env.
func rigProvider(t *testing.T) (agent.Provider, string) {
	return rigProviderEnv(t, "SESH_RIG_PROVIDER", "SESH_RIG_MODEL")
}

// rigProviderEnv resolves a provider selected by a pair of env vars, so an
// experiment can aim different roles (the worker, the brief writer) at
// different brains.
func rigProviderEnv(t *testing.T, provEnv, modelEnv string) (agent.Provider, string) {
	t.Helper()
	cfg := loadProviders()
	name := os.Getenv(provEnv) // empty resolves the configured default
	pname, prof, err := cfg.resolve(name)
	if err != nil {
		t.Fatalf("rig provider: %v", err)
	}
	model := prof.Model
	if m := os.Getenv(modelEnv); m != "" {
		model = m
	}
	key := prof.Key
	if key == "" {
		key = loadCredentials()[pname]
	}
	if err := resolveDefaults(prof.Protocol, &prof.URL, &model); err != nil {
		t.Fatalf("rig provider: %v", err)
	}
	p, err := buildProvider(prof.Protocol, prof.URL, model, key, prof.KeyEnv)
	if err != nil {
		t.Fatalf("rig provider: %v", err)
	}
	return p, model
}

// Probe instructions per condition. The baseline confines the model to its
// context; the chain wording permits the archived history without prescribing
// the recall tool (the handoff seed already introduces it).
const baseProbeInstr = "From this conversation's context only, answer briefly: %s If the conversation does not say, answer exactly: unknown."
const chainProbeInstr = "Answer briefly from this conversation, including its archived history: %s If you truly cannot find it, answer exactly: unknown."

// probe asks one fact's question against a copy of the history and scores the
// answer. Probes never contaminate the history they measure.
func probe(t *testing.T, p agent.Provider, system string, history []agent.Turn, f fact, tools []agent.Tool, instr string) (float64, string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	h := make([]agent.Turn, len(history), len(history)+1)
	copy(h, history)
	h = append(h, agent.Turn{Role: "user", Text: fmt.Sprintf(instr, f.probe)})
	out, _, err := agent.Run(ctx, p, system, h, tools, agent.Hooks{})
	if err != nil {
		t.Logf("probe %s errored: %v", f.name, err)
		return 0, ""
	}
	answer := lastText(out)
	return scoreAnswer(answer, f.keywords), answer
}

// probeAll scores every fact and returns per-fact scores plus the mean.
func probeAll(t *testing.T, p agent.Provider, system string, history []agent.Turn, tools []agent.Tool, instr string) (map[string]float64, float64) {
	scores := map[string]float64{}
	sum := 0.0
	for _, f := range plantedFacts {
		s, answer := probe(t, p, system, history, f, tools, instr)
		scores[f.name] = s
		sum += s
		t.Logf("  probe %-15s score %.2f  answer: %.110s", f.name, s, strings.ReplaceAll(answer, "\n", " "))
	}
	return scores, sum / float64(len(plantedFacts))
}

// TestRigRetention measures the generation-decay curve of in-place compaction:
// plant, bury, compact, bury, compact... probing after each generation. This is
// the baseline the continuity chain has to beat.
func TestRigRetention(t *testing.T) {
	if os.Getenv("SESH_RIG") == "" {
		t.Skip("live experiment; set SESH_RIG=1 (and optionally SESH_RIG_PROVIDER/SESH_RIG_MODEL)")
	}
	p, model := rigProvider(t)
	system := "You are a coding agent working on the billing service. Answer questions from the conversation context."
	const generations = 3

	h := addFiller(plantHistory(plantedFacts), 20, 1)
	t.Logf("== model %s · planted history ~%d tokens", model, approxTokens(h))

	t.Logf("== generation 0 (full history, sanity check)")
	_, mean := probeAll(t, p, system, h, nil, baseProbeInstr)
	t.Logf("== generation 0 mean retention %.2f", mean)
	if mean < 0.5 {
		t.Logf("warning: model cannot even recall from full context; results below say little")
	}

	for g := 1; g <= generations; g++ {
		h = compactHistory(p, system, h)
		h = addFiller(h, 10, g+1) // new work arrives between compactions
		t.Logf("== generation %d (after compaction %d, ~%d tokens)", g, g, approxTokens(h))
		_, mean := probeAll(t, p, system, h, nil, baseProbeInstr)
		t.Logf("== generation %d mean retention %.2f", g, mean)
	}
}

// TestRigChain is the same decay measurement through the continuity chain:
// handoff instead of in-place compaction, probing after each generation.
// Sessions are sandboxed in a temp HOME so the rig never touches real ones.
// SESH_RIG_NORECALL=1 ablates the recall tool to isolate what the brief,
// ledger, and verbatim tail preserve on their own.
func TestRigChain(t *testing.T) {
	if os.Getenv("SESH_RIG") == "" {
		t.Skip("live experiment; set SESH_RIG=1 (and optionally SESH_RIG_PROVIDER/SESH_RIG_MODEL)")
	}
	p, model := rigProvider(t) // resolves config from the real HOME first
	bp, bmodel := p, model
	if os.Getenv("SESH_RIG_BRIEF_PROVIDER") != "" || os.Getenv("SESH_RIG_BRIEF_MODEL") != "" {
		bp, bmodel = rigProviderEnv(t, "SESH_RIG_BRIEF_PROVIDER", "SESH_RIG_BRIEF_MODEL")
	}
	t.Setenv("HOME", t.TempDir())
	system := "You are a coding agent working on the billing service. Answer questions from the conversation context."
	withRecall := os.Getenv("SESH_RIG_NORECALL") == ""
	const generations = 3
	const tailBudget = 4000 // tokens, = a 32k window / 8

	sess := &Session{ID: newSessionID(), Cwd: "/rig"}
	h := addFiller(plantHistory(plantedFacts), 20, 1)
	sess.Turns = h
	if err := sess.save(); err != nil {
		t.Fatal(err)
	}
	t.Logf("== model %s · chain condition · recall=%v · brief writer %s · ~%d tokens", model, withRecall, bmodel, approxTokens(h))

	cur := sess
	var tools []agent.Tool
	if withRecall {
		tools = []agent.Tool{recallTool(func() *Session { return cur })}
	}
	for g := 1; g <= generations; g++ {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		brief, entry, _, err := writeBrief(ctx, bp, renderTranscript(h, 300))
		cancel()
		if err != nil {
			t.Fatalf("generation %d brief failed: %v", g, err)
		}
		t.Logf("-- generation %d brief --\n%s\n-- ledger entry: %s", g, brief, entry)
		cur.Turns = h
		cur.save() // archive the dying link in full: this is what recall searches
		cur = seedChain(cur, brief, entry, "(rig: no repo)", verbatimTail(h, tailBudget))
		if err := cur.save(); err != nil {
			t.Fatal(err)
		}
		h = addFiller(cur.Turns, 10, g+1) // new work arrives in the new link
		cur.Turns = h
		cur.save()

		t.Logf("== generation %d (after handoff %d, ~%d tokens, ledger %d)", g, g, approxTokens(h), len(cur.Ledger))
		_, mean := probeAll(t, p, system, h, tools, chainProbeInstr)
		t.Logf("== generation %d mean retention %.2f", g, mean)

		// false-memory check: nothing was ever established about these
		negSum := 0.0
		for _, f := range negativeProbes {
			_, answer := probe(t, p, system, h, f, tools, chainProbeInstr)
			s := scoreNegative(answer)
			negSum += s
			t.Logf("  %-21s honest %.0f  answer: %.90s", f.name, s, strings.ReplaceAll(answer, "\n", " "))
		}
		t.Logf("== generation %d honesty %.2f (1.00 = no invented facts)", g, negSum/float64(len(negativeProbes)))
	}
}
