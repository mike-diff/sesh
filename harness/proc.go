// Process supervisor: long-lived background processes the session owns.
//
// The lifecycle is core because it is tied to sesh's own lifetime: every
// managed process runs in its own process group, so stopping one never touches
// its parent or sesh, and the whole set is reaped when the session exits. A
// foreground bash command that outlives the promote timeout is moved here
// rather than killed, so a dev server or a slow build keeps running with a
// handle instead of dying and losing its output.
//
// The read side stays deliberately small: a bounded ring of CLEANED output
// (ANSI and carriage-return spinners stripped), a non-destructive cursor, and
// tail-biased truncation with a token estimate. Cleverer summarization
// (dedup, error classification) is a user-space concern, layered over this via
// the tool-mod contract, never baked into the binary.
package harness

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/mike-diff/sesh/agent"
)

// runDir holds a session's proc logs and the crash-recovery record.
func runDir(sessionID string) string {
	return filepath.Join(os.Getenv("HOME"), ".sesh", "run", sessionID)
}

const (
	procRingMax  = maxBashOutput // bound the in-memory cleaned tail (1 MiB)
	procSpillMax = 8 << 20       // bound the on-disk log per process
	killGrace    = 400 * time.Millisecond
	portCacheTTL = 2 * time.Second
)

var errShapeRe = regexp.MustCompile(`(?i)(error|panic|fatal|exception|traceback)`)

func errShape(line []byte) bool { return errShapeRe.Match(line) }

// ---------------------------------------------------------------------------
// ring: a bounded byte buffer that tracks how much it dropped from the head,
// so a logical cursor survives the buffer wrapping.
// ---------------------------------------------------------------------------

type ring struct {
	buf  []byte
	max  int
	base int64 // logical bytes evicted from the head
}

func (r *ring) append(p []byte) {
	r.buf = append(r.buf, p...)
	if over := len(r.buf) - r.max; over > 0 {
		r.buf = append([]byte(nil), r.buf[over:]...)
		r.base += int64(over)
	}
}

func (r *ring) total() int64 { return r.base + int64(len(r.buf)) }

// from returns the cleaned bytes at or after logical offset off, and whether
// off predated the buffer (so the caller can report truncation).
func (r *ring) from(off int64) ([]byte, bool) {
	trunc := false
	if off < r.base {
		off, trunc = r.base, true
	}
	idx := off - r.base
	if idx < 0 {
		idx = 0
	}
	if idx > int64(len(r.buf)) {
		idx = int64(len(r.buf))
	}
	return r.buf[idx:], trunc
}

// ---------------------------------------------------------------------------
// lineCleaner: a streaming io.Writer that strips ANSI CSI/OSC escapes and
// collapses carriage-return progress to the final value per line, emitting one
// completed (newline-terminated) cleaned line at a time. A partial trailing
// line waits for its newline, which is fine for line-oriented server logs.
// ---------------------------------------------------------------------------

type lineCleaner struct {
	cur   []byte
	state int // 0 normal, 1 esc, 2 csi, 3 osc, 4 osc-esc
	emit  func([]byte)
}

func (c *lineCleaner) Write(p []byte) (int, error) {
	for _, b := range p {
		switch c.state {
		case 1: // after ESC
			switch b {
			case '[':
				c.state = 2
			case ']':
				c.state = 3
			default:
				c.state = 0
			}
		case 2: // CSI ... final byte in @..~
			if b >= 0x40 && b <= 0x7e {
				c.state = 0
			}
		case 3: // OSC ... terminated by BEL or ESC '\'
			if b == 0x07 {
				c.state = 0
			} else if b == 0x1b {
				c.state = 4
			}
		case 4: // OSC ESC
			if b == '\\' {
				c.state = 0
			} else {
				c.state = 3
			}
		default: // normal
			switch b {
			case 0x1b:
				c.state = 1
			case '\r':
				c.cur = c.cur[:0] // spinner overwrite: reset the line
			case '\n':
				c.cur = append(c.cur, '\n')
				c.emit(c.cur)
				c.cur = c.cur[:0]
			default:
				c.cur = append(c.cur, b)
			}
		}
	}
	return len(p), nil
}

