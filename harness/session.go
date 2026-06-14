// Session management: a session is nothing but the saved Turn history plus a
// little metadata. Because Turn is the harness's own neutral format, a session
// recorded against one provider can be resumed against another.
package harness

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"sesh/agent"
)

type Session struct {
	ID       string       `json:"id"`
	Title    string       `json:"title"`
	Cwd      string       `json:"cwd,omitempty"`      // where the work happened; -continue matches on it
	Provider string       `json:"provider,omitempty"` // profile name, so the credential follows a resume
	Protocol string       `json:"protocol"`
	URL      string       `json:"url"`
	Model    string       `json:"model"`
	Parent   string       `json:"parent,omitempty"`       // session this one continued from (context handoff)
	Child    string       `json:"continued_by,omitempty"` // set when sealed by a handoff; recall reads sealed transcripts, so they must never grow again
	Root     string       `json:"chain,omitempty"`        // first session of this chain; names the chain ledger file
	Hops     int          `json:"hops,omitempty"`         // how many handoffs precede this link
	Ledger   []string     `json:"ledger,omitempty"`       // the most recent chain-ledger entries (the full ledger lives in the chain file)
	Created  time.Time    `json:"created"`
	Updated  time.Time    `json:"updated"`
	Turns    []agent.Turn `json:"turns"`
}

// newSessionID is a timestamp plus four random bytes: readable, sortable, and
// collision-free even when a burst creates thousands of sessions within one
// second (the 4000-handoff scale test broke a chain on two random bytes; an
// id collision silently overwrites another session, so entropy is cheap
// insurance).
func newSessionID() string {
	var b [4]byte
	rand.Read(b[:])
	return time.Now().Format("20060102-150405") + "-" + hex.EncodeToString(b[:])
}

func sessionsDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ".sesh-sessions"
	}
	return filepath.Join(home, ".sesh", "sessions")
}

func (s *Session) path() string { return filepath.Join(sessionsDir(), s.ID+".json") }

func (s *Session) save() error {
	if err := os.MkdirAll(sessionsDir(), 0o755); err != nil {
		return err
	}
	s.Updated = time.Now()
	if s.Title == "" {
		for _, t := range s.Turns {
			if t.Role == "user" && t.Text != "" {
				s.Title = compact(firstLine(t.Text))
				break
			}
		}
	}
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	// Write-then-rename so a crash mid-save cannot corrupt the session file.
	tmp := s.path() + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, s.path())
}

func loadSession(id string) (*Session, error) {
	b, err := os.ReadFile(filepath.Join(sessionsDir(), id+".json"))
	if err != nil {
		return nil, err
	}
	var s Session
	if err := json.Unmarshal(b, &s); err != nil {
		return nil, err
	}
	return &s, nil
}

// forkSession loads a session and returns a copy under a fresh id, so the
// original transcript stays intact while the conversation branches. This is
// the essence of pi's session tree, without the in-place navigation UI.
func forkSession(id string) (*Session, error) {
	src, err := loadSession(id)
	if err != nil {
		return nil, err
	}
	turns := make([]agent.Turn, len(src.Turns))
	copy(turns, src.Turns)
	cwd, _ := os.Getwd()
	ledger := make([]string, len(src.Ledger))
	copy(ledger, src.Ledger)
	// The fork keeps its ancestry (Parent and Ledger describe the state at
	// that link, and recall works through them) but is NOT continued by the
	// source's child: it is a fresh branch, so the seal does not carry over.
	return &Session{
		ID:       newSessionID(),
		Title:    "fork of " + src.ID,
		Cwd:      cwd,
		Provider: src.Provider,
		Protocol: src.Protocol,
		URL:      src.URL,
		Model:    src.Model,
		Parent:   src.Parent,
		Ledger:   ledger,
		Created:  time.Now(),
		Turns:    turns,
	}, nil
}

// ---------------------------------------------------------------------------
// Session locks: one live instance per session. Multiple sessions in the same
// directory are first-class (every plain start is a fresh id); what must not
// happen is two instances appending to the SAME session file, where the last
// writer silently discards the other's turns. A pid file per active session
// makes the collision loud instead.
// ---------------------------------------------------------------------------

func lockPath(id string) string { return filepath.Join(sessionsDir(), id+".lock") }

