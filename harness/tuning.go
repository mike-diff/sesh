// Tuning: every behavioral assumption the harness makes about what models can
// and cannot do, gathered in one place and overridable from user space.
//
// The doctrine (Anthropic's harness-design lesson, adopted as ours): every
// component in a harness encodes an assumption about what the model cannot do
// on its own, and each model generation invalidates some of them. A dial that
// requires recompiling pins the design; a dial in a file does not. So the
// defaults below are this release's best evidence (most of them measured by
// the retention rig), and ~/.sesh/tuning.json overlaid by
// .sesh/tuning.json adjusts any of them per user, per project, per model
// era, with no code change and no breaking release.
package harness

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

// Tuning holds the dials. Zero values mean "keep the default", so a tuning
// file states only what it changes.
type Tuning struct {
	// HandoffPct is the soft context-pressure threshold (percent of the
	// window) where handoff engages at a clean boundary. Default 80: every
	// production harness converged on a 20-30% reserve.
	HandoffPct int `json:"handoff_pct,omitempty"`
	// HardPct forces the handoff regardless of boundary quality. Default 90.
	HardPct int `json:"hard_pct,omitempty"`
	// MaxUsefulContext caps the effective window: past this size, measured
	// reasoning quality falls regardless of the advertised window. Default
	// 250000. Raise it as models stop rotting; the rig will tell you.
	MaxUsefulContext int `json:"max_useful_context,omitempty"`
	// AssumedContext stands in when nothing declares or discovers a window.
	// Default 200000.
	AssumedContext int `json:"assumed_context,omitempty"`
	// SeedLedgerEntries is how many chain-ledger entries ride in every
	// handoff seed (the full ledger stays in the chain file). Default 10.
	SeedLedgerEntries int `json:"seed_ledger_entries,omitempty"`
	// TaskDepth caps subagent nesting. Default 3.
	TaskDepth int `json:"task_depth,omitempty"`
	// StuckAfter is how many driven iterations without one approved mutation
	// stop a request as stuck. Default 3.
	StuckAfter int `json:"stuck_after,omitempty"`
	// RecallLinks is how many chain links one recall call searches before
	// disclosing that the chain runs deeper. Default 50.
	RecallLinks int `json:"recall_links,omitempty"`
	// DiffLines caps the line diff that edit and write results carry (the
	// renderer styles it; the model sees it as applied-change feedback).
	// Default 40; -1 disables the diff entirely. Zero means keep the default,
	// like every dial.
	DiffLines int `json:"diff_lines,omitempty"`
	// SkillManifestMax caps how many skill manifest lines ride in the skill
	// tool's description; overflow is loud, never silent. Default 50.
	SkillManifestMax int `json:"skill_manifest_max,omitempty"`
	// McpManifestMax caps how many server:tool manifest lines ride in the mcp
	// tool's description; overflow is loud, never silent. Default 100.
	McpManifestMax int `json:"mcp_manifest_max,omitempty"`
	// SkillNoteOff drops the skills system-prompt note (the line telling the
	// model to consult the manifest before a task). Default off, so the note
	// is present: it moves weak local models from 1/3 to 3/3 triggering and
	// is provably harmless on reasoning models, which trigger correctly from
	// the manifest alone (engines rig + dogfood). Set
	// true to reclaim the tokens when you only run strong models. Inverted so
	// the zero value keeps the default, like every dial.
	SkillNoteOff bool `json:"skill_note_off,omitempty"`
	// ProcPromoteSecs is how long a foreground bash command may run before it
	// is promoted to a tracked background process instead of being killed.
	// Default 60: a server or a slow build outlives this and keeps running with
	// a handle, rather than dying at the timeout and losing its output.
	ProcPromoteSecs int `json:"proc_promote_secs,omitempty"`
	// MaxProcs caps how many background processes one session may own at once;
	// past it, proc start refuses (loud, never silent). Default 10.
	MaxProcs int `json:"max_procs,omitempty"`
	// ProcLogTail is the default number of recent lines a proc logs read
	// returns when no explicit tail is given. Default 200.
	ProcLogTail int `json:"proc_log_tail,omitempty"`
	// ProcSpillOff drops the on-disk log file for background processes (the
	// in-memory ring still serves reads). Default off, so logs also spill to
	// ~/.sesh/run/<session>/<id>.log for inspection. Inverted so the zero
	// value keeps the default, like every dial.
	ProcSpillOff bool `json:"proc_spill_off,omitempty"`
	// BriefProvider and BriefModel aim handoff-brief writing at a different
	// brain than the worker, typically a cheap local model writing briefs for
	// an expensive one. Empty (the default) means the worker writes its own.
	// BriefModel alone swaps the model on the worker's endpoint; BriefProvider
	// selects another configured profile, with BriefModel overriding its
	// default model. Evidence the swap is safe: the
	// cheap-brief experiment.
	BriefProvider string `json:"brief_provider,omitempty"`
	BriefModel    string `json:"brief_model,omitempty"`
	// UpdateCheck, when on, has interactive startup ask the latest release
	// whether a newer build exists and, if so, print a one-line nudge to run
	// /update. Default off: it is a network call to GitHub on every launch, so
	// it is opt-in rather than phoning home for everyone.
	UpdateCheck bool `json:"update_check,omitempty"`
}

