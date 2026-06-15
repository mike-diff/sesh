package harness

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// waitFor polls cond up to d, so tests don't race the output-copy goroutine.
func waitFor(d time.Duration, cond func() bool) bool {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return cond()
}

// TestProcStopKillsGroup: a managed process and the children it spawned die
// together on stop. Breaker: drop Setpgid / kill the leader pid instead of the
// group, and the backgrounded child survives.
func TestProcStopKillsGroup(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	m := newProcManager("scale-stop")
	// bash backgrounds a sleep, prints its pid, then waits so the group stays up.
	p, err := m.start("sleep 30 & echo CHILD $!; wait", "")
	if err != nil {
		t.Fatal(err)
	}
	var childPID int
	if !waitFor(3*time.Second, func() bool {
		out, _ := m.logsText(p.ID, 0, "")
		for _, f := range strings.Fields(out) {
			if n, e := strconv.Atoi(f); e == nil && n > 1 {
				childPID = n
				return true
			}
		}
		return false
	}) {
		t.Fatal("never saw the child pid in the logs")
	}
	if !pidAlive(childPID) {
		t.Fatalf("child %d should be alive before stop", childPID)
	}
	if _, isErr := m.stop(p.ID); isErr {
		t.Fatal("stop reported an error")
	}
	if !waitFor(3*time.Second, func() bool { return !pidAlive(childPID) }) {
		t.Fatalf("child %d survived the group kill: Setpgid/group-kill is broken", childPID)
	}
}

// TestAutoPromote: a command that outlives the promote window becomes a tracked
// background process (handle returned, kept in the registry); a quick command
// returns synchronously and leaves nothing behind. Breaker: revert doBash to
// kill-at-timeout and the long command dies with no handle.
func TestAutoPromote(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	defer func(n int) { tune.ProcPromoteSecs = n }(tune.ProcPromoteSecs)
	tune.ProcPromoteSecs = 1
	m := newProcManager("scale-promote")

	out, isErr := m.doBash(context.Background(), "echo quick")
	if isErr || strings.TrimSpace(out) != "quick" {
		t.Fatalf("fast command must return synchronously: %q err=%v", out, isErr)
	}
	if n := len(m.views(false)); n != 0 {
		t.Fatalf("a finished foreground command must not linger: %d procs", n)
	}

	out, isErr = m.doBash(context.Background(), "sleep 5")
	if isErr || !strings.Contains(out, "promoted to background") {
		t.Fatalf("a long command must promote, not die: %q err=%v", out, isErr)
	}
	if n := len(m.views(true)); n != 1 {
		t.Fatalf("promoted process must be tracked: %d bg procs", n)
	}
	m.reapAll()
}

// TestPromoteCancel: a cancelled turn kills a still-running foreground command
// rather than promoting it. Breaker: promote on ctx cancel.
func TestPromoteCancel(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	defer func(n int) { tune.ProcPromoteSecs = n }(tune.ProcPromoteSecs)
	tune.ProcPromoteSecs = 30
	m := newProcManager("scale-cancel")
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(200 * time.Millisecond); cancel() }()
	out, isErr := m.doBash(ctx, "sleep 30")
	if !isErr || !strings.Contains(out, "cancel") {
		t.Fatalf("cancel must kill, not promote: %q err=%v", out, isErr)
	}
	if n := len(m.views(false)); n != 0 {
		t.Fatalf("a cancelled command must leave nothing: %d procs", n)
	}
}

// TestLogsIncrementalAndFilterNonDestructive: a plain read returns only new
// output and advances; a filtered read is a peek that does not consume, so the
// unmatched lines are still there for the next plain read. Breaker: advance the
// cursor on a filtered read and the next plain read comes back empty.
func TestLogsIncrementalAndFilterNonDestructive(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	m := newProcManager("scale-logs")
	p := &Proc{ID: "proc-1", status: "running", started: time.Now()}
	p.ring.max = procRingMax
	m.procs = append(m.procs, p)

	p.appendLine([]byte("alpha\n"))
	p.appendLine([]byte("boom error here\n"))
	p.appendLine([]byte("gamma\n"))

	out, _ := m.logsText("proc-1", 0, "error") // filtered peek
	if !strings.Contains(out, "boom error") || strings.Contains(out, "alpha") {
		t.Fatalf("filter must return only matches: %q", out)
	}
	out, _ = m.logsText("proc-1", 0, "") // plain read sees everything still
	if !strings.Contains(out, "alpha") || !strings.Contains(out, "gamma") {
		t.Fatalf("filter consumed the buffer (non-destructive broken): %q", out)
	}
	out, _ = m.logsText("proc-1", 0, "") // now consumed: nothing new
	if !strings.Contains(out, "no new output") {
		t.Fatalf("a plain read must advance the cursor: %q", out)
	}
}