// flush emits any partial trailing line (output that never ended in a newline).
// Safe only once the writing side has stopped, i.e. after the process exits.
func (c *lineCleaner) flush() {
	if len(c.cur) > 0 {
		c.emit(c.cur)
		c.cur = c.cur[:0]
	}
}

// ---------------------------------------------------------------------------
// Proc: one managed process.
// ---------------------------------------------------------------------------

type Proc struct {
	ID, Name, Command string
	pgid              int
	started           time.Time

	cl       *lineCleaner // the output cleaner, flushed on exit
	mu       sync.Mutex
	bg       bool // shown in list/footer (promoted or started explicitly)
	status   string
	exitCode int
	ended    bool
	ring     ring
	spill    *os.File
	spilled  int
	readOff  int64 // auto-cursor: logical bytes already returned by logs
	newLines int   // cleaned lines since the last read
	errHits  int   // error-shaped lines since the last read
	ports    []int
	portsAt  time.Time
}

func (p *Proc) appendLine(line []byte) {
	p.mu.Lock()
	p.ring.append(line)
	if p.spill != nil && p.spilled < procSpillMax {
		n, _ := p.spill.Write(line)
		p.spilled += n
	}
	p.newLines++
	if errShape(line) {
		p.errHits++
	}
	p.mu.Unlock()
}

// portList returns the process group's listening TCP ports, cached briefly.
func (p *Proc) portList() []int {
	p.mu.Lock()
	if time.Since(p.portsAt) < portCacheTTL {
		ports := p.ports
		p.mu.Unlock()
		return ports
	}
	ended, pgid := p.ended, p.pgid
	p.mu.Unlock()
	var ports []int
	if !ended {
		ports = listeningPorts(pgid)
	}
	p.mu.Lock()
	p.ports, p.portsAt = ports, time.Now()
	p.mu.Unlock()
	return ports
}

// ---------------------------------------------------------------------------
// procManager: the per-session registry.
// ---------------------------------------------------------------------------

type procManager struct {
	mu        sync.Mutex
	sessionID string
	procs     []*Proc
	seq       int
	onChange  func() // footer refresh; may be nil (print mode, tests)
}

func newProcManager(sessionID string) *procManager { return &procManager{sessionID: sessionID} }

func (m *procManager) notify() {
	if m.onChange != nil {
		m.onChange()
	}
}

// launch starts command in its own process group, wiring cleaned output into a
// new Proc. The Proc is registered immediately (so its group is reaped even if
// it dies mid-command) but stays hidden until bg is set. The returned channel
// delivers the process's exit error exactly once.
func (m *procManager) launch(command, name string) (*Proc, <-chan error, error) {
	m.mu.Lock()
	m.seq++
	p := &Proc{ID: fmt.Sprintf("proc-%d", m.seq), Name: name, Command: command, status: "running", started: time.Now()}
	p.ring.max = procRingMax
	m.mu.Unlock()
	if p.Name == "" {
		p.Name = procName(command)
	}
	if !tune.ProcSpillOff {
		os.MkdirAll(runDir(m.sessionID), 0o755)
		if f, err := os.Create(filepath.Join(runDir(m.sessionID), p.ID+".log")); err == nil {
			p.spill = f
		}
	}
	cmd := exec.Command("bash", "-c", command)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cl := &lineCleaner{emit: p.appendLine}
	p.cl = cl
	cmd.Stdout, cmd.Stderr = cl, cl
	if err := cmd.Start(); err != nil {
		if p.spill != nil {
			p.spill.Close()
		}
		return nil, nil, err
	}
	p.pgid = cmd.Process.Pid // group leader: pgid == pid
	m.mu.Lock()
	m.procs = append(m.procs, p)
	m.mu.Unlock()
	m.persist()
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	return p, done, nil
}

