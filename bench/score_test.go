package bench

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// TestParseLogExtractsHandoffsAndFinal asserts the bench reads the CURRENT sesh
// output, not the pre-rebrand log. Breaker: change handoffRe or resumeRe to the
// old "harness -resume" wording and the final id goes empty, failing here.
func TestParseLogExtractsHandoffsAndFinal(t *testing.T) {
	log := "\x1b[2m  handed off: aaa continues as bbb (handoff 1; brief cost 10 in / 2 out tokens)\x1b[0m\n" +
		"\x1b[2m  handed off: bbb continues as ccc (handoff 2; brief cost 9 in / 3 out tokens)\x1b[0m\n" +
		"\x1b[2msesh -resume ccc\x1b[0m\n"
	r := parseLog(log)
	if len(r.Handoffs) != 2 {
		t.Fatalf("want 2 handoffs, got %d: the handoff line wording drifted from harness.repl", len(r.Handoffs))
	}
	if r.Handoffs[0] != [2]string{"aaa", "bbb"} || r.Handoffs[1] != [2]string{"bbb", "ccc"} {
		t.Fatalf("handoff pairs wrong: %v", r.Handoffs)
	}
	if r.FinalID != "ccc" {
		t.Fatalf("final id %q, want ccc: the resume hint must match goodbye's 'sesh -resume <id>'", r.FinalID)
	}
}

// TestParseLogLastResumeWins asserts the FINAL resume hint is taken, not the
// first. A chain prints one per handoff in some traces; only the last names the
// live tip. Breaker: take resumes[0] instead of the last and this fails.
func TestParseLogLastResumeWins(t *testing.T) {
	log := "sesh -resume first\nsesh -resume second\nsesh -resume last\n"
	if got := parseLog(log).FinalID; got != "last" {
		t.Fatalf("final id %q, want last", got)
	}
}

// TestParseLogCountsErrors asserts error lines are surfaced. Breaker: drop the
// errorRe match and a run that errored looks clean.
func TestParseLogCountsErrors(t *testing.T) {
	log := "all good\n  error: connection refused\nmore\n  error: timeout\n"
	if got := parseLog(log).Errors; len(got) != 2 {
		t.Fatalf("want 2 errors, got %d (%v)", len(got), got)
	}
}

// writeSession is a test helper that writes a session JSON exactly as sesh does.
func writeSession(t *testing.T, dir string, s session) {
	t.Helper()
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, s.ID+".json"), b, 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestWalkChainOldestFirst asserts parent pointers are followed back to the
// root and returned oldest first, the order probes reason in. Breaker: skip the
// reverse step and the chain comes back newest first, flipping link indices.
func TestWalkChainOldestFirst(t *testing.T) {
	dir := t.TempDir()
	writeSession(t, dir, session{ID: "root", Parent: ""})
	writeSession(t, dir, session{ID: "mid", Parent: "root"})
	writeSession(t, dir, session{ID: "tip", Parent: "mid"})

	chain, err := walkChain(dir, "tip")
	if err != nil {
		t.Fatal(err)
	}
	got := []string{chain[0].ID, chain[1].ID, chain[2].ID}
	want := []string{"root", "mid", "tip"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("chain order %v, want %v: walk must return oldest first", got, want)
		}
	}
}

// TestWalkChainBreaksCycle asserts a corrupt parent pointer cannot loop the
// walk forever. Breaker: drop the seen-set guard and this hangs instead of
// returning, which the test timeout would catch.
func TestWalkChainBreaksCycle(t *testing.T) {
	dir := t.TempDir()
	writeSession(t, dir, session{ID: "a", Parent: "b"})
	writeSession(t, dir, session{ID: "b", Parent: "a"})

	chain, err := walkChain(dir, "a")
	if err != nil {
		t.Fatal(err)
	}
	if len(chain) != 2 {
		t.Fatalf("cycle walk returned %d links, want 2 (each visited once)", len(chain))
	}
}

