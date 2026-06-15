// Goal-driven persistence: the harness's default turn lifecycle, not a mode.
// The goal is the user's request; nothing arms it: no file, no command, no
// flag. After any turn that did real work (used tools), a fresh-context judge
// reads the request and the transcript and rules done, continue, or blocked;
// continue feeds the judge's reason into the next iteration and the harness
// keeps working. Turns without tool use are conversation: never judged, never
// looped, never billed for a verdict.
//
// Stop layers, all of them, because each alone fails (the unanimous field
// lesson): the judge, never the worker, decides from transcript evidence;
// -max-iters bounds every request; a no-progress detector stops iterations
// that mutate nothing; Ctrl-C always pauses to the prompt. Mutation approval
// is untouched: gates in interactive, -yes in print mode.
package harness

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/mike-diff/sesh/agent"
)

// drive outcomes. Print mode maps them to exit codes; interactive maps them
// to transcript notices.
const (
	driveDone        = 0
	driveBlocked     = 1
	driveStuck       = 3
	driveMaxIters    = 4
	driveInterrupted = 5
)

// workedOn reports whether these turns contain tool activity: the line
// between a request that needs driving and conversation that does not.
func workedOn(turns []agent.Turn) bool {
	for _, t := range turns {
		if t.Role == "tool" {
			return true
		}
	}
	return false
}

const continueTemplate = `You are continuing iteration {{iteration}} of the same request. A reviewer
judged the work not finished yet:

<reviewer_verdict>
{{verdict}}
</reviewer_verdict>

The original request follows. Address the verdict, verify your work with bash,
and finish what you can this iteration.

<request>
{{request}}
</request>`

// ---------------------------------------------------------------------------
// The judge: a fresh-context call that is never the worker. Done only on
// transcript evidence; the worker's own claim of impossibility or completion
// is evidence, not proof; blocked hands the prompt back to the user.
// ---------------------------------------------------------------------------

type verdict struct {
	Done    bool   `json:"done"`
	Blocked bool   `json:"blocked"`
	Reason  string `json:"reason"`
}

const judgeInstructions = `<role>
You judge whether a coding agent has finished the user's request. You are not
the worker; you only weigh evidence.
</role>

<instructions>
Read the request and the transcript of the latest work. Judge ONLY from
evidence in the transcript:
- done=true only when the transcript contains clear evidence the request is
  satisfied (builds run, tests pass, the asked-for output exists); quote that
  evidence in reason. The worker merely claiming success is evidence, not
  proof.
- blocked=true when the worker genuinely needs something only the user can
  give: a decision between real alternatives, missing information, an
  approval that was declined. Asking out of politeness is not blocked.
- Otherwise done=false, blocked=false, and reason states the single most
  useful next action, starting "insufficient evidence:" when verification is
  what is missing.
</instructions>

<output>
Reply with ONLY a JSON object: {"done": bool, "blocked": bool, "reason": "..."}
</output>`

// judgeGoal returns the verdict and the judge call's own token usage, so the
// driver can count it: the judge runs every iteration and is real spend the
// status line would otherwise miss.
func judgeGoal(ctx context.Context, p agent.Provider, request, transcript string) (verdict, agent.Usage, error) {
	prompt := steerPrompt("judge", judgeInstructions) + "\n\n<request>\n" + request + "\n</request>\n\n<transcript>\n" + transcript + "\n</transcript>"
	out, used, err := agent.Run(ctx, p, "You judge request completion from transcript evidence.",
		[]agent.Turn{{Role: "user", Text: prompt}}, nil, agent.Hooks{})
	if err != nil {
		return verdict{}, used, err
	}
	v, err := parseVerdict(lastText(out))
	return v, used, err
}

// parseVerdict digs the JSON object out of a reply that may wrap it in fences
// or prose; lenient because weak local models are still valid judges.
func parseVerdict(s string) (verdict, error) {
	start := strings.IndexByte(s, '{')
	end := strings.LastIndexByte(s, '}')
	if start < 0 || end <= start {
		return verdict{}, fmt.Errorf("no JSON object in the judge's reply: %.120s", s)
	}
	var v verdict
	if err := json.Unmarshal([]byte(s[start:end+1]), &v); err != nil {
		return verdict{}, fmt.Errorf("judge reply unparseable: %v", err)
	}
	return v, nil
}

