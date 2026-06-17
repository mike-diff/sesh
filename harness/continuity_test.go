package harness

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/mike-diff/sesh/agent"
)

func turnsOf(pairs ...string) []agent.Turn {
	var out []agent.Turn
	for i, text := range pairs {
		role := "user"
		if i%2 == 1 {
			role = "assistant"
		}
		out = append(out, agent.Turn{Role: role, Text: text})
	}
	return out
}

func TestVerbatimTail(t *testing.T) {
	big := strings.Repeat("x", 8000) // ~2000 tokens
	h := turnsOf("u1 "+big, "a1 "+big, "u2 "+big, "a2 "+big, "u3", "a3")

	// budget for the last pair only
	tail := verbatimTail(h, 100)
	if len(tail) != 2 || tail[0].Text != "u3" {
		t.Fatalf("tail should snap to the last user turn: %d turns, first %q", len(tail), tail[0].Text)
	}
	// budget for everything
	if tail = verbatimTail(h, 1_000_000); len(tail) != len(h) {
		t.Fatalf("unlimited budget should keep all turns, got %d", len(tail))
	}
	if tail[0].Role != "user" {
		t.Fatal("tail must start at a user turn")
	}
	// budget too small for even the smallest user-rooted suffix
	huge := turnsOf("u "+strings.Repeat("y", 9000), "a")
	if tail = verbatimTail(huge, 100); tail != nil {
		t.Fatalf("an oversized suffix should yield no tail, got %d turns", len(tail))
	}
}