// start runs a background process for the proc tool. reused is true when an
// identical command is already running and we hand back that process instead of
// launching a duplicate (the "reuse-if-owned" half of conflict handling).
func (m *procManager) start(command, name string) (p *Proc, reused bool, err error) {
	if existing := m.findRunningByCommand(command); existing != nil {
		return existing, true, nil
	}
	if m.bgCount() >= tune.MaxProcs {
		return nil, false, fmt.Errorf("at the %d-process limit (max_procs); stop one before starting another", tune.MaxProcs)
	}
	p, done, err := m.launch(command, name)
	if err != nil {
		return nil, false, err
	}
	p.mu.Lock()
	p.bg = true
	p.mu.Unlock()
	go func() { m.finalize(p, <-done) }()
	m.notify()
	return p, false, nil
}

// findRunningByCommand returns an owned, still-running process with the same
// command, so the agent (or a handoff successor) reuses it instead of stacking
// duplicate dev servers.
func (m *procManager) findRunningByCommand(command string) *Proc {
	cmd := strings.TrimSpace(command)
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, p := range m.procs {
		p.mu.Lock()
		match := p.bg && !p.ended && strings.TrimSpace(p.Command) == cmd
		p.mu.Unlock()
		if match {
			return p
		}
	}
	return nil
}

func (m *procManager) bgCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for _, p := range m.procs {
		p.mu.Lock()
		if p.bg && !p.ended {
			n++
		}
		p.mu.Unlock()
	}
	return n
}

// finalize records a process's exit, keeping it in the registry so its final
// logs stay readable until the session ends. A fast failure gets a port-conflict
// note when its output names a port already in use.
func (m *procManager) finalize(p *Proc, err error) {
	p.cl.flush() // the process is done; emit any unterminated last line
	p.mu.Lock()
	fast := time.Since(p.started) < 5*time.Second
	if err == nil {
		p.status, p.exitCode = "exited", 0
	} else if ee, ok := err.(*exec.ExitError); ok {
		p.status, p.exitCode = "exited", ee.ExitCode()
	} else {
		p.status, p.exitCode = "crashed", -1
	}
	failed := err != nil
	p.mu.Unlock()
	if fast && failed {
		m.annotatePortConflict(p) // appends a note to the log via appendLine
	}
	p.mu.Lock()
	p.ended = true
	if p.spill != nil {
		p.spill.Close()
		p.spill = nil
	}
	p.mu.Unlock()
	m.persist()
	m.notify()
}

// killGroup signals the whole process group: SIGTERM, a grace window, SIGKILL.
func killGroup(pgid int) {
	if pgid <= 0 {
		return
	}
	syscall.Kill(-pgid, syscall.SIGTERM)
	time.Sleep(killGrace)
	syscall.Kill(-pgid, syscall.SIGKILL)
}

// stop terminates one process by id.
func (m *procManager) stop(id string) (string, bool) {
	p := m.byID(id)
	if p == nil {
		return "no such process: " + id, true
	}
	p.mu.Lock()
	ended, pgid := p.ended, p.pgid
	p.mu.Unlock()
	if ended {
		return id + " already stopped", false
	}
	killGroup(pgid)
	m.notify()
	return "stopped " + id, false
}

// reapAll stops every owned process on session exit. Safe to call more than
// once: a SIGTERM/SIGKILL to a dead group is harmless.
func (m *procManager) reapAll() {
	m.mu.Lock()
	procs := append([]*Proc(nil), m.procs...)
	m.mu.Unlock()
	var live []int
	for _, p := range procs {
		p.mu.Lock()
		if !p.ended && p.pgid > 0 {
			live = append(live, p.pgid)
		}
		p.mu.Unlock()
	}
	for _, pgid := range live {
		syscall.Kill(-pgid, syscall.SIGTERM)
	}
	if len(live) > 0 {
		time.Sleep(killGrace)
		for _, pgid := range live {
			syscall.Kill(-pgid, syscall.SIGKILL)
		}
	}
	os.RemoveAll(runDir(m.sessionID))
}

