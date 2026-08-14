package harness

import (
	"os"
	"testing"

	"github.com/mike-diff/sesh/agent"
)

// TestGateModResolvesPerCall: a gate mod installed (or fixed) mid-session
// binds at the next mutating call, not after a restart. The gate re-resolves
// the executable per call: a boundary the user drops in place while a
// long unattended run is going is exactly when it must start ruling.
// Breaker: capture the mod path once at gate construction (the old behavior)
// and the second call passes with the deny gate installed.
func TestGateModResolvesPerCall(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	chtmp(t)
	g := gate(&plainConsole{}, false)
	call := agent.ToolCall{Name: "bash", Args: []byte(`{"command":"true"}`)}
	if err := g(call); err != nil {
		t.Fatalf("with no mod installed a mutating call must pass: %v", err)
	}

	os.MkdirAll(".sesh", 0o755)
	os.WriteFile(".sesh/gate", []byte("#!/bin/sh\nexit 1\n"), 0o755)
	if err := g(call); err == nil {
		t.Fatal("a gate mod installed mid-session must deny the next call")
	}
}
