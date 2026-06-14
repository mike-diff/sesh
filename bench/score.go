// Package bench folds the end-to-end retention bench into the tree. It drives a
// real sesh binary through a scripted user session against the ticketsvc
// fixture, then scores what survived the handoff chain. The live run is opt-in
// (see bench_test.go); everything in this file is pure and unit-tested, so the
// scoring logic is provable without a provider.
//
// This is a Go port of the original hbench-score.py, brought forward to the
// post-rebrand truth: sessions live under ~/.sesh/sessions, the binary is
// sesh, and the exit hint prints "sesh -resume <id>". The handoff economics
// (what a re-fed, uncached prefix costs per link) are read from the chain
// ledger that sesh already persists, which the Python scorer never looked at.
package bench

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// ---------------------------------------------------------------------------
// On-disk session shape. These mirror exactly the JSON that harness writes
// (session.go, agent.Turn, agent.ToolCall): the bench reads the files sesh
// produced, so it owns its own minimal view rather than importing harness
// internals. A field rename in the product that this struct does not track
// shows up as a zero value here, which the live bench surfaces as a failed
// chain walk, the honest signal that the format drifted.
// ---------------------------------------------------------------------------

type session struct {
	ID     string   `json:"id"`
	Parent string   `json:"parent"`
	Ledger []string `json:"ledger"`
	Turns  []turn   `json:"turns"`
}

type turn struct {
	Role  string     `json:"role"`
	Text  string     `json:"text"`
	Calls []toolCall `json:"calls"`
}

// toolCall mirrors agent.ToolCall, which carries no json tags, so Go marshals
// it under its exported field names. The Python scorer matched c.get("Name");
// we match the same capitalized key by mirroring the field name.
type toolCall struct {
	Name string
}

// ---------------------------------------------------------------------------
// Log parsing. The footer transcript is the bench's record of what sesh did at
// the session boundary. We match the CURRENT output, not the pre-rebrand log
// the snapshot carried: harness.repl prints "handed off: <from> continues as
// <to> (...)" and goodbye prints "sesh -resume <id>".
// ---------------------------------------------------------------------------

// ansi strips terminal color codes so the patterns match the plain text. The
// footer wraps nearly every line in \033[2m ... \033[0m.
var ansi = regexp.MustCompile(`\x1b\[[0-9;]*[a-zA-Z]`)

// these mirror harness output verbatim; if either line is reworded, the live
// bench loses its anchor and the matching unit test names exactly which.
var (
	handoffRe = regexp.MustCompile(`handed off: (\S+) continues as (\S+)`)
	resumeRe  = regexp.MustCompile(`sesh -resume (\S+)`)
	errorRe   = regexp.MustCompile(`(?m)^.*error: .*$`)
)

// logReport is what one run's transcript tells us before we touch the chain.
type logReport struct {
	Handoffs [][2]string // from, to pairs in order
	FinalID  string      // the session the run ended on (last resume hint)
	Errors   []string
}

func parseLog(raw string) logReport {
	clean := ansi.ReplaceAllString(raw, "")
	var r logReport
	for _, m := range handoffRe.FindAllStringSubmatch(clean, -1) {
		r.Handoffs = append(r.Handoffs, [2]string{m[1], m[2]})
	}
	resumes := resumeRe.FindAllStringSubmatch(clean, -1)
	if len(resumes) > 0 {
		r.FinalID = resumes[len(resumes)-1][1]
	}
	for _, m := range errorRe.FindAllString(clean, -1) {
		r.Errors = append(r.Errors, strings.TrimSpace(m))
	}
	return r
}

// ---------------------------------------------------------------------------
// Chain walking. From the final session id we follow parent pointers back to
// the root, then return the chain oldest-first, the order the scorer reasons
// in (a fact is planted early and probed late).
// ---------------------------------------------------------------------------

