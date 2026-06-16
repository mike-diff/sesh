package harness

import (
	"testing"

	"github.com/mike-diff/sesh/agent"
)

// TestLastAssistantText: /copy reaches back past trailing tool/user turns and
// past a tool-call-only assistant turn (which has no text) to the last response
// the user actually saw. Breaker: walk the history forward, or drop the
// non-empty check, and the wrong turn (or none) is copied.
func TestLastAssistantText(t *testing.T) {
	r := &repl{history: []agent.Turn{
		{Role: "user", Text: "first"},
		{Role: "assistant", Text: "older answer"},
		{Role: "user", Text: "second"},
		{Role: "assistant", Text: "newer answer"},
		{Role: "assistant", Text: ""}, // a tool-call-only turn carries no text
		{Role: "tool"},
	}}
	if got := r.lastAssistantText(); got != "newer answer" {
		t.Fatalf("lastAssistantText = %q, want %q", got, "newer answer")
	}
	if got := (&repl{}).lastAssistantText(); got != "" {
		t.Fatalf("empty history must yield no text, got %q", got)
	}
}

// TestOSC52 pins the clipboard escape sequence to the wire format terminals
// expect (a real external contract): ESC ] 52 ; c ; <base64> BEL. Breaker:
// wrong selection char, wrong terminator, or unencoded payload.
func TestOSC52(t *testing.T) {
	if got := osc52("hi"); got != "\033]52;c;aGk=\007" {
		t.Fatalf("osc52 = %q, want ESC]52;c;aGk=BEL", got)
	}
}