// driveConfig is everything the driver needs beyond the live repl.
type driveConfig struct {
	request  string // the user's message: the goal, verbatim
	maxIters int
	maxTools int // per-iteration tool budget (0 = unlimited); fresh each iteration
	tools    []agent.Tool
	hooks    agent.Hooks
	// mutations reports how many mutating tool calls have been approved so
	// far; the no-progress detector reads it across iterations.
	mutations func() int
	// say renders one progress line per iteration; nil means stdout.
	// Interactive routes it into the transcript.
	say func(format string, a ...any)
	// turnCtx supplies each iteration's context; nil means Background. The
	// interactive REPL passes its interrupt watcher's, so Ctrl-C pauses the
	// drive instead of being ignored.
	turnCtx func() (context.Context, func())
}

// drive continues an already-run first turn until the judge rules done or
// blocked, or a stop layer fires. firstTurns is the first turn's slice of
// history (already appended and saved by the caller); iterations after it
// are run here through the same lifecycle the caller used.
func drive(r *repl, cfg driveConfig, firstTurns []agent.Turn) int {
	say := cfg.say
	if say == nil {
		say = func(format string, a ...any) { fmt.Printf(format+"\n", a...) }
	}
	turnCtx := cfg.turnCtx
	if turnCtx == nil {
		turnCtx = func() (context.Context, func()) { return context.Background(), func() {} }
	}
	if cfg.maxIters <= 1 {
		return driveDone // persistence dialed off: single-turn contract
	}
	if !workedOn(firstTurns) {
		return driveDone // conversation, not work: nothing to drive
	}

	iterTurns := firstTurns
	stuck := 0
	for iter := 1; ; iter++ {
		// The judge runs under the same cancellable context as the worker, so
		// Ctrl-C pauses the drive during the judge phase too (not just during a
		// streamed worker iteration).
		jctx, jdone := turnCtx()
		v, jUsed, jerr := judgeGoal(jctx, r.p, cfg.request, renderTranscript(iterTurns, 300))
		jdone()
		r.accountAux(jUsed) // the judge is real spend; count it, leave the gauge
		switch {
		case jerr != nil:
			if isCanceled(jerr) {
				say("== paused; your next message steers")
				return driveInterrupted
			}
			// No verdict means no mandate to keep spending: stop quietly.
			say("== judge unavailable (%s); returning to you", compact(jerr.Error()))
			return driveBlocked
		case v.Done:
			if iter > 1 {
				say("== done after %d iterations: %s", iter, compact(v.Reason))
			}
			return driveDone
		case v.Blocked:
			say("== needs you: %s", compact(v.Reason))
			return driveBlocked
		}

		if iter >= cfg.maxIters {
			say("== max iterations (%d) reached; latest verdict: %s", cfg.maxIters, compact(v.Reason))
			return driveMaxIters
		}

		start := time.Now()
		mutBefore := cfg.mutations()
		mark := len(r.history)
		opening := render(steerPrompt("continue", continueTemplate), map[string]string{
			"iteration": fmt.Sprint(iter + 1), "verdict": v.Reason, "request": cfg.request,
		})
		if r.preflight(opening) {
			return driveBlocked
		}
		r.history = append(r.history, agent.Turn{Role: "user", Text: opening})
		iterHooks := cfg.hooks
		if cfg.maxTools > 0 { // the budget is per iteration, fresh attention each time
			iterHooks.Gate = budgetGate(cfg.maxTools, cfg.hooks.Gate)
		}
		ctx, done := turnCtx()
		out, spent, err := agent.Run(ctx, r.p, r.system, r.history, cfg.tools, iterHooks)
		done()
		if r.md.flush() { // close the iteration's trailing line before its summary
			emit("\n")
		}
		if err != nil {
			// Roll back the unconsumed opening (consecutive user turns would
			// poison the next call).
			r.history = r.history[:mark]
			r.sess.Turns = r.history
			r.sess.save()
			if isCanceled(err) {
				say("== paused; your next message steers")
				return driveInterrupted
			}
			say("== iteration %d error: %v", iter+1, err)
			stuck++
			if stuck >= tune.StuckAfter {
				return driveStuck
			}
			continue
		}
		r.history = out
		r.sess.Turns = r.history
		r.sess.save()
		r.account(spent) // fold this iteration into the status totals + gauge
		r.pushStatus()
		r.managePressure() // the window boundary stays the chain's

		iterTurns = out[mark:]
		if cfg.mutations() == mutBefore {
			stuck++
		} else {
			stuck = 0
		}
		say("== iteration %d · %s · %d in / %d out tokens",
			iter+1, time.Since(start).Round(time.Second), spent.Input, spent.Output)
		if stuck >= tune.StuckAfter {
			say("== stopping: %d iterations without progress; latest verdict: %s", stuck, compact(v.Reason))
			return driveStuck
		}
	}
}