// killAllNow is the force-quit reaper: SIGTERM then SIGKILL every owned group
// back to back, with no grace window, so a second Ctrl-C exits at once instead
// of blocking on a stubborn process. The signals are sent in the same loop so
// the kernel queues SIGKILL right behind SIGTERM; the caller exits immediately
// after and the OS reaps any group that has not died yet. Returns the number of
// groups it signalled, so a test can prove it acted without sleeping.
func (m *procManager) killAllNow() int {
	m.mu.Lock()
	procs := append([]*Proc(nil), m.procs...)
	m.mu.Unlock()
	signalled := 0
	for _, p := range procs {
		p.mu.Lock()
		pgid := p.pgid
		dead := p.ended || pgid <= 0
		p.mu.Unlock()
		if dead {
			continue
		}
		syscall.Kill(-pgid, syscall.SIGTERM)
		syscall.Kill(-pgid, syscall.SIGKILL)
		signalled++
	}
	return signalled
}

func (m *procManager) byID(id string) *Proc {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, p := range m.procs {
		if p.ID == id {
			return p
		}
	}
	return nil
}

func (m *procManager) drop(p *Proc) {
	m.mu.Lock()
	for i, q := range m.procs {
		if q == p {
			m.procs = append(m.procs[:i], m.procs[i+1:]...)
			break
		}
	}
	m.mu.Unlock()
	if p.spill != nil {
		p.spill.Close()
		os.Remove(filepath.Join(runDir(m.sessionID), p.ID+".log"))
	}
	m.persist()
}

// ---------------------------------------------------------------------------
// doBash: the foreground bash tool, with auto-promote on timeout.
// ---------------------------------------------------------------------------

// doBash runs a command in the foreground and returns its output, unless it
// outlives the promote timeout, in which case it becomes a tracked background
// process and the model gets a handle instead of a kill. Ctrl-C (a cancelled
// turn) kills the command; the timeout never does.
func (m *procManager) doBash(ctx context.Context, command string) (string, bool) {
	p, done, err := m.launch(command, "")
	if err != nil {
		return "could not start command: " + err.Error(), true
	}
	promote := time.Duration(tune.ProcPromoteSecs) * time.Second
	select {
	case werr := <-done:
		p.cl.flush() // the process is done; emit any unterminated last line
		out := m.foregroundOutput(p)
		m.drop(p)
		if werr != nil {
			return strings.TrimSpace(out + "\n" + werr.Error()), true
		}
		if out == "" {
			return "(no output)", false
		}
		return out, false
	case <-time.After(promote):
		p.mu.Lock()
		p.bg = true
		recent := string(tailBytes(p.ring.buf, 4096))
		p.mu.Unlock()
		go func() { m.finalize(p, <-done) }()
		m.notify()
		msg := fmt.Sprintf("still running after %ds; promoted to background as %s. tail it with proc(action:\"logs\", id:\"%s\") or end it with proc(action:\"stop\", id:\"%s\").", tune.ProcPromoteSecs, p.ID, p.ID, p.ID)
		if recent != "" {
			msg += "\noutput so far:\n" + recent
		}
		return msg, false
	case <-ctx.Done():
		killGroup(p.pgid)
		<-done
		m.drop(p)
		return "command cancelled", true
	}
}

// foregroundOutput returns a finished foreground command's whole cleaned
// output, capped to maxBashOutput.
func (m *procManager) foregroundOutput(p *Proc) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	s := strings.TrimRight(string(p.ring.buf), "\n")
	if p.ring.base > 0 {
		s = fmt.Sprintf("... [output capped: %d earlier bytes dropped]\n", p.ring.base) + s
	}
	return s
}

// ---------------------------------------------------------------------------
// The proc tool.
// ---------------------------------------------------------------------------

const procToolDesc = `Manage long-lived background processes (dev servers, watchers, a client and server at once). action:
- "start": run command in the background; returns its id. Use this for anything that does not return on its own. Starting a command that is already running reuses it (no duplicate). If it dies because its port is taken, logs name the holder; never kill a process you did not start.
- "list": show owned processes with their ports, status, and how much new log output is waiting.
- "logs": return new output since your last read for one process (incremental). Optional: tail (last N lines), filter (a regex to narrow the view; non-destructive).
- "stop": terminate one process by id.
A plain bash command that outlives the timeout is auto-promoted here, so check list/logs if a bash call reports a promotion. Running processes survive a handoff: do not restart one the handoff brief says is already up. Process output is kept out of your context; pull it with logs.`

