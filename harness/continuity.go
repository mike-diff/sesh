// Continuity: the infinite-session machinery. When a session's context window
// nears its limit, the harness does not compact in place (re-summarizing a
// summary loses most of everything within two or three generations); it hands
// off to a fresh session in the same chain. The new session is seeded with
// four layers, none of which is a summary of a summary:
//
//  1. standing context, re-read fresh (system prompt, AGENTS.md): zero decay
//  2. the chain ledger, carried forward verbatim and appended to, never
//     recompressed
//  3. a handoff brief written by a fresh-context model call that reads the
//     dying session's transcript (full information, fresh attention)
//  4. the most recent exchanges verbatim
//
// What the brief drops is still not lost: prior sessions are archived in full
// and the recall tool searches the whole chain. The handoff is silent because
// it is recoverable, not because the summary is trusted.
package harness

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"sesh/agent"
)

// approxTokens is the usual chars/4 estimate: good enough for budgeting the
// verbatim tail and reporting, never for billing.
func approxTokens(turns []agent.Turn) int {
	n := 0
	for _, t := range turns {
		n += len(t.Text)
		for _, r := range t.Results {
			n += len(r.Content)
		}
	}
	return n / 4
}

// ---------------------------------------------------------------------------
// The handoff brief: written by a fresh context, not by the dying one.
// ---------------------------------------------------------------------------

// renderTranscript flattens a history for the brief writer: conversation text
// in full (the signal), tool calls as one-liners and tool results elided to a
// stub (the bulk). A transcript over maxBriefTranscript keeps its head (where
// the task and decisions were established) and tail, eliding the middle.
const maxBriefTranscript = 240_000 // chars, ~60k tokens

func renderTranscript(turns []agent.Turn, maxResult int) string {
	var b strings.Builder
	for _, t := range turns {
		switch t.Role {
		case "user":
			fmt.Fprintf(&b, "USER: %s\n\n", t.Text)
		case "assistant":
			if t.Text != "" {
				fmt.Fprintf(&b, "ASSISTANT: %s\n", t.Text)
			}
			for _, c := range t.Calls {
				fmt.Fprintf(&b, "ASSISTANT ran %s %s\n", c.Name, compact(string(c.Args)))
			}
			b.WriteByte('\n')
		case "tool":
			for _, res := range t.Results {
				out := res.Content
				if len(out) > maxResult {
					out = out[:maxResult] + "..."
				}
				fmt.Fprintf(&b, "TOOL RESULT: %s\n", strings.ReplaceAll(out, "\n", " · "))
			}
			b.WriteByte('\n')
		}
	}
	s := b.String()
	if len(s) > maxBriefTranscript {
		head, tail := maxBriefTranscript/3, maxBriefTranscript*2/3
		s = s[:head] + "\n[... middle of the transcript omitted ...]\n" + s[len(s)-tail:]
	}
	return s
}

// briefInstructions is structured role-first. Negative constraints and failed
// approaches get their own numbered section because they are what default
// summaries reliably drop, and re-walking a known dead end is the most
// expensive handoff failure.
const briefInstructions = `<role>
You write handoff briefs. A coding session's context window is nearly full; a
fresh session will continue the work knowing only standing project files, what
you write here, and the last few exchanges verbatim.
</role>

<instructions>
From the transcript below, extract exactly what the continuing agent needs:
1. Task: the user's goal, quoting their key requests verbatim.
2. Decisions: each with its rationale and any rejected alternative.
3. Forbidden and failed: everything established as off-limits or already tried
   and abandoned, with the reason. Never drop one of these.
4. Files that matter: path, plus one line on why.
5. Environment: build, test, and run commands exactly as used.
6. State and next step: what is done, what is in flight, the exact next action.
7. Open questions.
Do not invent anything that is not in the transcript. Omit pleasantries.
If the transcript begins with an earlier handoff brief, its items are
established facts that this conversation inherited: merge every one of them
forward verbatim into the matching section (dedupe, but never drop an item
unless the transcript shows it resolved or superseded). Facts that live only
in a brief decay silently across handoffs otherwise.
</instructions>

<output>
Compact markdown under those numbered headings. Then one final line starting
exactly with "LEDGER: " followed by two to four sentences: what this session
accomplished and its single most important decision, with the reasoning.
</output>`