// TestFindAnswerTakesLastRealOccurrence asserts the scorer scores the model's
// real answer, not an earlier mention or a replayed seed copy. Breaker: stop
// skipping seed turns and the planted-fact copy in the seed scores instead of
// the answer; or take the first occurrence and an early stub scores.
func TestFindAnswerTakesLastRealOccurrence(t *testing.T) {
	chain := []*session{
		{ID: "s1", Turns: []turn{
			// a seed copy quoting the probe text: must be skipped
			{Role: "user", Text: "This conversation continues session s0. footer approach was tried"},
			{Role: "assistant", Text: "Understood, continuing."},
		}},
		{ID: "s2", Turns: []turn{
			{Role: "user", Text: "what footer approach was abandoned?"},
			{Role: "assistant", Text: "DECSTBM scroll regions, because of scrollback loss."},
			{Role: "user", Text: "next unrelated question"},
			{Role: "assistant", Text: "unrelated answer"},
		}},
	}
	li, answer := findAnswer(chain, "footer approach")
	if li != 1 {
		t.Fatalf("answer came from link %d, want 1: the seed copy in link 0 must be skipped", li)
	}
	if answer != "DECSTBM scroll regions, because of scrollback loss." {
		t.Fatalf("wrong answer %q: must take the assistant turn after the real user probe", answer)
	}
}

// TestFindAnswerMissingProbe asserts an unasked probe reports link -1 and an
// empty answer (scores zero downstream). Breaker: default link to 0 and an
// unasked probe would falsely attribute to the first link.
func TestFindAnswerMissingProbe(t *testing.T) {
	chain := []*session{{ID: "s1", Turns: []turn{{Role: "user", Text: "hello"}}}}
	li, answer := findAnswer(chain, "never asked")
	if li != -1 || answer != "" {
		t.Fatalf("missing probe gave link %d answer %q, want -1 and empty", li, answer)
	}
}

// TestScoreAnswerAlternativesAndCase asserts |-alternatives and case folding
// both hit. Breaker: compare case-sensitively, or stop splitting on |, and a
// correct answer phrased differently scores below 1.0.
func TestScoreAnswerAlternativesAndCase(t *testing.T) {
	got := scoreAnswer("We use FNV-32a from the STDLIB, not crypto",
		[]string{"fnv", "standard library|stdlib", "cryptographic|crypto"})
	if got != 1.0 {
		t.Fatalf("alternatives and case folding should all hit, got %v", got)
	}
}

// TestScoreAnswerPartial asserts a partial recall scores the right fraction.
// Breaker: divide by a wrong denominator and the fraction drifts.
func TestScoreAnswerPartial(t *testing.T) {
	got := scoreAnswer("no redis allowed", []string{"redis", "64", "in-process|in process"})
	if got < 0.32 || got > 0.34 {
		t.Fatalf("one of three keyword sets hit should score ~1/3, got %v", got)
	}
}

// TestScoreAnswerMiss asserts a wrong answer scores zero. Breaker: a substring
// false positive (e.g. matching "443" inside other numbers) would lift this.
func TestScoreAnswerMiss(t *testing.T) {
	if got := scoreAnswer("the port is 8443", []string{"7443"}); got != 0 {
		t.Fatalf("8443 must not satisfy the 7443 keyword, got %v", got)
	}
}

// TestCountRecalls asserts recall tool calls are counted across the whole
// chain, matching the capitalized Name key sesh writes. Breaker: match "Recall"
// or a lowercased "name" key and the count drops to zero.
func TestCountRecalls(t *testing.T) {
	chain := []*session{
		{Turns: []turn{
			{Role: "assistant", Calls: []toolCall{{Name: "recall"}, {Name: "read"}}},
		}},
		{Turns: []turn{
			{Role: "assistant", Calls: []toolCall{{Name: "recall"}}},
		}},
	}
	if got := countRecalls(chain); got != 2 {
		t.Fatalf("want 2 recall calls across the chain, got %d", got)
	}
}