func procSchema() map[string]any {
	str := func(d string) map[string]any { return map[string]any{"type": "string", "description": d} }
	num := func(d string) map[string]any { return map[string]any{"type": "integer", "description": d} }
	return map[string]any{
		"type":     "object",
		"required": []string{"action"},
		"properties": map[string]any{
			"action":  map[string]any{"type": "string", "enum": []string{"start", "list", "logs", "stop"}, "description": "start | list | logs | stop"},
			"command": str("for start: the shell command to run in the background."),
			"name":    str("for start: an optional short label (defaults to the command's program)."),
			"id":      str("for logs/stop: the process id, e.g. proc-1."),
			"tail":    num("for logs: return only the last N lines."),
			"filter":  str("for logs: a regex; only matching lines are returned (the buffer is not consumed)."),
		},
	}
}

type procArgs struct {
	Action  string `json:"action"`
	Command string `json:"command"`
	Name    string `json:"name"`
	ID      string `json:"id"`
	Tail    int    `json:"tail"`
	Filter  string `json:"filter"`
}

// runTool dispatches the proc tool. ctx carries the turn's cancellation; every
// action here is a fast bookkeeping call (start spawns and returns, the rest
// only read or signal), so none blocks on it. The parameter keeps the proc
// tool's Run on the same cancellable contract as bash, so a future blocking
// action inherits cancellation instead of having to retrofit the signature.
func (m *procManager) runTool(ctx context.Context, raw json.RawMessage) (string, bool) {
	var a procArgs
	if err := json.Unmarshal(raw, &a); err != nil {
		return "invalid proc input: " + err.Error(), true
	}
	switch a.Action {
	case "start":
		if strings.TrimSpace(a.Command) == "" {
			return "proc start needs a command", true
		}
		p, reused, err := m.start(a.Command, a.Name)
		if err != nil {
			return err.Error(), true
		}
		if reused {
			return fmt.Sprintf("%s is already running that command; reused it instead of starting a duplicate", p.ID), false
		}
		return fmt.Sprintf("started %s (%s): %s", p.ID, p.Name, a.Command), false
	case "list":
		return m.listText(), false
	case "logs":
		if a.ID == "" {
			return "proc logs needs an id", true
		}
		return m.logsText(a.ID, a.Tail, a.Filter)
	case "stop":
		if a.ID == "" {
			return "proc stop needs an id", true
		}
		return m.stop(a.ID)
	default:
		return "unknown proc action: " + a.Action, true
	}
}

// snapshot copies the display-relevant fields under lock.
type procView struct {
	id, name, command, status string
	exitCode                  int
	ended                     bool
	uptime                    time.Duration
	newLines, errHits         int
	ports                     []int
}

func (m *procManager) views(bgOnly bool) []procView {
	m.mu.Lock()
	procs := append([]*Proc(nil), m.procs...)
	m.mu.Unlock()
	var out []procView
	for _, p := range procs {
		ports := p.portList()
		p.mu.Lock()
		if bgOnly && !p.bg {
			p.mu.Unlock()
			continue
		}
		out = append(out, procView{
			id: p.ID, name: p.Name, command: p.Command, status: p.status, exitCode: p.exitCode,
			ended: p.ended, uptime: time.Since(p.started), newLines: p.newLines, errHits: p.errHits, ports: ports,
		})
		p.mu.Unlock()
	}
	return out
}

