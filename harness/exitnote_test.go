package harness

import (
	"context"
	"strings"
	"testing"
)

// The classic flail: grep answers "no match" with exit 1 and the model used to
// read it as breakage. Breaker: drop the grep row from the table and the note
// vanishes.
func TestExitNoteGrepping(t *testing.T) {
	out, isErr := boundedBash(context.Background(), "grep -q zzz /dev/null")
	if !isErr {
		t.Fatal("a benign exit is still an error result; the note must not lie about status")
	}
	if !strings.Contains(out, "exit status 1") {
		t.Fatalf("the real exit line must stay: %q", out)
	}
	if !strings.Contains(out, "grep exits 1 when no lines match") {
		t.Fatalf("the benign-exit note must teach the semantics: %q", out)
	}
}

// Exit 2 from grep is a genuine failure (bad argument); annotating it would
// teach the model to ignore real breakage. Breaker: annotate every nonzero
// code and this fails.
func TestExitNoteRealFailuresStayBare(t *testing.T) {
	out, isErr := boundedBash(context.Background(), "grep --definitely-bad-flag /dev/null")
	if !isErr {
		t.Fatal("exit 2 must remain an error result")
	}
	if strings.Contains(out, "not a command failure") {
		t.Fatalf("a genuine failure must not carry a benign note: %q", out)
	}
	if !strings.Contains(out, "exit status 2") {
		t.Fatalf("the real exit line must stay: %q", out)
	}
}

// The other ordinary answers, and the paths that reach them.
func TestExitNoteFamilies(t *testing.T) {
	cases := []struct{ cmd, want string }{
		{"diff a b", "diff exits 1 when the inputs differ"},
		{"cmp a b", "cmp exits 1 when the files differ"},
		{"test -e /nope", "test exits 1 when the condition is false"},
		{"[ -e /nope ]", "test exits 1 when the condition is false"},
		{"cat x | grep needle", "grep exits 1 when no lines match"}, // pipe: last program rules
		{"FOO=1 grep -q x /dev/null", "grep exits 1 when no lines match"},
		{"/usr/bin/grep -q x /dev/null", "grep exits 1 when no lines match"},
		{"true", ""}, // exit 0: no error path, no note
	}
	for _, c := range cases {
		got, _ := exitNote(c.cmd, 1)
		if c.want == "" {
			continue
		}
		if !strings.Contains(got, "exits 1") || !strings.Contains(got, strings.Fields(c.want)[0]) {
			t.Errorf("exitNote(%q) = %q, want it to name %q", c.cmd, got, strings.Fields(c.want)[0])
		}
	}
	// exit 0 never produces a note even for a listed program
	if got, ok := exitNote("grep -q x f", 0); ok {
		t.Errorf("exit 0 must not be annotated: %q", got)
	}
}

// The proc-manager path (top-level sessions) must teach the same semantics.
func TestExitNoteThroughProcManager(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	m := newProcManager("scale-exitnote")
	t.Cleanup(m.reapAll)
	out, isErr := m.doBash(context.Background(), "grep -q zzz /dev/null")
	if !isErr || !strings.Contains(out, "not a command failure") {
		t.Fatalf("the supervisor path must annotate too: %q err=%v", out, isErr)
	}
}