func TestRenderTranscript(t *testing.T) {
	h := []agent.Turn{
		{Role: "user", Text: "fix the bug"},
		{Role: "assistant", Text: "looking", Calls: []agent.ToolCall{{Name: "read", Args: json.RawMessage(`{"path":"a.go"}`)}}},
		{Role: "tool", Results: []agent.ToolResult{{Content: strings.Repeat("z", 1000) + "\nsecond line"}}},
		{Role: "assistant", Text: "done"},
	}
	got := renderTranscript(h, 100)
	for _, want := range []string{"USER: fix the bug", "ASSISTANT ran read", "TOOL RESULT: " + strings.Repeat("z", 100) + "...", "ASSISTANT: done"} {
		if !strings.Contains(got, want) {
			t.Fatalf("transcript missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, strings.Repeat("z", 101)) {
		t.Fatal("tool results must be elided to the stub")
	}
	// a huge transcript keeps head and tail, eliding the middle
	var hh []agent.Turn
	for i := 0; i < 300; i++ {
		hh = append(hh, agent.Turn{Role: "user", Text: fmt.Sprintf("m%d %s", i, strings.Repeat("w", 2000))})
	}
	long := renderTranscript(hh, 100)
	if len(long) > maxBriefTranscript+200 {
		t.Fatalf("over-cap transcript not bounded: %d chars", len(long))
	}
	if !strings.Contains(long, "m0 ") || !strings.Contains(long, "m299 ") || !strings.Contains(long, "middle of the transcript omitted") {
		t.Fatal("cap must keep the head and tail and mark the elision")
	}
}

func TestSplitLedger(t *testing.T) {
	brief, entry := splitLedger("1. Task: stuff\n2. Decisions\nLEDGER: Built the rig. Chose files over vectors because grep is in-distribution.")
	if strings.Contains(brief, "LEDGER") || !strings.HasPrefix(entry, "Built the rig.") {
		t.Fatalf("split wrong: brief=%q entry=%q", brief, entry)
	}
	brief, entry = splitLedger("just a brief with no marker\nmore")
	if brief == "" || entry != "just a brief with no marker" {
		t.Fatalf("fallback wrong: brief=%q entry=%q", brief, entry)
	}
}

func TestSeedChain(t *testing.T) {
	old := &Session{ID: "old-1", Cwd: "/w", Provider: "remote", Protocol: "openai", URL: "u", Model: "m",
		Ledger: []string{"first handoff entry"}}
	tail := turnsOf("recent question", "recent answer")
	next := seedChain(old, "THE BRIEF", "second entry", "branch: main", tail)

	if next.Parent != "old-1" || next.Provider != "remote" || next.Model != "m" || next.Cwd != "/w" {
		t.Fatalf("chain metadata wrong: %+v", next)
	}
	if len(next.Ledger) != 2 || next.Ledger[1] != "second entry" {
		t.Fatalf("ledger must append, never recompress: %v", next.Ledger)
	}
	if next.ID == old.ID {
		t.Fatal("next link needs its own id")
	}
	h := next.Turns
	if len(h) != 4 || h[0].Role != "user" || h[1].Role != "assistant" || h[2].Role != "user" {
		t.Fatalf("seed must keep roles alternating: %d turns", len(h))
	}
	for _, want := range []string{"continues session old-1", "1. first handoff entry", "2. second entry", "THE BRIEF", "branch: main", "recall"} {
		if !strings.Contains(h[0].Text, want) {
			t.Fatalf("seed missing %q", want)
		}
	}
	if h[2].Text != "recent question" {
		t.Fatal("verbatim tail must follow the handoff pair")
	}
}

func TestRecallAcrossChain(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	parent := &Session{ID: "rc-parent", Turns: []agent.Turn{
		{Role: "user", Text: "the staging port is 7443"},
		{Role: "assistant", Text: "noted"},
		{Role: "tool", Results: []agent.ToolResult{{Content: "grep hit: PORT=7443 in conf"}}},
	}}
	if err := parent.save(); err != nil {
		t.Fatal(err)
	}
	child := &Session{ID: "rc-child", Parent: "rc-parent", Turns: turnsOf("new work", "ok")}

	tool := recallTool(func() *Session { return child })
	out, isErr := tool.Run(context.Background(), json.RawMessage(`{"pattern":"7443"}`))
	if isErr {
		t.Fatalf("recall errored: %s", out)
	}
	if !strings.Contains(out, "rc-parent#0 user: the staging port is 7443") || !strings.Contains(out, "rc-parent#2 tool:") {
		t.Fatalf("recall missed chain content:\n%s", out)
	}

	if out, _ = tool.Run(context.Background(), json.RawMessage(`{"pattern":"zzz-not-there"}`)); out != "no matches in the session chain" {
		t.Fatalf("miss should say so: %q", out)
	}
	if out, isErr = tool.Run(context.Background(), json.RawMessage(`{"pattern":""}`)); !isErr {
		t.Fatalf("empty pattern must be an error: %q", out)
	}

	// a broken parent pointer degrades gracefully
	orphan := &Session{ID: "rc-orphan", Parent: "rc-gone", Turns: turnsOf("the word needle here", "ok")}
	tool = recallTool(func() *Session { return orphan })
	out, isErr = tool.Run(context.Background(), json.RawMessage(`{"pattern":"needle"}`))
	if isErr || !strings.Contains(out, "needle") || !strings.Contains(out, "could not be loaded") {
		t.Fatalf("broken chain handling: %q err=%v", out, isErr)
	}
}

// TestSessionJSONHasNoImageBytes: an image turn saved the way session.save
// marshals it carries the hash and metadata but never the base64 of Data, so the
// session file stays lean. Breaker: drop the json:"-" tag on Image.Data and the
// marshalled JSON contains the encoded bytes.
func TestSessionJSONHasNoImageBytes(t *testing.T) {
	data := []byte("PRETEND-IMAGE-BYTES-THAT-MUST-NOT-LEAK")
	sess := &Session{
		ID:    "img-sess",
		Turns: []agent.Turn{{Role: "user", Text: "look", Images: []agent.Image{{Hash: "abc123", MediaType: "image/png", Width: 100, Height: 80, Data: data}}}},
	}

	b, err := json.MarshalIndent(sess, "", "  ") // exactly how session.save marshals
	if err != nil {
		t.Fatal(err)
	}
	out := string(b)

	if !strings.Contains(out, "abc123") {
		t.Fatal("the session JSON must keep the image hash as the reference")
	}
	if strings.Contains(out, base64.StdEncoding.EncodeToString(data)) {
		t.Fatal("the session JSON must not contain the base64 of the image bytes")
	}
	if strings.Contains(out, string(data)) {
		t.Fatal("the raw image bytes must not appear in the session JSON")
	}
}

// TestHandoffCarriesImageRef: the verbatim tail copied into a successor session
// carries the image reference (hash/metadata) but not the bytes, and the
// successor resolves the bytes from the shared blob store via rehydrateImages.
// Breaker: exclude Images from the carried turn and the successor has no image
// ref to resolve.
func TestHandoffCarriesImageRef(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	bytesOnDisk := []byte("the shared blob the successor will resolve")
	hash, err := storeBlob(bytesOnDisk, "image/png")
	if err != nil {
		t.Fatal(err)
	}

	// A dying session whose tail is a user turn bearing an image with no Data
	// (the on-disk form: bytes live in the blob store, not the turn).
	dying := []agent.Turn{
		{Role: "user", Text: "older"}, {Role: "assistant", Text: "ok"},
		{Role: "user", Text: "what is in this screenshot?",
			Images: []agent.Image{{Hash: hash, MediaType: "image/png", Width: 100, Height: 80}}},
		{Role: "assistant", Text: "a chart"},
	}

	tail := verbatimTail(dying, 1_000_000)
	old := &Session{ID: "dying-1", Cwd: "/w"}
	next := seedChain(old, "BRIEF", "entry", "branch: main", tail)

	// the carried turn keeps the reference, not the bytes
	var imgTurn *agent.Turn
	for i := range next.Turns {
		if len(next.Turns[i].Images) > 0 {
			imgTurn = &next.Turns[i]
			break
		}
	}
	if imgTurn == nil {
		t.Fatal("the successor must carry the image-bearing turn from the tail")
	}
	if imgTurn.Images[0].Hash != hash {
		t.Fatalf("the carried image must keep its hash ref: %q", imgTurn.Images[0].Hash)
	}
	if len(imgTurn.Images[0].Data) != 0 {
		t.Fatal("the seed must carry a reference, not the bytes")
	}

	// the successor resolves the bytes from the shared blob store
	rehydrateImages(next.Turns)
	if string(imgTurn.Images[0].Data) != string(bytesOnDisk) {
		t.Fatalf("the successor must resolve Data from the shared blob store: got %q", imgTurn.Images[0].Data)
	}
}

// TestApproxTokensCountsImages: a turn carrying an image reports more tokens than
// the same turn without it, so the verbatim-tail budget accounts for image cost.
// Breaker: omit the image term in approxTokens and the two counts are equal.
func TestApproxTokensCountsImages(t *testing.T) {
	withText := []agent.Turn{{Role: "user", Text: "describe this"}}
	withImage := []agent.Turn{{Role: "user", Text: "describe this",
		Images: []agent.Image{{Hash: "h", MediaType: "image/png", Width: 1456, Height: 819}}}}

	plain := approxTokens(withText)
	withImg := approxTokens(withImage)
	if withImg <= plain {
		t.Fatalf("an image must add to the token estimate: text=%d image=%d", plain, withImg)
	}
	if want := estimateImageTokens(1456, 819); withImg-plain != want {
		t.Fatalf("the image term must equal its patch estimate: delta=%d want=%d", withImg-plain, want)
	}
}

// TestRenderTranscriptNotesImages: a user turn with images gets a one-line,
// byte-free note in the brief transcript so the brief writer knows an image
// existed. Breaker: skip the note in the user arm and the dimensions/media type
// never reach the transcript.
func TestRenderTranscriptNotesImages(t *testing.T) {
	h := []agent.Turn{{Role: "user", Text: "compare these",
		Images: []agent.Image{
			{Hash: "a", MediaType: "image/png", Width: 1456, Height: 819},
			{Hash: "b", MediaType: "image/jpeg", Width: 800, Height: 600},
		}}}
	got := renderTranscript(h, 100)

	for _, want := range []string{"USER: compare these", "2 image(s)", "image/png 1456x819", "image/jpeg 800x600"} {
		if !strings.Contains(got, want) {
			t.Fatalf("transcript missing %q:\n%s", want, got)
		}
	}
}

// TestReplHandoff: the full product flow: brief written, old session archived,
// new session seeded and live, ledger grown.
func TestReplHandoff(t *testing.T) {
	r := newTestRepl(t)
	r.p = fakeChat{text: "1. Task: build it\nLEDGER: Did the thing; chose X because Y."}
	r.sess.Cwd = "/w"
	r.history = turnsOf("u1", "a1", "u2", "a2", "u3 latest", "a3 latest")
	r.sess.Turns = r.history
	old := r.sess.ID

	if !r.handoff() {
		t.Fatal("handoff should succeed")
	}
	if r.sess.ID == old || r.sess.Parent != old {
		t.Fatalf("new session not chained: id=%s parent=%s", r.sess.ID, r.sess.Parent)
	}
	if len(r.sess.Ledger) != 1 || !strings.HasPrefix(r.sess.Ledger[0], "Did the thing") {
		t.Fatalf("ledger: %v", r.sess.Ledger)
	}
	if !strings.Contains(r.history[0].Text, "1. Task: build it") {
		t.Fatal("brief must seed the new history")
	}
	if lastText(r.history) != "a3 latest" {
		t.Fatal("verbatim tail must carry the latest exchanges")
	}
	// the dying session was archived in full
	archived, err := loadSession(old)
	if err != nil || len(archived.Turns) != 6 {
		t.Fatalf("old session not archived: %v turns=%d", err, len(archived.Turns))
	}

	// guards: thin history and missing provider refuse cleanly
	r2 := newTestRepl(t)
	r2.p = fakeChat{text: "x"}
	r2.history = turnsOf("u", "a")
	if r2.handoff() {
		t.Fatal("thin history must not hand off")
	}
	r2.p = nil
	r2.history = turnsOf("u1", "a1", "u2", "a2")
	if r2.handoff() {
		t.Fatal("nil provider must not hand off")
	}
}

// TestAutoHandoffTrigger: crossing the context threshold with auto management
// on hands off instead of compacting in place.
func TestAutoHandoffTrigger(t *testing.T) {
	r := newTestRepl(t)
	r.p = fakeChat{text: "brief\nLEDGER: entry."}
	r.ctxLimit = 1000
	r.history = turnsOf("u1", "a1", "u2", "a2")
	r.sess.Turns = r.history
	old := r.sess.ID

	r.afterTurn(agent.Usage{Input: 900, Output: 10, LastInput: 900})
	if r.sess.Parent != old {
		t.Fatalf("threshold crossing should chain a new session; parent=%q", r.sess.Parent)
	}
	if r.ctxTokens != 0 {
		t.Fatal("context gauge must reset after handoff")
	}
}

// TestSealedChainSemantics covers the resume/continue/fork edge cases around
// sealed sessions: a handoff must seal the parent, -resume must land on the
// chain tip, -continue must skip sealed links, forks must not inherit seals,
// and broken or cyclic chains must degrade instead of looping.
func TestSealedChainSemantics(t *testing.T) {
	r := newTestRepl(t)
	r.p = fakeChat{text: "brief\nLEDGER: entry."}
	r.sess.Cwd = "/w"
	r.history = turnsOf("u1", "a1", "u2", "a2")
	r.sess.Turns = r.history
	parentID := r.sess.ID
	if !r.handoff() {
		t.Fatal("handoff failed")
	}

	// the parent on disk is sealed and points forward
	parent, err := loadSession(parentID)
	if err != nil || parent.Child != r.sess.ID {
		t.Fatalf("parent not sealed: child=%q err=%v", parent.Child, err)
	}

	// chainTip walks a sealed link to the live end
	tip, hops := chainTip(parent)
	if tip.ID != r.sess.ID || hops != 1 {
		t.Fatalf("chainTip: got %s after %d hops", tip.ID, hops)
	}
	// and is a no-op on the tip itself
	if same, hops := chainTip(r.sess); same.ID != r.sess.ID || hops != 0 {
		t.Fatal("tip must resolve to itself")
	}

	// -continue must never pick the sealed parent, even if it saved later
	parent.save()
	got, err := latestSession("/w")
	if err != nil || got.ID != r.sess.ID {
		t.Fatalf("latestSession picked %v (err=%v), want the unsealed tip %s", got, err, r.sess.ID)
	}

	// a fork of the sealed parent branches the archive without the seal
	f, err := forkSession(parentID)
	if err != nil {
		t.Fatal(err)
	}
	if f.Child != "" {
		t.Fatal("a fork must not inherit continued_by")
	}
	if len(f.Turns) != len(parent.Turns) {
		t.Fatal("a fork must carry the archived turns")
	}

	// a missing child ends the walk at the last loadable link
	orphan := &Session{ID: "seal-orphan", Child: "seal-gone"}
	if tip, hops := chainTip(orphan); tip.ID != "seal-orphan" || hops != 0 {
		t.Fatalf("missing child should end the walk: %s %d", tip.ID, hops)
	}

	// a corrupt cycle terminates
	a := &Session{ID: "cyc-a", Child: "cyc-b"}
	b := &Session{ID: "cyc-b", Child: "cyc-a"}
	a.save()
	b.save()
	if _, hops := chainTip(a); hops > tune.RecallLinks {
		t.Fatalf("cycle not bounded: %d hops", hops)
	}
}

// TestPreflight: a message that can never fit is refused before any API call;
// one that would land deep in the reserve hands off first; small messages and
// unknown windows pass untouched.
func TestPreflight(t *testing.T) {
	r := newTestRepl(t)
	r.p = fakeChat{text: "brief\nLEDGER: entry."}
	r.ctxLimit = 10000

	if r.preflight(strings.Repeat("x", 40000)) != true { // ~10k tokens > 80% of window
		t.Fatal("an unfittable message must be refused")
	}
	if r.preflight("small message") {
		t.Fatal("a small message must pass")
	}

	// near the reserve: handoff fires before the message is sent
	r.history = turnsOf("u1", "a1", "u2", "a2")
	r.sess.Turns = r.history
	r.ctxTokens = 8500
	old := r.sess.ID
	if r.preflight(strings.Repeat("y", 4000)) { // ~1k tokens, lands at ~95%
		t.Fatal("a fitting message must not be refused")
	}
	if r.sess.ID == old || r.sess.Parent != old {
		t.Fatal("preflight should have handed off first")
	}

	// unknown window: preflight stays out of the way
	r2 := newTestRepl(t)
	r2.p = fakeChat{text: "x"}
	if r2.preflight(strings.Repeat("z", 100000)) {
		t.Fatal("unknown window must not refuse anything")
	}
}

// TestCapWindow: declared or discovered windows clamp to the useful maximum.
func TestCapWindow(t *testing.T) {
	if got := capWindow(1_000_000); got != tune.MaxUsefulContext {
		t.Fatalf("1M must clamp to %d, got %d", tune.MaxUsefulContext, got)
	}
	if got := capWindow(202_752); got != 202_752 {
		t.Fatalf("a window under the cap stays put: %d", got)
	}
	if got := capWindow(0); got != 0 {
		t.Fatalf("unknown stays unknown: %d", got)
	}
}

// TestSoftBoundaryDefers: in the 80-90% zone a mid-investigation turn (tool
// activity since the last user message) defers the handoff; 90%+ forces it,
// and a settled turn hands off in the soft zone.
func TestSoftBoundaryDefers(t *testing.T) {
	midInvestigation := []agent.Turn{
		{Role: "user", Text: "u1"}, {Role: "assistant", Text: "a1"},
		{Role: "user", Text: "dig in"},
		{Role: "assistant", Calls: []agent.ToolCall{{ID: "1", Name: "read"}}},
		{Role: "tool", Results: []agent.ToolResult{{ID: "1", Content: "stuff"}}},
		{Role: "assistant", Text: "found part of it"},
	}

	r := newTestRepl(t)
	r.p = fakeChat{text: "brief\nLEDGER: e."}
	r.ctxLimit = 1000
	r.history = midInvestigation
	r.sess.Turns = r.history
	old := r.sess.ID

	r.ctxTokens = 850 // soft zone, mid-investigation: defer
	r.managePressure()
	if r.sess.ID != old {
		t.Fatal("soft zone must defer a mid-investigation handoff")
	}
	r.ctxTokens = 950 // hard zone: force
	r.managePressure()
	if r.sess.Parent != old {
		t.Fatal("hard zone must hand off regardless")
	}

	// settled history hands off in the soft zone
	r2 := newTestRepl(t)
	r2.p = fakeChat{text: "brief\nLEDGER: e."}
	r2.ctxLimit = 1000
	r2.history = turnsOf("u1", "a1", "u2", "a2")
	r2.sess.Turns = r2.history
	old2 := r2.sess.ID
	r2.ctxTokens = 850
	r2.managePressure()
	if r2.sess.Parent != old2 {
		t.Fatal("a settled turn in the soft zone should hand off")
	}
}

// TestChainRecordCachedIn: the ledger persists the cached fraction of the
// brief call's prompt, the field the bench's cache-ratio reading depends on.
// Breaker: drop cached_in from chainRecord and the round-trip loses it.
func TestChainRecordCachedIn(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	rec := chainRecord{From: "a", To: "b", Entry: "e", CtxTokens: 5000, BriefIn: 200, BriefOut: 40, CachedIn: 4400}
	if err := appendChainRecord("root", rec); err != nil {
		t.Fatal(err)
	}
	got := readChain("root")
	if len(got) != 1 || got[0].CachedIn != 4400 {
		t.Fatalf("cached_in did not survive the ledger round-trip: %+v", got)
	}
}

// TestChainScale4000: the weeks-long-session question, settled empirically.
// 4000 synthetic handoffs (no model needed) must leave: a constant-size seed,
// a capped per-session ledger, a complete chain file, a resumable tip, and
// recall that discloses its depth limit instead of lying.
func TestChainScale4000(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	const N = 4000
	entry := strings.Repeat("decided something important because of a reason. ", 3) // ~150 chars, realistic

	start := time.Now()
	s := &Session{ID: "scale-root", Cwd: "/w"}
	if err := s.save(); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < N; i++ {
		next := seedChain(s, "the brief", fmt.Sprintf("handoff %d: %s", i+1, entry), "branch: main", nil)
		if err := appendChainRecord(next.Root, chainRecord{From: s.ID, To: next.ID, Entry: entry, CtxTokens: 160000}); err != nil {
			t.Fatal(err)
		}
		s.Child = next.ID
		if err := s.save(); err != nil {
			t.Fatal(err)
		}
		s = next
		if err := s.save(); err != nil {
			t.Fatal(err)
		}
	}
	buildTime := time.Since(start)

	// constant-size state regardless of depth
	if s.Hops != N {
		t.Fatalf("hops: %d", s.Hops)
	}
	if len(s.Ledger) != tune.SeedLedgerEntries {
		t.Fatalf("session ledger must stay capped: %d entries", len(s.Ledger))
	}
	seed := s.Turns[0].Text
	if len(seed) > 6000 {
		t.Fatalf("seed must not grow with chain depth: %d chars", len(seed))
	}
	if !strings.Contains(seed, fmt.Sprintf("(%d earlier entries", N-tune.SeedLedgerEntries)) {
		t.Fatal("seed must disclose the depth it is not carrying")
	}
	if !strings.Contains(seed, fmt.Sprintf("%d. handoff %d:", N, N)) {
		t.Fatal("ledger numbering must stay absolute at depth")
	}

	// the chain file holds every record
	recs := readChain("scale-root")
	if len(recs) != N {
		t.Fatalf("chain file: %d records", len(recs))
	}
	if fi, _ := os.Stat(chainPath("scale-root")); fi.Size() > 2<<20 {
		t.Fatalf("chain file unexpectedly large: %d bytes", fi.Size())
	}

	// resuming the root still reaches the tip
	root, err := loadSession("scale-root")
	if err != nil {
		t.Fatal(err)
	}
	tipStart := time.Now()
	tip, hops := chainTip(root)
	tipTime := time.Since(tipStart)
	if tip.ID != s.ID || hops != N {
		t.Fatalf("tip walk: %s after %d hops", tip.ID, hops)
	}

	// recall caps its walk and says so
	tool := recallTool(func() *Session { return s })
	out, isErr := tool.Run(context.Background(), json.RawMessage(`{"pattern":"zzz-absent"}`))
	if isErr || !strings.Contains(out, "chain continues deeper") {
		t.Fatalf("deep recall must disclose its window: %q", out)
	}

	t.Logf("scale: built %d handoffs in %v · tip walk %v · chain file %d records", N, buildTime, tipTime, len(recs))
}