func (m *procManager) listText() string {
	vs := m.views(true)
	if len(vs) == 0 {
		return "no background processes"
	}
	var b strings.Builder
	for _, v := range vs {
		st := v.status
		if v.ended {
			st = fmt.Sprintf("%s(%d)", v.status, v.exitCode)
		}
		port := "-"
		if len(v.ports) > 0 {
			port = portsStr(v.ports)
		}
		fmt.Fprintf(&b, "%-7s %-10s %-6s %-9s %s  +%d new", v.id, clip(v.name, 10), port, st, shortDur(v.uptime), v.newLines)
		if v.errHits > 0 {
			fmt.Fprintf(&b, " %d err", v.errHits)
		}
		fmt.Fprintf(&b, "  %s\n", v.command)
	}
	return strings.TrimRight(b.String(), "\n")
}

// logsText returns new output since the last read, tail-biased and with a
// token estimate. The full cleaned log stays in the ring, so a regex filter is
// a view, never a consume: the same range is re-readable with a smaller tail.
func (m *procManager) logsText(id string, tail int, filter string) (string, bool) {
	p := m.byID(id)
	if p == nil {
		return "no such process: " + id, true
	}
	var re *regexp.Regexp
	if filter != "" {
		var err error
		if re, err = regexp.Compile(filter); err != nil {
			return "bad filter regex: " + err.Error(), true
		}
	}
	if tail <= 0 {
		tail = tune.ProcLogTail
	}
	p.mu.Lock()
	data, trunc := p.ring.from(p.readOff)
	body := append([]byte(nil), data...)
	// A filtered read is a non-destructive peek: it must not advance the cursor
	// past the lines it didn't show, or an unfiltered read would never see them.
	// Only a plain read consumes.
	if re == nil {
		p.readOff = p.ring.total()
		p.newLines, p.errHits = 0, 0
	}
	ended, status, code := p.ended, p.status, p.exitCode
	p.mu.Unlock()

	lines := splitLines(body)
	elided := 0
	if len(lines) > tail {
		elided = len(lines) - tail
		lines = lines[elided:]
	}
	if re != nil {
		kept := lines[:0]
		for _, ln := range lines {
			if re.MatchString(ln) {
				kept = append(kept, ln)
			}
		}
		lines = kept
	}
	out := strings.Join(lines, "\n")
	var meta []string
	if trunc {
		meta = append(meta, "older output dropped")
	}
	if elided > 0 {
		meta = append(meta, fmt.Sprintf("%d earlier lines elided", elided))
	}
	meta = append(meta, fmt.Sprintf("~%d tokens", len(out)/4))
	if ended {
		meta = append(meta, fmt.Sprintf("process %s(%d)", status, code))
	}
	tag := fmt.Sprintf("[%s logs: %s]", id, strings.Join(meta, "; "))
	if out == "" {
		return "(no new output) " + tag, false
	}
	return out + "\n" + tag, false
}

// manifestLine is the footer's process row: cheap, glanceable, fitted to width.
func (m *procManager) manifestLine(width int) string {
	vs := m.views(true)
	if len(vs) == 0 {
		return ""
	}
	parts := make([]string, 0, len(vs))
	for _, v := range vs {
		seg := v.name
		if len(v.ports) > 0 {
			seg += " " + portsStr(v.ports)
		}
		switch {
		case v.ended:
			seg += " ✗"
		case len(v.ports) > 0:
			seg += " ●"
		default:
			seg += " ·"
		}
		seg += " " + shortDur(v.uptime)
		if v.newLines > 0 {
			seg += fmt.Sprintf(" +%d", v.newLines)
		}
		if v.errHits > 0 {
			seg += fmt.Sprintf(" ⚠%d", v.errHits)
		}
		parts = append(parts, seg)
	}
	full := "⚙ " + strings.Join(parts, "   ")
	if len([]rune(full)) <= width {
		return full
	}
	// Too wide: collapse to a count plus ports.
	var ports []int
	for _, v := range vs {
		ports = append(ports, v.ports...)
	}
	collapsed := fmt.Sprintf("⚙ %d running", len(vs))
	if len(ports) > 0 {
		collapsed += " " + portsStr(ports)
	}
	return collapsed
}

// ---------------------------------------------------------------------------
// Crash backstop: a tiny record per session, swept on the next startup. Reap
// only when the group leader is provably the same process we started (pid
// alive with a matching start time), so a recycled pid is never killed.
// ---------------------------------------------------------------------------

