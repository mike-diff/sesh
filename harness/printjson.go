// The -json contract for print mode: one line of JSON on stdout describing the
// whole run, emitted in every outcome including failure, so a pipe stays
// parseable no matter how the run ended. The text mode's contract (bare reply
// on stdout, progress on stderr, exit code for the drive outcome) is
// unchanged; this is its machine-readable twin, the "third set of hooks" the
// architecture notes anticipated.
package harness

import (
	"encoding/json"
	"fmt"
	"os"
)

// printEnvelope is the stable -json output shape. Flat on purpose: it is a
// contract for scripts, not a dump. Error is empty on success; on failure the
// reply is empty and Error carries the reason, so a consumer needs one parse
// and one field check, never stderr scraping.
type printEnvelope struct {
	Reply      string `json:"reply"`
	ExitCode   int    `json:"exit_code"`
	Outcome    string `json:"outcome"`
	Provider   string `json:"provider,omitempty"`
	Model      string `json:"model,omitempty"`
	Session    string `json:"session,omitempty"`
	Iterations int    `json:"iterations"`
	ToolCalls  int    `json:"tool_calls"`
	Mutations  int    `json:"mutations"`
	Usage      struct {
		Input     int `json:"input"`
		Output    int `json:"output"`
		CacheRead int `json:"cache_read"`
	} `json:"usage"`
	Error string `json:"error,omitempty"`
}

// outcomeName maps a drive outcome constant to the envelope's name. Blocked
// shares exit 0 with done (the user got their answer either way), so the name
// is the only way to tell them apart; that is why the field exists.
func outcomeName(code int) string {
	switch code {
	case driveDone:
		return "done"
	case driveBlocked:
		return "blocked"
	case driveStuck:
		return "stuck"
	case driveMaxIters:
		return "max-iterations"
	case driveInterrupted:
		return "interrupted"
	default:
		return "error"
	}
}

// emitPrintJSON writes the envelope as one line to stdout and exits with code.
// It is the -json mode's ONLY stdout write, and it owns the exit so no caller
// can accidentally print after it.
func emitPrintJSON(e printEnvelope, code int) {
	b, err := json.Marshal(e)
	if err != nil {
		// Marshal of this shape cannot fail; if it ever does, say so in the
		// only channel left rather than dying silently.
		fmt.Fprintf(os.Stderr, "internal: envelope marshal failed: %v\n", err)
		os.Exit(1)
	}
	fmt.Println(string(b))
	os.Exit(code)
}
