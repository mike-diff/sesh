// The Linux /proc reading the supervisor needs: listening-port discovery for a
// process group, the holder of a given port, and the per-process start time
// that defeats pid reuse in the crash sweep. Split out of proc.go to keep the
// supervisor logic separate from the OS-specific parsing (and to mark, in one
// place, the part that is Linux-only). No lsof/ss dependency: stdlib + /proc.
package harness

import (
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
)

// listeningPorts returns the listening TCP ports held by any process in the
// group, read from /proc. Best-effort: any parse failure yields no port rather
// than a wrong one.
func listeningPorts(pgid int) []int {
	inodes := groupSocketInodes(pgid)
	if len(inodes) == 0 {
		return nil
	}
	set := map[int]bool{}
	for _, f := range []string{"/proc/net/tcp", "/proc/net/tcp6"} {
		for _, port := range listenPortsForInodes(f, inodes) {
			set[port] = true
		}
	}
	if len(set) == 0 {
		return nil
	}
	out := make([]int, 0, len(set))
	for p := range set {
		out = append(out, p)
	}
	sort.Ints(out)
	return out
}

func groupSocketInodes(pgid int) map[string]bool {
	procs, err := os.ReadDir("/proc")
	if err != nil {
		return nil
	}
	inodes := map[string]bool{}
	for _, e := range procs {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		if processPgid(pid) != pgid {
			continue
		}
		fds, err := os.ReadDir(fmt.Sprintf("/proc/%d/fd", pid))
		if err != nil {
			continue
		}
		for _, fd := range fds {
			link, err := os.Readlink(fmt.Sprintf("/proc/%d/fd/%s", pid, fd.Name()))
			if err == nil && strings.HasPrefix(link, "socket:[") {
				inodes[strings.TrimSuffix(strings.TrimPrefix(link, "socket:["), "]")] = true
			}
		}
	}
	return inodes
}

func processPgid(pid int) int {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return -1
	}
	s := string(b)
	i := strings.LastIndexByte(s, ')')
	if i < 0 {
		return -1
	}
	fields := strings.Fields(s[i+1:]) // state ppid pgrp ...
	if len(fields) < 3 {
		return -1
	}
	pgrp, err := strconv.Atoi(fields[2])
	if err != nil {
		return -1
	}
	return pgrp
}

func listenPortsForInodes(path string, inodes map[string]bool) []int {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var ports []int
	for _, line := range strings.Split(string(b), "\n")[1:] {
		f := strings.Fields(line)
		if len(f) < 10 || f[3] != "0A" { // 0A = TCP LISTEN
			continue
		}
		if !inodes[f[9]] {
			continue
		}
		local := f[1] // hex ip:port
		if i := strings.LastIndexByte(local, ':'); i >= 0 {
			if port, err := strconv.ParseInt(local[i+1:], 16, 32); err == nil {
				ports = append(ports, int(port))
			}
		}
	}
	return ports
}

// listenInodeForPort returns the socket inode of the process listening on a
// TCP port, or "" if none is.
func listenInodeForPort(port int) string {
	for _, f := range []string{"/proc/net/tcp", "/proc/net/tcp6"} {
		b, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(b), "\n")[1:] {
			ff := strings.Fields(line)
			if len(ff) < 10 || ff[3] != "0A" {
				continue
			}
			local := ff[1]
			i := strings.LastIndexByte(local, ':')
			if i < 0 {
				continue
			}
			if p, err := strconv.ParseInt(local[i+1:], 16, 32); err == nil && int(p) == port {
				return ff[9]
			}
		}
	}
	return ""
}

func pidForInode(inode string) int {
	procs, err := os.ReadDir("/proc")
	if err != nil {
		return 0
	}
	target := "socket:[" + inode + "]"
	for _, e := range procs {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		fds, err := os.ReadDir(fmt.Sprintf("/proc/%d/fd", pid))
		if err != nil {
			continue
		}
		for _, fd := range fds {
			if link, err := os.Readlink(fmt.Sprintf("/proc/%d/fd/%s", pid, fd.Name())); err == nil && link == target {
				return pid
			}
		}
	}
	return 0
}

func procCmdline(pid int) string {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
	if err != nil || len(b) == 0 {
		return "unknown"
	}
	parts := strings.Split(strings.TrimRight(string(b), "\x00"), "\x00")
	return clip(strings.Join(parts, " "), 60)
}

// procStartTime is field 22 of /proc/<pid>/stat (start time in clock ticks), a
// stable per-process token that distinguishes a recycled pid.
func procStartTime(pid int) string {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return ""
	}
	s := string(b)
	if i := strings.LastIndexByte(s, ')'); i >= 0 { // comm may contain spaces/parens
		fields := strings.Fields(s[i+1:])
		if len(fields) >= 20 { // state is fields[0]; starttime is the 22nd overall = fields[19]
			return fields[19]
		}
	}
	return ""
}