func loadSession(dir, id string) (*session, error) {
	b, err := os.ReadFile(filepath.Join(dir, id+".json"))
	if err != nil {
		return nil, err
	}
	var s session
	if err := json.Unmarshal(b, &s); err != nil {
		return nil, fmt.Errorf("session %s: %w", id, err)
	}
	return &s, nil
}

// walkChain loads the chain ending at finalID, oldest first. A cycle (a corrupt
// parent pointer) is broken by the seen set rather than looping forever.
func walkChain(dir, finalID string) ([]*session, error) {
	var chain []*session
	seen := map[string]bool{}
	id := finalID
	for id != "" && !seen[id] {
		seen[id] = true
		s, err := loadSession(dir, id)
		if err != nil {
			return nil, err
		}
		chain = append(chain, s)
		id = s.Parent
	}
	// reverse to oldest-last -> oldest-first
	for i, j := 0, len(chain)-1; i < j; i, j = i+1, j-1 {
		chain[i], chain[j] = chain[j], chain[i]
	}
	return chain, nil
}

// ---------------------------------------------------------------------------
// Probe scoring. A probe is a planted fact and the keyword sets that prove its
// answer survived. Each keyword is a |-separated alternative set, satisfied if
// any alternative appears (case-insensitive) in the answer. This is the same
// scoring the retention rig uses; the bench keeps its own copy so the two
// layers can diverge in fixtures without coupling.
// ---------------------------------------------------------------------------

type probe struct {
	Name     string
	Needle   string   // a lowercase substring that marks the probe's user turn
	Keywords []string // |-separated alternative sets
}

// defaultProbes are the five planted facts from inputs.txt, in the categories
// research says a summary loses first: a negative constraint, a decision with
// rationale, a failed approach, an environment fact, and a verbatim detail.
var defaultProbes = []probe{
	{"constraint", "caching technology is forbidden", []string{"redis", "64", "in-process|in process"}},
	{"decision", "hash function do we use", []string{"fnv", "standard library|stdlib", "cryptographic|crypto"}},
	{"failed-approach", "footer approach", []string{"scroll region|decstbm", "scrollback"}},
	{"environment", "command to run this project", []string{"rigfixture", "go test"}},
	{"verbatim", "staging gateway listen", []string{"7443"}},
}

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

// seedMarker identifies a handoff seed turn. The seed is replayed into the new
// session as a user turn that quotes the planted facts, so a naive search would
// score the seed copy instead of the model's real answer. The product's seed
// template opens with this exact phrase (continuity.go, seedTemplate).
const seedMarker = "this conversation continues session"

// findAnswer reproduces hbench-score.py's find_answer: scan the chain oldest
// first for the LAST user turn whose text contains the needle and is not a
// handoff seed, then take the assistant text that follows it before the next
// user turn. Returns the link index (0-based) and the answer; link is -1 when
// the probe was never asked.
func findAnswer(chain []*session, needle string) (int, string) {
	link, best := -1, ""
	needle = strings.ToLower(needle)
	for li, s := range chain {
		turns := s.Turns
		for i, t := range turns {
			if t.Role != "user" || !strings.Contains(strings.ToLower(t.Text), needle) {
				continue
			}
			if strings.Contains(strings.ToLower(t.Text), seedMarker) {
				continue // a seed copy, not the user actually asking
			}
			for _, u := range turns[i+1:] {
				if u.Role == "user" {
					break
				}
				if u.Role == "assistant" && u.Text != "" {
					link, best = li, u.Text
				}
			}
		}
	}
	return link, best
}

// countRecalls totals recall tool calls across the whole chain: the bench's
// measure of how often the model reached past its window into archived history.
func countRecalls(chain []*session) int {
	n := 0
	for _, s := range chain {
		for _, t := range s.Turns {
			for _, c := range t.Calls {
				if c.Name == "recall" {
					n++
				}
			}
		}
	}
	return n
}

