// Input history for the footer TUI, persisted per project: up/down at the
// prompt recalls what you typed in this directory before, across sessions.
// One JSON-encoded string per line, so multiline (pasted) entries survive.
// Files live under ~/.sesh/history/, which the agent's own read/search
// tools already refuse, since typed history can be sensitive.
package harness

import (
	"bufio"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"os"
	"path/filepath"
)

const historyKeep = 200 // entries retained per project

func historyPath() string {
	cwd, _ := os.Getwd()
	h := fnv.New32a()
	h.Write([]byte(cwd))
	return filepath.Join(os.Getenv("HOME"), ".sesh", "history", fmt.Sprintf("%08x.jsonl", h.Sum32()))
}

// loadHistory reads this project's input history, oldest first, trimming the
// file when it has grown well past the retention cap.
func loadHistory(path string) []string {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	var hist []string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		var line string
		if json.Unmarshal(sc.Bytes(), &line) == nil && line != "" {
			hist = append(hist, line)
		}
	}
	if len(hist) > historyKeep+100 {
		hist = hist[len(hist)-historyKeep:]
		rewriteHistory(path, hist)
	}
	return hist
}

// appendHistory adds one entry, skipping empties, immediate repeats, and
// session-ending commands: recalling "exit" with up-arrow only ever quits a
// session by accident.
func appendHistory(path string, hist []string, line string) []string {
	switch line {
	case "", "exit", "quit", "/exit", "/quit":
		return hist
	}
	if len(hist) > 0 && hist[len(hist)-1] == line {
		return hist
	}
	hist = append(hist, line)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return hist
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return hist
	}
	defer f.Close()
	if b, err := json.Marshal(line); err == nil {
		fmt.Fprintf(f, "%s\n", b)
	}
	return hist
}

func rewriteHistory(path string, hist []string) {
	f, err := os.OpenFile(path+".tmp", os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	for _, line := range hist {
		if b, err := json.Marshal(line); err == nil {
			fmt.Fprintf(f, "%s\n", b)
		}
	}
	f.Close()
	os.Rename(path+".tmp", path)
}