// writeBrief runs the brief writer as a fresh-context model call (no tools, no
// inherited confusion) and splits off the ledger entry. The usage comes back
// so handoff cost is visible in tokens: the honest unit; dollars vary by
// provider, tokens do not.
func writeBrief(ctx context.Context, p agent.Provider, transcript string) (brief, ledgerEntry string, used agent.Usage, err error) {
	prompt := steerPrompt("brief", briefInstructions) + "\n\n<transcript>\n" + transcript + "\n</transcript>"
	out, used, err := agent.Run(ctx, p, "You write precise handoff briefs for coding agents.",
		[]agent.Turn{{Role: "user", Text: prompt}}, nil, agent.Hooks{})
	if err != nil {
		return "", "", used, err
	}
	got := strings.TrimSpace(lastText(out))
	if got == "" {
		return "", "", used, errors.New("the model returned an empty brief")
	}
	brief, ledgerEntry = splitLedger(got)
	return brief, ledgerEntry, used, nil
}

// splitLedger separates the brief from its final LEDGER: line. A brief without
// one still yields a usable (if thin) entry: its first line.
func splitLedger(brief string) (string, string) {
	if i := strings.LastIndex(brief, "LEDGER:"); i >= 0 {
		if entry := strings.TrimSpace(brief[i+len("LEDGER:"):]); entry != "" {
			return strings.TrimSpace(brief[:i]), entry
		}
	}
	return brief, firstLine(brief)
}

// ---------------------------------------------------------------------------
// Mechanical facts: generated by the shell, never summarized by the model.
// ---------------------------------------------------------------------------

func mechanicalFacts() string {
	run := func(args ...string) string {
		out, err := exec.Command("git", args...).Output()
		if err != nil {
			return ""
		}
		return strings.TrimSpace(string(out))
	}
	branch := run("rev-parse", "--abbrev-ref", "HEAD")
	if branch == "" {
		return "(not a git repository)"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "branch: %s\n", branch)
	if status := run("status", "--short"); status != "" {
		if lines := strings.Split(status, "\n"); len(lines) > 30 {
			status = strings.Join(lines[:30], "\n") + fmt.Sprintf("\n... and %d more", len(lines)-30)
		}
		fmt.Fprintf(&b, "working tree:\n%s\n", status)
	} else {
		b.WriteString("working tree: clean\n")
	}
	if log := run("log", "--oneline", "-5"); log != "" {
		fmt.Fprintf(&b, "recent commits:\n%s", log)
	}
	return b.String()
}

// ---------------------------------------------------------------------------
// Seeding the next link of the chain.
// ---------------------------------------------------------------------------

// verbatimTail returns the longest suffix of turns that starts at a user turn
// and fits the token budget: the part of the conversation every production
// harness independently chose to protect from summarization.
func verbatimTail(turns []agent.Turn, budgetTokens int) []agent.Turn {
	best := -1
	for i := len(turns) - 1; i >= 0; i-- {
		if turns[i].Role != "user" {
			continue
		}
		if approxTokens(turns[i:]) > budgetTokens {
			break
		}
		best = i
	}
	if best < 0 {
		return nil
	}
	return slices.Clone(turns[best:])
}

// ---------------------------------------------------------------------------
// The chain ledger file: one append-only JSONL per chain. Append is O(1) and
// crash-safe (a torn final line drops one record, not the file); a chain with
// thousands of handoffs stays cheap because sessions carry only the most
// recent entries and the file carries them all. pi, Claude Code, and Codex
// all converged on JSONL for exactly this property.
// ---------------------------------------------------------------------------