type procRecord struct {
	ID, Command string
	Pgid        int
	LeaderStart string // /proc/<pgid>/stat start time, to defeat pid reuse
}

func (m *procManager) persist() {
	m.mu.Lock()
	var recs []procRecord
	for _, p := range m.procs {
		p.mu.Lock()
		if p.bg && !p.ended && p.pgid > 0 {
			recs = append(recs, procRecord{ID: p.ID, Command: p.Command, Pgid: p.pgid, LeaderStart: procStartTime(p.pgid)})
		}
		p.mu.Unlock()
	}
	id := m.sessionID
	m.mu.Unlock()
	path := filepath.Join(runDir(id), "procs.json")
	if len(recs) == 0 {
		os.Remove(path)
		return
	}
	os.MkdirAll(runDir(id), 0o755)
	if b, err := json.Marshal(recs); err == nil {
		os.WriteFile(path, b, 0o644)
	}
}

// sweepDeadProcs reaps background processes left behind by a previous sesh that
// exited without cleaning up (a crash or kill -9). Live sessions and the
// current one are skipped; a recorded group is killed only when its leader is
// verifiably still our process.
func sweepDeadProcs(currentSessionID string) {
	base := filepath.Join(os.Getenv("HOME"), ".sesh", "run")
	entries, err := os.ReadDir(base)
	if err != nil {
		return
	}
	for _, e := range entries {
		id := e.Name()
		if id == currentSessionID || sessionLocked(id) {
			continue
		}
		b, err := os.ReadFile(filepath.Join(base, id, "procs.json"))
		if err == nil {
			var recs []procRecord
			if json.Unmarshal(b, &recs) == nil {
				for _, r := range recs {
					if r.Pgid > 0 && pidAlive(r.Pgid) && procStartTime(r.Pgid) == r.LeaderStart {
						syscall.Kill(-r.Pgid, syscall.SIGKILL)
					}
				}
			}
		}
		os.RemoveAll(filepath.Join(base, id))
	}
}

// sessionLocked reports whether a session's live-instance lock is held by a
// running process.
func sessionLocked(id string) bool {
	b, err := os.ReadFile(lockPath(id))
	if err != nil {
		return false
	}
	pid, _ := strconv.Atoi(strings.TrimSpace(string(b)))
	return pid > 0 && pidAlive(pid)
}

// ---------------------------------------------------------------------------
// Handoff inheritance: the supervisor follows the live end of the chain. The
// process registry is in-memory, so it survives a handoff for free (same
// process); rekey just re-points the on-disk paths, and seedNote tells the
// successor what is already running so it reuses rather than duplicates.
// ---------------------------------------------------------------------------

// rekey re-points the manager at the successor session on a handoff. The run
// dir is moved so log spill and the crash record follow the live session;
// already-open spill descriptors keep writing to the same inode.
func (m *procManager) rekey(newID string) {
	m.mu.Lock()
	old := m.sessionID
	m.sessionID = newID
	m.mu.Unlock()
	if old == newID {
		return
	}
	if _, err := os.Stat(runDir(old)); err == nil {
		os.MkdirAll(filepath.Dir(runDir(newID)), 0o755)
		os.Rename(runDir(old), runDir(newID))
	}
	m.persist()
	m.notify()
}

// seedNote lists the still-running owned processes for a handoff seed, so the
// successor inherits them instead of starting duplicates.
func (m *procManager) seedNote() string {
	var b strings.Builder
	for _, v := range m.views(true) {
		if v.ended {
			continue
		}
		port := ""
		if len(v.ports) > 0 {
			port = " on " + portsStr(v.ports)
		}
		fmt.Fprintf(&b, "- %s (%s)%s: %s\n", v.id, v.name, port, v.command)
	}
	if b.Len() == 0 {
		return ""
	}
	return "background processes already running (inherited; reuse them, do not restart):\n" + strings.TrimRight(b.String(), "\n")
}

// ---------------------------------------------------------------------------
// Port-conflict detection: identify who holds a port, and never kill a process
// this session did not start.
// ---------------------------------------------------------------------------

