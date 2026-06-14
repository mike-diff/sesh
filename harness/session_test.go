package harness

import (
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/mike-diff/sesh/agent"
)

func TestLatestSessionPerDirectory(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	// save stamps Updated at save time, so save order is recency order
	mk := func(id, cwd string) {
		s := &Session{ID: id, Cwd: cwd, Created: time.Now(),
			Turns: []agent.Turn{{Role: "user", Text: "x"}}}
		if err := s.save(); err != nil {
			t.Fatal(err)
		}
		time.Sleep(5 * time.Millisecond)
	}
	mk("aaa-1", "/proj/a")
	mk("bbb-1", "/proj/b")
	mk("aaa-2", "/proj/a")

	// the latest session for /proj/a is aaa-2, even though bbb-1 is newer
	// than aaa-1 and aaa-2 is newest overall
	got, err := latestSession("/proj/a")
	if err != nil || got.ID != "aaa-2" {
		t.Fatalf("latest for /proj/a: %+v err=%v", got, err)
	}
	got, err = latestSession("/proj/b")
	if err != nil || got.ID != "bbb-1" {
		t.Fatalf("latest for /proj/b: %+v err=%v", got, err)
	}

	// a directory with no sessions errors with guidance, not a wrong session
	if _, err := latestSession("/proj/elsewhere"); err == nil || !strings.Contains(err.Error(), "no sessions for this directory") {
		t.Fatalf("expected per-directory miss error, got %v", err)
	}
}

// TestLastBrain: a fresh session adopts the latest brain for its directory,
// falls back to the latest anywhere, and nil only on a truly empty history.
func TestLastBrain(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	if got := lastBrain("/proj/a"); got != nil {
		t.Fatalf("empty history should be nil, got %+v", got)
	}
	mk := func(id, cwd, provider string) {
		s := &Session{ID: id, Cwd: cwd, Provider: provider, Created: time.Now(),
			Turns: []agent.Turn{{Role: "user", Text: "x"}}}
		if err := s.save(); err != nil {
			t.Fatal(err)
		}
		time.Sleep(5 * time.Millisecond)
	}
	mk("s1", "/proj/a", "local")
	mk("s2", "/proj/b", "remote")

	if got := lastBrain("/proj/a"); got == nil || got.Provider != "local" {
		t.Fatalf("directory match should win: %+v", got)
	}
	if got := lastBrain("/proj/new"); got == nil || got.Provider != "remote" {
		t.Fatalf("no directory match should fall back to latest anywhere: %+v", got)
	}
}

// TestNewSessionIDEntropy pins the id format as the entropy contract: the
// breaker is shrinking the random suffix (reverting to 2 bytes broke a
// 4000-handoff chain via birthday collisions; TestChainScale4000 catches the
// behavior, this catches the knob). A uniqueness loop cannot honestly detect
// that regression at any non-flaky sample size, so there isn't one.
func TestNewSessionIDEntropy(t *testing.T) {
	id := newSessionID()
	if len(id) != len("20060102-150405-abcdef01") {
		t.Fatalf("id %q: the random suffix must stay 4 bytes (8 hex chars)", id)
	}
	if newSessionID() == newSessionID() {
		t.Fatal("back-to-back ids must differ: the suffix must actually be random")
	}
}

// TestSessionLocks: own claim succeeds and re-releases; a live foreign owner
// is a conflict; a dead owner's lock is stale and taken over; release never
// removes a lock it does not own.
func TestSessionLocks(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	if err := acquireLock("lk-1"); err != nil {
		t.Fatalf("fresh acquire: %v", err)
	}
	releaseLock("lk-1")
	if err := acquireLock("lk-1"); err != nil {
		t.Fatalf("reacquire after release: %v", err)
	}
	releaseLock("lk-1")

	// a live foreign owner (the test runner's parent) conflicts
	os.WriteFile(lockPath("lk-2"), []byte(strconv.Itoa(os.Getppid())), 0o644)
	if err := acquireLock("lk-2"); err == nil || !strings.Contains(err.Error(), "another sesh") {
		t.Fatalf("live owner must conflict: %v", err)
	}

	// a dead owner is stale: spawn-and-reap a process for a dead pid
	cmd := exec.Command("true")
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(lockPath("lk-3"), []byte(strconv.Itoa(cmd.Process.Pid)), 0o644)
	if err := acquireLock("lk-3"); err != nil {
		t.Fatalf("stale lock should be taken over: %v", err)
	}

	// release leaves a foreign lock alone
	os.WriteFile(lockPath("lk-4"), []byte(strconv.Itoa(os.Getppid())), 0o644)
	releaseLock("lk-4")
	if _, err := os.Stat(lockPath("lk-4")); err != nil {
		t.Fatal("release must not remove a lock it does not own")
	}
}