// TestLineCleaner: ANSI escapes are stripped and carriage-return spinners
// collapse to the final value, even split across writes. Breaker: skip the
// cleaner and the ring keeps escape bytes and every spinner frame.
func TestLineCleaner(t *testing.T) {
	var got []string
	c := &lineCleaner{emit: func(b []byte) { got = append(got, string(b)) }}
	c.Write([]byte("\x1b[32mgr"))   // color start, split mid-line
	c.Write([]byte("een\x1b[0m\n")) // color end + newline
	c.Write([]byte("10%\r50%\r100%\n"))
	want := []string{"green\n", "100%\n"}
	if len(got) != len(want) {
		t.Fatalf("lines: %q", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("clean[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

// TestListenPortsParse: the /proc/net/tcp parser extracts a listening port for
// a matching socket inode and ignores non-listening or unmatched rows. Breaker:
// wrong field index, or failing to require state 0A (LISTEN).
func TestListenPortsParse(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "tcp")
	// header + a LISTEN on :3000 (0BB8) inode 111, an ESTABLISHED inode 111,
	// and a LISTEN inode 999 we don't own.
	body := "  sl  local_address rem_address   st ... uid timeout inode\n" +
		"   0: 00000000:0BB8 00000000:0000 0A x x x 0 0 111 1 y z w\n" +
		"   1: 00000000:1F90 01010101:1F90 01 x x x 0 0 111 1 y z w\n" +
		"   2: 00000000:2328 00000000:0000 0A x x x 0 0 999 1 y z w\n"
	os.WriteFile(f, []byte(body), 0o644)
	ports := listenPortsForInodes(f, map[string]bool{"111": true})
	if len(ports) != 1 || ports[0] != 3000 {
		t.Fatalf("want [3000], got %v", ports)
	}
}

// TestManifestCollapse: the footer row shows full detail when it fits and
// collapses to a count when it does not. Breaker: no collapse and the row
// overflows the terminal width.
func TestManifestCollapse(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	m := newProcManager("scale-manifest")
	for _, name := range []string{"web", "api"} {
		p := &Proc{ID: name, Name: name, status: "running", started: time.Now(), bg: true, ports: []int{3000}, portsAt: time.Now()}
		p.ring.max = procRingMax
		m.procs = append(m.procs, p)
	}
	if full := m.manifestLine(200); !strings.Contains(full, "web") || !strings.Contains(full, "api") {
		t.Fatalf("wide line must show both: %q", full)
	}
	collapsed := m.manifestLine(10)
	if !strings.Contains(collapsed, "running") || len([]rune(collapsed)) > 10 && !strings.Contains(collapsed, "running") {
		t.Fatalf("narrow line must collapse: %q", collapsed)
	}
	if strings.Contains(collapsed, "web") {
		t.Fatalf("narrow line must not list each proc: %q", collapsed)
	}
}

// TestSweepVerifiesLeader: the crash sweep must never kill a process whose
// recorded start time does not match the live pid (a recycled pid). Breaker:
// skip the procStartTime check and the innocent process is killed.
func TestSweepVerifiesLeader(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	// A harmless real process in its own group, standing in for a recycled pid.
	cmd := startGroupSleep(t)
	pgid := cmd.Process.Pid
	defer syscall.Kill(-pgid, syscall.SIGKILL)

	dead := filepath.Join(home, ".sesh", "run", "20200101-000000-deadbeef")
	os.MkdirAll(dead, 0o755)
	recs := []procRecord{{ID: "proc-1", Command: "x", Pgid: pgid, LeaderStart: "999999999"}} // wrong start time
	b, _ := json.Marshal(recs)
	os.WriteFile(filepath.Join(dead, "procs.json"), b, 0o644)

	sweepDeadProcs("current-session")

	if !pidAlive(pgid) {
		t.Fatal("sweep killed a process with a mismatched start time: pid-reuse guard is broken")
	}
	if _, err := os.Stat(dead); !os.IsNotExist(err) {
		t.Fatal("sweep must remove a dead session's run dir")
	}
}

// TestListeningPortsLive: against the real /proc, a port this process is
// actually listening on is discovered for its own process group. Breaker: any
// off-by-one in the /proc/net/tcp field parsing or fd-inode matching.
func TestListeningPortsLive(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skip("cannot bind a port:", err)
	}
	defer ln.Close()
	want := ln.Addr().(*net.TCPAddr).Port
	pgrp, err := syscall.Getpgid(os.Getpid())
	if err != nil {
		t.Skip("no pgid:", err)
	}
	got := listeningPorts(pgrp)
	for _, p := range got {
		if p == want {
			return
		}
	}
	t.Fatalf("port %d not found in %v: /proc port discovery is broken", want, got)
}

func startGroupSleep(t *testing.T) *exec.Cmd {
	t.Helper()
	cmd := exec.Command("sleep", "30")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go cmd.Wait()
	return cmd
}

// TestFooterProcRowGeometry: the footer draws an extra row only when a process
// line is set, and the cursor-up counts match (2 without it, 3 with it), so the
// input row stays put. Breaker: a fixed row count and the editor row drifts.
func TestFooterProcRowGeometry(t *testing.T) {
	read := func(setup func(*tuiConsole)) string {
		f, err := os.CreateTemp(t.TempDir(), "tui")
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		tc := &tuiConsole{out: f, cols: 80, rows: 24, status: "status"}
		setup(tc)
		b, _ := os.ReadFile(f.Name())
		return string(b)
	}
	plain := read(func(tc *tuiConsole) { tc.drawFooterLocked() })
	if !strings.Contains(plain, "\033[2A") || strings.Contains(plain, "\033[3A") {
		t.Fatalf("no-proc footer must move up 2 to the input row: %q", plain)
	}
	withProc := read(func(tc *tuiConsole) {
		tc.procStatus = "⚙ web :3000"
		tc.drawFooterLocked()
		if !tc.footerProc {
			t.Fatal("footerProc must record that the row was drawn")
		}
	})
	if !strings.Contains(withProc, "web :3000") || !strings.Contains(withProc, "\033[3A") {
		t.Fatalf("proc footer must show the row and move up 3: %q", withProc)
	}
}

// TestForegroundNoTrailingNewline: a command whose output never ends in a
// newline still returns its last line. Breaker: drop the cleaner flush and the
// partial line is lost (a regression from the old bash tool).
func TestForegroundNoTrailingNewline(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	m := newProcManager("scale-printf")
	out, isErr := m.doBash(context.Background(), "printf hi")
	if isErr || strings.TrimSpace(out) != "hi" {
		t.Fatalf("unterminated last line lost: %q err=%v", out, isErr)
	}
}

// TestProcToolDispatch: the model-facing JSON interface drives the whole
// lifecycle: start returns a handle, list shows it, logs reads it, stop ends it.
// Breaker: any action wiring or arg-parsing regression in runTool.
func TestProcToolDispatch(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	m := newProcManager("scale-tool")
	out, isErr := m.runTool([]byte(`{"action":"start","command":"echo hello; sleep 30","name":"svc"}`))
	if isErr || !strings.Contains(out, "started proc-1") {
		t.Fatalf("start: %q err=%v", out, isErr)
	}
	if out, _ := m.runTool([]byte(`{"action":"list"}`)); !strings.Contains(out, "proc-1") || !strings.Contains(out, "svc") {
		t.Fatalf("list must show the process: %q", out)
	}
	waitFor(2*time.Second, func() bool {
		o, _ := m.runTool([]byte(`{"action":"logs","id":"proc-1"}`))
		return strings.Contains(o, "hello")
	})
	if out, _ := m.runTool([]byte(`{"action":"logs","id":"proc-1"}`)); strings.Contains(out, "hello") {
		t.Fatalf("a second logs read must not repeat consumed output: %q", out)
	}
	if out, isErr := m.runTool([]byte(`{"action":"stop","id":"proc-1"}`)); isErr || !strings.Contains(out, "stopped proc-1") {
		t.Fatalf("stop: %q err=%v", out, isErr)
	}
	if out, _ := m.runTool([]byte(`{"action":"bogus"}`)); !strings.Contains(out, "unknown proc action") {
		t.Fatalf("unknown action must be reported: %q", out)
	}
	m.reapAll()
}