// acquireLock claims a session for this process. A lock held by a live
// process is a real conflict; one left by a dead process is stale and taken
// over. Locking is best-effort protection against accidents, not security.
func acquireLock(id string) error {
	os.MkdirAll(sessionsDir(), 0o755)
	for attempt := 0; attempt < 2; attempt++ {
		f, err := os.OpenFile(lockPath(id), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
		if err == nil {
			fmt.Fprintf(f, "%d", os.Getpid())
			return f.Close()
		}
		b, rerr := os.ReadFile(lockPath(id))
		if rerr != nil {
			continue // raced with a release; retry the create
		}
		pid, _ := strconv.Atoi(strings.TrimSpace(string(b)))
		if pid > 0 && pidAlive(pid) {
			return fmt.Errorf("session %s is open in another sesh (pid %d); each session runs in one instance at a time", id, pid)
		}
		os.Remove(lockPath(id)) // stale: owner is gone
	}
	return fmt.Errorf("could not lock session %s", id)
}

// releaseLock drops this process's claim. It only removes a lock it owns, so
// a successor's claim on a recycled id is never destroyed.
func releaseLock(id string) {
	b, err := os.ReadFile(lockPath(id))
	if err != nil {
		return
	}
	if pid, _ := strconv.Atoi(strings.TrimSpace(string(b))); pid == os.Getpid() {
		os.Remove(lockPath(id))
	}
}

// pidAlive reports whether a process exists. Signal 0 probes without
// touching: ESRCH means gone; EPERM means alive under another user.
func pidAlive(pid int) bool {
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	err = proc.Signal(syscall.Signal(0))
	return err == nil || errors.Is(err, syscall.EPERM)
}

// chainTip follows continued_by pointers to the live end of a handoff chain.
// A sealed session must never accumulate new turns (its archived transcript
// is what descendants' recall reads), so resuming one lands here instead.
// Returns how many hops were taken; a missing child file ends the walk at the
// last loadable link. The walk is unbounded by depth (a weeks-long chain is
// thousands of links and resuming it must still work); the seen map guards
// corrupt cycles.
func chainTip(s *Session) (*Session, int) {
	hops := 0
	seen := map[string]bool{}
	for s.Child != "" && !seen[s.ID] {
		seen[s.ID] = true
		next, err := loadSession(s.Child)
		if err != nil {
			break
		}
		s = next
		hops++
	}
	return s, hops
}

func allSessions() []*Session {
	entries, err := os.ReadDir(sessionsDir())
	if err != nil {
		return nil
	}
	var out []*Session
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		if s, err := loadSession(strings.TrimSuffix(e.Name(), ".json")); err == nil {
			out = append(out, s)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Updated.After(out[j].Updated) })
	return out
}

// latestSession returns the most recent session recorded in cwd, so -continue
// picks up this project's work rather than whatever ran last somewhere else.
// Sealed sessions (handed off, continued elsewhere) are never candidates: the
// live end of their chain is, and it sorts later anyway.
// Sessions from elsewhere are reachable explicitly via -resume or -fork.
func latestSession(cwd string) (*Session, error) {
	sessions := allSessions()
	if len(sessions) == 0 {
		return nil, fmt.Errorf("no sessions to continue in %s", sessionsDir())
	}
	for _, s := range sessions {
		if s.Cwd == cwd && s.Child == "" {
			return s, nil
		}
	}
	return nil, fmt.Errorf("no sessions for this directory (%d elsewhere; sesh -list to see them, -resume <id> to pick one)", len(sessions))
}

// lastBrain returns the most recent session whose brain a fresh session
// should adopt: this directory's latest first, then the latest anywhere.
// Nil when there is no history at all (first run), which falls back to the
// config default.
func lastBrain(cwd string) *Session {
	sessions := allSessions()
	for _, s := range sessions {
		if s.Cwd == cwd {
			return s
		}
	}
	if len(sessions) > 0 {
		return sessions[0]
	}
	return nil
}

func printSessions() {
	sessions := allSessions()
	if len(sessions) == 0 {
		fmt.Printf("no sessions yet (they autosave to %s)\n", sessionsDir())
		return
	}
	home := os.Getenv("HOME")
	for _, s := range sessions {
		dir := s.Cwd
		if home != "" {
			dir = strings.Replace(dir, home, "~", 1)
		}
		mark := ""
		if s.Child != "" {
			mark = "  [sealed -> " + s.Child + "]"
		}
		fmt.Printf("%s  %s  %-24s %-20s %3d turns  %q%s\n",
			s.ID, s.Updated.Format("2006-01-02 15:04"), s.Model, dir, len(s.Turns), s.Title, mark)
	}
}