// ---------------------------------------------------------------------------
// Retention scoring: tie the log, the chain, and the probes together.
// ---------------------------------------------------------------------------

// probeResult is one fact's outcome.
type probeResult struct {
	Name   string
	Score  float64
	Link   int // 1-based link the answer came from, 0 when never asked
	Answer string
}

// retention is the bench's verdict on what survived.
type retention struct {
	Probes  []probeResult
	Mean    float64
	Recalls int
}

func scoreRetention(chain []*session, probes []probe) retention {
	var r retention
	r.Recalls = countRecalls(chain)
	sum := 0.0
	for _, p := range probes {
		li, answer := findAnswer(chain, p.Needle)
		s := scoreAnswer(answer, p.Keywords)
		sum += s
		r.Probes = append(r.Probes, probeResult{Name: p.Name, Score: s, Link: li + 1, Answer: answer})
	}
	if len(probes) > 0 {
		r.Mean = sum / float64(len(probes))
	}
	return r
}

// ---------------------------------------------------------------------------
// Handoff economics. This is the unmeasured metric the fold-in adds. sesh
// already persists, per chain link, what each handoff cost: the chain ledger
// is one JSONL record per handoff (continuity.go, chainRecord). We read it
// rather than the session files because usage is not persisted per session;
// the ledger is the durable, per-link cost record sesh keeps.
//
// What a handoff costs in re-fed, uncached prefix: every link past the first
// re-reads its seeded context (ledger, brief, verbatim tail) plus the standing
// system prompt as fresh prompt tokens. CtxTokens is the context size that
// triggered each handoff, the high-water mark that the chain chose to break
// rather than keep paying for in one session. BriefIn/BriefOut is the extra
// cost of writing the handoff brief itself, a fresh-context model call.
//
// Cached-prefix detail: both adapters now surface cache reads into the
// neutral agent.Usage.CacheRead (the OpenAI adapter reads
// usage.prompt_tokens_details.cached_tokens; Anthropic reads
// cache_read_input_tokens), and the ledger persists the brief call's cached
// fraction as cached_in. What that measures is the brief surcharge's cache
// hit, the one per-handoff model call the ledger records. The cached fraction
// of each link's ordinary turns is still not persisted anywhere, so the
// re-fed prefix figure stays in full prompt tokens. A provider that reports
// no cache detail (ollama does not) simply leaves cached_in absent, and the
// report says so instead of inferring a ratio.
// ---------------------------------------------------------------------------

// chainRecord mirrors continuity.go's chainRecord on disk.
type chainRecord struct {
	From      string `json:"from"`
	To        string `json:"to"`
	Entry     string `json:"entry"`
	CtxTokens int    `json:"ctx_tokens"`
	BriefIn   int    `json:"brief_in"`
	BriefOut  int    `json:"brief_out"`
	CachedIn  int    `json:"cached_in"`
}

// readLedger parses one chain's JSONL, oldest first. A torn final line (a crash
// mid-append) is skipped, never fatal, matching how sesh itself reads it.
func readLedger(chainsDir, root string) ([]chainRecord, error) {
	b, err := os.ReadFile(filepath.Join(chainsDir, root+".jsonl"))
	if err != nil {
		return nil, err
	}
	var recs []chainRecord
	for _, line := range strings.Split(string(b), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var rec chainRecord
		if json.Unmarshal([]byte(line), &rec) == nil {
			recs = append(recs, rec)
		}
	}
	return recs, nil
}