// chainRecord is one handoff, as appended to the chain file.
type chainRecord struct {
	Time      time.Time `json:"time"`
	From      string    `json:"from"`
	To        string    `json:"to"`
	Entry     string    `json:"entry"`                // the ledger entry the brief writer produced
	CtxTokens int       `json:"ctx_tokens,omitempty"` // context size that triggered the handoff
	BriefIn   int       `json:"brief_in,omitempty"`   // what writing the brief cost, in tokens
	BriefOut  int       `json:"brief_out,omitempty"`
	CachedIn  int       `json:"cached_in,omitempty"` // cached fraction of the brief call's prompt, when the provider reports one
}

func chainsDir() string { return filepath.Join(os.Getenv("HOME"), ".sesh", "chains") }

func chainPath(root string) string { return filepath.Join(chainsDir(), root+".jsonl") }

// appendChainRecord writes one handoff record. Appended BEFORE the parent is
// sealed, so the chain file is the source of truth a recovery can replay.
func appendChainRecord(root string, rec chainRecord) error {
	if err := os.MkdirAll(chainsDir(), 0o755); err != nil {
		return err
	}
	b, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(chainPath(root), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(append(b, '\n'))
	return err
}

// readChain returns a chain's records, oldest first. Malformed lines (a torn
// tail from a crash) are skipped, never fatal.
func readChain(root string) []chainRecord {
	b, err := os.ReadFile(chainPath(root))
	if err != nil {
		return nil
	}
	var out []chainRecord
	for _, line := range strings.Split(string(b), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var rec chainRecord
		if json.Unmarshal([]byte(line), &rec) == nil {
			out = append(out, rec)
		}
	}
	return out
}

const seedTemplate = `This conversation continues session {{parent}}; its context window filled and
the work was handed off. Standing project context (system prompt, AGENTS.md)
is loaded fresh. Earlier sessions are archived in full. When you need exact
wording, paths, errors, or decisions that are not below, you MUST search them
with the recall tool before answering that you do not know.

<chain_ledger>
{{ledger}}
</chain_ledger>

<handoff_brief>
{{brief}}
</handoff_brief>

<repo_state>
{{repo}}
</repo_state>

The most recent exchanges follow verbatim; continue from where they leave off.`

// seedChain builds the next session in the chain and its opening history: the
// handoff context as a sealed user/assistant pair (so the next real user
// message keeps roles alternating), then the verbatim tail. Only the most
// recent ledger entries ride in the seed (numbered absolutely); the chain
// file keeps them all, so deep chains stay constant-cost.
func seedChain(old *Session, brief, ledgerEntry, mech string, tail []agent.Turn) *Session {
	root := old.Root
	if root == "" {
		root = old.ID
	}
	hops := old.Hops
	if hops < len(old.Ledger) { // sessions from before Hops existed undercount
		hops = len(old.Ledger)
	}
	hops++
	ledger := append(slices.Clone(old.Ledger), ledgerEntry)
	if len(ledger) > tune.SeedLedgerEntries {
		ledger = ledger[len(ledger)-tune.SeedLedgerEntries:]
	}
	first := hops - len(ledger) + 1 // absolute number of the first shown entry
	var numbered []string
	for i, e := range ledger {
		numbered = append(numbered, fmt.Sprintf("%d. %s", first+i, e))
	}
	shown := strings.Join(numbered, "\n")
	if first > 1 {
		shown = fmt.Sprintf("(%d earlier entries are in the chain ledger; recall searches their sessions)\n%s", first-1, shown)
	}
	seed := render(steerPrompt("seed", seedTemplate), map[string]string{
		"parent": old.ID, "ledger": shown, "brief": brief, "repo": mech,
	})
	turns := []agent.Turn{
		{Role: "user", Text: seed},
		{Role: "assistant", Text: "Understood. Continuing the work from the handoff."},
	}
	turns = append(turns, tail...)
	return &Session{
		ID:       newSessionID(),
		Title:    "continues " + old.ID,
		Cwd:      old.Cwd,
		Provider: old.Provider,
		Protocol: old.Protocol,
		URL:      old.URL,
		Model:    old.Model,
		Parent:   old.ID,
		Root:     root,
		Hops:     hops,
		Ledger:   ledger,
		Created:  time.Now(),
		Turns:    turns,
	}
}

// ---------------------------------------------------------------------------
// recall: lossless paging over the chain. This is what lets the handoff be
// silent: anything the brief dropped is one tool call away.
// ---------------------------------------------------------------------------

const maxRecallHits = 50

const recallDescription = "Search the archived transcripts of this conversation's session chain (case-insensitive substring match). " +
	"After a context handoff, earlier parts of the conversation are out of your window but archived in full; " +
	"use this to recover exact wording, decisions, paths, or error messages. " +
	"Returns matching lines as session#turn role: text. " +
	"Full transcripts are also plain files readable with the read tool: ~/.sesh/sessions/<id>.json."

func recallTool(getSess func() *Session) agent.Tool {
	return agent.Tool{
		Def: agent.ToolDef{
			Name:        "recall",
			Description: recallDescription,
			Schema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"pattern": map[string]any{"type": "string",
						"description": "The text to find in the chain's transcripts (case-insensitive substring)."},
				},
				"required": []string{"pattern"},
			},
		},
		Run: func(_ context.Context, raw json.RawMessage) (string, bool) {
			var in struct {
				Pattern string `json:"pattern"`
			}
			if err := json.Unmarshal(raw, &in); err != nil || strings.TrimSpace(in.Pattern) == "" {
				return "recall needs a non-empty pattern", true
			}
			sess := getSess()
			if sess == nil {
				return "no session to search", true
			}
			var hits []string
			seen := map[string]bool{}
			s, walked := sess, 0
			for s != nil && !seen[s.ID] && walked < tune.RecallLinks {
				seen[s.ID] = true
				walked++
				hits = append(hits, searchTurns(s, in.Pattern, maxRecallHits-len(hits))...)
				if len(hits) >= maxRecallHits || s.Parent == "" {
					s = nil
					break
				}
				next, err := loadSession(s.Parent)
				if err != nil {
					hits = append(hits, fmt.Sprintf("(chain ends: parent session %s could not be loaded)", s.Parent))
					s = nil
					break
				}
				s = next
			}
			// Deep chains are disclosed, never silently truncated: the model
			// can read older transcripts directly, they are plain files.
			if s != nil && walked >= tune.RecallLinks {
				hits = append(hits, fmt.Sprintf("(searched the %d most recent links; the chain continues deeper. Older transcripts are readable files: ~/.sesh/sessions/<id>.json, parent ids inside)", tune.RecallLinks))
			}
			if len(hits) == 0 {
				return "no matches in the session chain", false
			}
			return strings.Join(hits, "\n"), false
		},
		Parallel: true, // reads archived sessions from disk; mutates nothing
	}
}

// searchTurns scans one session's transcript for the pattern, newest turns
// last, returning at most max matching lines tagged session#turn role.
func searchTurns(s *Session, pattern string, max int) []string {
	if max <= 0 {
		return nil
	}
	low := strings.ToLower(pattern)
	var hits []string
	add := func(turn int, role, text string) bool {
		for _, line := range strings.Split(text, "\n") {
			if !strings.Contains(strings.ToLower(line), low) {
				continue
			}
			line = strings.TrimSpace(line)
			if len(line) > 240 {
				line = line[:240] + "..."
			}
			hits = append(hits, fmt.Sprintf("%s#%d %s: %s", s.ID, turn, role, line))
			if len(hits) >= max {
				return true
			}
		}
		return false
	}
	for i, t := range s.Turns {
		if add(i, t.Role, t.Text) {
			return hits
		}
		for _, r := range t.Results {
			if add(i, "tool", r.Content) {
				return hits
			}
		}
	}
	return hits
}