var addrInUseRe = regexp.MustCompile(`(?i)(address already in use|eaddrinuse|already in use|port \d+ .*in use)`)
var portInMsgRe = regexp.MustCompile(`(?i)(?::|port\s+)(\d{2,5})\b`)

// parseAddrInUse extracts the port from an address-in-use error, or 0 if the
// output is not such an error (or names no port, like Python's bare errno 98).
func parseAddrInUse(out string) int {
	if !addrInUseRe.MatchString(out) {
		return 0
	}
	if mm := portInMsgRe.FindStringSubmatch(out); mm != nil {
		if p, _ := strconv.Atoi(mm[1]); p > 0 && p < 65536 {
			return p
		}
	}
	return 0
}

// annotatePortConflict appends a human-and-model-readable note to a process's
// log when it died because its port was taken, naming the holder. It never
// kills anything.
func (m *procManager) annotatePortConflict(p *Proc) {
	p.mu.Lock()
	out := string(p.ring.buf)
	p.mu.Unlock()
	port := parseAddrInUse(out)
	if port == 0 {
		return
	}
	m.mu.Lock()
	owned := append([]*Proc(nil), m.procs...)
	m.mu.Unlock()
	pid, cmd, ours := portHolder(port, owned)
	var note string
	switch {
	case ours != nil && ours != p:
		note = fmt.Sprintf("[sesh] port %d is already served by %s (%s); reuse it instead of starting another.", port, ours.ID, ours.Name)
	case pid > 0:
		note = fmt.Sprintf("[sesh] port %d is in use by pid %d (%s), which this session does not own; stop that process or use a different port. sesh will not kill processes it did not start.", port, pid, cmd)
	default:
		note = fmt.Sprintf("[sesh] port %d is already in use; start on a different port.", port)
	}
	p.appendLine([]byte(note + "\n"))
}

// portHolder finds the process listening on a TCP port: its pid, command, and
// the owned Proc if this session started it. pid 0 means nothing holds it.
func portHolder(port int, owned []*Proc) (pid int, command string, ours *Proc) {
	inode := listenInodeForPort(port)
	if inode == "" {
		return 0, "", nil
	}
	pid = pidForInode(inode)
	if pid == 0 {
		return 0, "", nil
	}
	command = procCmdline(pid)
	pg := processPgid(pid)
	for _, p := range owned {
		if pg > 0 && p.pgid == pg {
			ours = p
			break
		}
	}
	return pid, command, ours
}

// ---------------------------------------------------------------------------
// gating + small helpers
// ---------------------------------------------------------------------------

// mutates reports whether a tool call changes state, for the gate and the
// no-progress detector. proc is action-aware: only start/stop mutate, so a
// read-only list/logs never trips an approval prompt.
func mutates(tc agent.ToolCall) bool {
	if tc.Name == "proc" {
		var a struct {
			Action string `json:"action"`
		}
		json.Unmarshal(tc.Args, &a)
		return a.Action == "start" || a.Action == "stop"
	}
	return mutating[tc.Name]
}

func procName(command string) string {
	for _, f := range strings.Fields(command) {
		if strings.Contains(f, "=") { // skip leading VAR=val
			continue
		}
		return filepath.Base(f)
	}
	return "proc"
}

func portsStr(ports []int) string {
	seen := map[int]bool{}
	var parts []string
	for _, p := range ports {
		if !seen[p] {
			seen[p] = true
			parts = append(parts, ":"+strconv.Itoa(p))
		}
	}
	return strings.Join(parts, " ")
}

func shortDur(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	default:
		return fmt.Sprintf("%dh", int(d.Hours()))
	}
}

func clip(s string, n int) string {
	if len([]rune(s)) <= n {
		return s
	}
	return string([]rune(s)[:n-1]) + "…"
}

func splitLines(b []byte) []string {
	s := strings.TrimRight(string(b), "\n")
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}

func tailBytes(b []byte, n int) []byte {
	if len(b) <= n {
		return b
	}
	return b[len(b)-n:]
}