// economics is the handoff-cost summary: what staying in one session would
// have cost versus what the chain actually paid to keep going.
type economics struct {
	Handoffs int // number of links broken
	// RefedPrefixTokens sums CtxTokens across handoffs: the prompt tokens the
	// chain chose to re-seed rather than keep accumulating in one window. This
	// is the headline "what a handoff costs in re-fed prefix" figure.
	RefedPrefixTokens int
	// BriefInTokens / BriefOutTokens sum the cost of writing the briefs: the
	// surcharge the chain pays on top of the re-fed prefix.
	BriefInTokens  int
	BriefOutTokens int
	// PeakCtxTokens is the largest context a single link reached before handing
	// off: the window pressure the chain relieves.
	PeakCtxTokens int
	// BriefCachedTokens sums cached_in: how much of the briefs' prompts the
	// provider served from cache. The brief re-reads the dying session's
	// transcript, the exact prefix the session had been paying for, so a warm
	// cache shows up here first.
	BriefCachedTokens int
	// CachedTokensKnown is true only when some ledger record carried cached_in.
	// A provider that reports no cache detail leaves it absent (cached_in is
	// omitempty), which is indistinguishable from a cold cache; consumers must
	// not infer a cache ratio when this is false.
	CachedTokensKnown bool
}

func summarizeEconomics(recs []chainRecord) economics {
	var e economics
	e.Handoffs = len(recs)
	for _, r := range recs {
		e.RefedPrefixTokens += r.CtxTokens
		e.BriefInTokens += r.BriefIn
		e.BriefOutTokens += r.BriefOut
		e.BriefCachedTokens += r.CachedIn
		if r.CtxTokens > e.PeakCtxTokens {
			e.PeakCtxTokens = r.CtxTokens
		}
	}
	e.CachedTokensKnown = e.BriefCachedTokens > 0
	return e
}

// ---------------------------------------------------------------------------
// Rendering the report. A plain, greppable block: the same shape the Python
// scorer printed, plus the handoff-economics section it lacked.
// ---------------------------------------------------------------------------

func renderReport(label string, log logReport, chain []*session, ret retention, econ economics) string {
	var b strings.Builder
	ids := make([]string, len(chain))
	for i, s := range chain {
		ids[i] = s.ID
	}
	fmt.Fprintf(&b, "== %s: %d handoffs in log; final session %s\n", label, len(log.Handoffs), log.FinalID)
	fmt.Fprintf(&b, "   chain on disk: %s\n", strings.Join(ids, " -> "))
	if len(chain) > 0 {
		fmt.Fprintf(&b, "   final ledger entries: %d\n", len(chain[len(chain)-1].Ledger))
	}
	for _, p := range ret.Probes {
		ans := strings.ReplaceAll(p.Answer, "\n", " ")
		if len(ans) > 80 {
			ans = ans[:80]
		}
		fmt.Fprintf(&b, "   probe %-16s score %.2f  [link %d/%d] %s\n", p.Name, p.Score, p.Link, len(chain), ans)
	}
	fmt.Fprintf(&b, "   mean retention %.2f · recall calls across chain: %d\n", ret.Mean, ret.Recalls)
	fmt.Fprintf(&b, "   handoff economics: %d handoffs · re-fed prefix %d prompt tokens · brief surcharge %d in / %d out · peak ctx %d\n",
		econ.Handoffs, econ.RefedPrefixTokens, econ.BriefInTokens, econ.BriefOutTokens, econ.PeakCtxTokens)
	if econ.CachedTokensKnown {
		total := econ.BriefInTokens + econ.BriefCachedTokens
		fmt.Fprintf(&b, "   brief prompts served from cache: %d of %d tokens (%.0f%%)\n",
			econ.BriefCachedTokens, total, 100*float64(econ.BriefCachedTokens)/float64(total))
	} else {
		fmt.Fprintf(&b, "   cached tokens: none reported on this chain (provider sent no cache detail, or the cache was cold)\n")
	}
	fmt.Fprintf(&b, "   errors in log: %d", len(log.Errors))
	if len(log.Errors) > 0 {
		first := log.Errors[0]
		if len(first) > 80 {
			first = first[:80]
		}
		fmt.Fprintf(&b, " (first: %s)", first)
	}
	b.WriteByte('\n')
	return b.String()
}