func defaultTuning() Tuning {
	return Tuning{
		HandoffPct:        80,
		HardPct:           90,
		MaxUsefulContext:  250_000,
		AssumedContext:    200_000,
		SeedLedgerEntries: 10,
		TaskDepth:         3,
		StuckAfter:        3,
		RecallLinks:       50,
		DiffLines:         40,
		SkillManifestMax:  50,
		McpManifestMax:    100,
		ProcPromoteSecs:   60,
		MaxProcs:          10,
		ProcLogTail:       200,
	}
}

// tune is the live dial set, resolved once at startup. Tests adjust fields
// directly and restore them.
var tune = defaultTuning()

// loadTuning resolves defaults, then the global file, then the project file,
// each layer overriding only the fields it states. A missing or unparseable
// file is skipped: tuning is purely additive, like every mod.
func loadTuning() Tuning {
	t := defaultTuning()
	for _, p := range []string{
		filepath.Join(os.Getenv("HOME"), ".sesh", "tuning.json"),
		".sesh/tuning.json",
	} {
		b, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		var got Tuning
		if json.Unmarshal(stripJSONComments(b), &got) != nil {
			continue
		}
		overlayTuning(&t, got)
	}
	return t
}

// stripJSONComments removes // line and /* */ block comments so tuning.json can
// carry the reasoning behind each dial inline (see the shipped
// tuning.json.example). String literals are left untouched, so a // inside a
// value (a URL in brief_model, say) survives. Scoped to tuning.json on purpose:
// it is the one config the harness reads but never rewrites, so the comments
// are never silently dropped on a save the way they would be in providers.json.
func stripJSONComments(b []byte) []byte {
	out := make([]byte, 0, len(b))
	inString := false
	for i := 0; i < len(b); i++ {
		c := b[i]
		if inString {
			out = append(out, c)
			if c == '\\' && i+1 < len(b) { // escaped char: copy it verbatim
				i++
				out = append(out, b[i])
			} else if c == '"' {
				inString = false
			}
			continue
		}
		switch {
		case c == '"':
			inString = true
			out = append(out, c)
		case c == '/' && i+1 < len(b) && b[i+1] == '/':
			for i < len(b) && b[i] != '\n' {
				i++
			}
			if i < len(b) {
				out = append(out, b[i]) // keep the newline for readable errors
			}
		case c == '/' && i+1 < len(b) && b[i+1] == '*':
			i += 2
			for i+1 < len(b) && !(b[i] == '*' && b[i+1] == '/') {
				i++
			}
			i++ // step over the closing '*'; the loop's i++ steps over '/'
		default:
			out = append(out, c)
		}
	}
	return out
}

func overlayTuning(t *Tuning, got Tuning) {
	set := func(dst *int, v int) {
		if v > 0 {
			*dst = v
		}
	}
	set(&t.HandoffPct, got.HandoffPct)
	set(&t.HardPct, got.HardPct)
	set(&t.MaxUsefulContext, got.MaxUsefulContext)
	set(&t.AssumedContext, got.AssumedContext)
	set(&t.SeedLedgerEntries, got.SeedLedgerEntries)
	set(&t.TaskDepth, got.TaskDepth)
	set(&t.StuckAfter, got.StuckAfter)
	set(&t.RecallLinks, got.RecallLinks)
	set(&t.SkillManifestMax, got.SkillManifestMax)
	set(&t.McpManifestMax, got.McpManifestMax)
	set(&t.ProcPromoteSecs, got.ProcPromoteSecs)
	set(&t.MaxProcs, got.MaxProcs)
	set(&t.ProcLogTail, got.ProcLogTail)
	// DiffLines accepts -1 (disable), so its overlay applies on any nonzero.
	if got.DiffLines != 0 {
		t.DiffLines = got.DiffLines
	}
	// Booleans overlay on true: a file states the flag only to turn it on.
	if got.SkillNoteOff {
		t.SkillNoteOff = true
	}
	if got.ProcSpillOff {
		t.ProcSpillOff = true
	}
	if got.UpdateCheck {
		t.UpdateCheck = true
	}
	sets := func(dst *string, v string) {
		if v != "" {
			*dst = v
		}
	}
	sets(&t.BriefProvider, got.BriefProvider)
	sets(&t.BriefModel, got.BriefModel)
}

// steerPrompt resolves a model-facing prompt template from user space:
// .sesh/prompts/<name>.md, then ~/.sesh/prompts/<name>.md, then the
// built-in. Prompts are the deepest model-capability assumptions of all, and
// the most common thing a new model generation wants changed, so they follow
// the same mod chain as SYSTEM.md. Placeholders like {{cwd}} are documented
// per template in ~/.sesh/prompts/README.md.
func steerPrompt(name, builtin string) string {
	home := os.Getenv("HOME")
	for _, p := range []string{
		filepath.Join(".sesh", "prompts", name+".md"),
		filepath.Join(home, ".sesh", "prompts", name+".md"),
	} {
		if b, err := os.ReadFile(p); err == nil && len(b) > 0 {
			return string(b)
		}
	}
	return builtin
}

// render substitutes {{name}} placeholders. Deliberately dumb: no escapes, no
// conditionals; a template language would be policy nobody can read.
func render(template string, vars map[string]string) string {
	out := template
	for k, v := range vars {
		out = strings.ReplaceAll(out, "{{"+k+"}}", v)
	}
	return out
}