// TestToolCallNameKeyMatchesProduct guards the one coupling to sesh's on-disk
// shape: agent.ToolCall carries no json tags, so Name marshals as "Name". If
// that ever changes, recall counting silently breaks. Breaker: lowercase the
// key in the encoded fixture and the decode misses, dropping the count to zero.
func TestToolCallNameKeyMatchesProduct(t *testing.T) {
	// the exact bytes sesh writes for one tool call
	raw := `{"turns":[{"role":"assistant","calls":[{"ID":"x","Name":"recall","Args":{}}]}]}`
	var s session
	if err := json.Unmarshal([]byte(raw), &s); err != nil {
		t.Fatal(err)
	}
	if got := countRecalls([]*session{&s}); got != 1 {
		t.Fatalf("recall call under the capitalized Name key did not decode, got %d", got)
	}
}

// TestReadLedgerSkipsTornLine asserts a crash-torn final JSONL line is skipped,
// not fatal, matching how sesh reads its own ledger. Breaker: fail on the bad
// line and a recoverable file becomes an error.
func TestReadLedgerSkipsTornLine(t *testing.T) {
	dir := t.TempDir()
	content := `{"from":"a","to":"b","ctx_tokens":1000,"brief_in":120,"brief_out":40}` + "\n" +
		`{"from":"b","to":"c","ctx_tokens":1500,"brief_in":` // torn: no newline, truncated
	if err := os.WriteFile(filepath.Join(dir, "root.jsonl"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	recs, err := readLedger(dir, "root")
	if err != nil {
		t.Fatalf("torn ledger must not error: %v", err)
	}
	if len(recs) != 1 {
		t.Fatalf("want 1 intact record, got %d", len(recs))
	}
}

// TestSummarizeEconomics asserts the handoff-cost aggregation: re-fed prefix is
// the sum of per-link context sizes, the brief surcharge sums separately, and
// peak is the high-water mark. Breaker: sum brief tokens into the prefix total
// and the headline figure double-counts the surcharge.
func TestSummarizeEconomics(t *testing.T) {
	recs := []chainRecord{
		{CtxTokens: 5800, BriefIn: 1200, BriefOut: 400},
		{CtxTokens: 6100, BriefIn: 1100, BriefOut: 380},
		{CtxTokens: 5950, BriefIn: 1300, BriefOut: 410},
	}
	e := summarizeEconomics(recs)
	if e.Handoffs != 3 {
		t.Fatalf("handoffs %d, want 3", e.Handoffs)
	}
	if e.RefedPrefixTokens != 5800+6100+5950 {
		t.Fatalf("re-fed prefix %d, want %d: must sum ctx_tokens only", e.RefedPrefixTokens, 5800+6100+5950)
	}
	if e.BriefInTokens != 1200+1100+1300 || e.BriefOutTokens != 400+380+410 {
		t.Fatalf("brief surcharge wrong: in=%d out=%d", e.BriefInTokens, e.BriefOutTokens)
	}
	if e.PeakCtxTokens != 6100 {
		t.Fatalf("peak ctx %d, want 6100", e.PeakCtxTokens)
	}
	if e.CachedTokensKnown {
		t.Fatal("no record carried cached_in; CachedTokensKnown must stay false so no one infers a cache ratio")
	}
}

// TestSummarizeEconomicsCachedIn asserts cache detail is reported only when the
// ledger actually carries it: cached_in sums, and its presence flips
// CachedTokensKnown. Breaker: leave CachedTokensKnown hardcoded false (or key it
// on anything but the cached_in sum) and a chain with real cache reads still
// claims the ratio is unknowable.
func TestSummarizeEconomicsCachedIn(t *testing.T) {
	recs := []chainRecord{
		{CtxTokens: 5800, BriefIn: 200, BriefOut: 400, CachedIn: 1000},
		{CtxTokens: 6100, BriefIn: 100, BriefOut: 380, CachedIn: 1100},
	}
	e := summarizeEconomics(recs)
	if !e.CachedTokensKnown {
		t.Fatal("records carried cached_in; CachedTokensKnown must be true")
	}
	if e.BriefCachedTokens != 2100 {
		t.Fatalf("brief cached tokens %d, want 2100", e.BriefCachedTokens)
	}
}
