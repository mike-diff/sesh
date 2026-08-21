// Benign exit-code notes for shell results. A command like `grep -q needle
// file` exits 1 when nothing matched: that is the command ANSWERING, not
// failing. The bash tools used to return the bare "exit status 1", the model
// read failure, and the classic flail followed: pointlessly re-running the
// command, switching tools, or reporting breakage that never happened.
//
// The note teaches semantics without lying about status: the result stays an
// error result, the note only says what the exit code MEANS for that program.
// Matching is on the command's first word, kept deliberately dumb: enough to
// be honest about the common tools, never enough to be a shell parser.
package harness

import (
	"os/exec"
	"strings"
)

// exitNote returns the meaning of code for the program leading command, and
// whether the code is one of that program's ordinary answers. Only exit 1 is
// annotated: 2 and up are genuine failures everywhere here.
func exitNote(command string, code int) (string, bool) {
	if code != 1 {
		return "", false
	}
	prog := firstWord(command)
	switch prog {
	case "grep", "egrep", "fgrep", "rg":
		return "note: grep exits 1 when no lines match; this is not a command failure", true
	case "diff":
		return "note: diff exits 1 when the inputs differ; this is not a command failure", true
	case "cmp":
		return "note: cmp exits 1 when the files differ; this is not a command failure", true
	case "test", "[", "[[":
		return "note: test exits 1 when the condition is false; this is not a command failure", true
	case "pgrep":
		return "note: pgrep exits 1 when no process matches; this is not a command failure", true
	}
	return "", false
}

// firstWord extracts the program a command line invokes: the first field,
// stripped of any path prefix. Shell syntax before it (env assignments,
// redirects) is ignored; pipes mean the LAST program's exit code is what
// matters, so the final segment is used.
func firstWord(command string) string {
	seg := command
	if i := strings.LastIndexByte(seg, '|'); i >= 0 {
		seg = seg[i+1:]
	}
	for _, f := range strings.Fields(seg) {
		if strings.Contains(f, "=") && !strings.ContainsAny(f, "/.") {
			continue // leading VAR=value assignment, not the program
		}
		if f == "sudo" || f == "env" {
			continue // look through to the wrapped program
		}
		return pathBase(f)
	}
	return ""
}

func pathBase(p string) string {
	if i := strings.LastIndexByte(p, '/'); i >= 0 {
		return p[i+1:]
	}
	return p
}

// annotateExit appends the benign-exit note to a failed command's output. It
// accepts the raw error so callers can pass whatever exec handed them; a
// non-ExitError (signal kill, spawn failure) carries no code and stays bare.
func annotateExit(command, out string, err error) string {
	if err == nil {
		return out
	}
	var ee *exec.ExitError
	if e, ok := err.(*exec.ExitError); ok {
		ee = e
	} else if wrapped, ok := err.(interface{ Unwrap() error }); ok {
		ee, _ = wrapped.Unwrap().(*exec.ExitError)
	}
	if ee == nil {
		return out
	}
	if note, ok := exitNote(command, ee.ExitCode()); ok {
		return out + "\n" + note
	}
	return out
}
