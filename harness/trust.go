// Project trust: a project's .sesh/ can hold files that run code (gate,
// statusline), route credentials (a providers.json profile can repoint a URL
// while the stored key follows the profile name), or replace sesh's own
// instructions (SYSTEM.md, APPEND_SYSTEM.md, prompts/). In a repo you just
// cloned those are someone else's, so they take effect only once the user
// approves them, and only while their content still matches what was
// approved: an edit by anyone, the agent's own bash included, suspends them
// until the next startup asks again.
package harness

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// gatedProjectMods are the .sesh/ entries that need approval. Everything else
// in a project .sesh/ (skills, theme, tuning, mcp selection) is instruction or
// preference, the same trust class as AGENTS.md.
var gatedProjectMods = []string{"gate", "statusline", "providers.json", "SYSTEM.md", "APPEND_SYSTEM.md", "prompts"}

// approvedProjectMods is the content hash the user approved at startup; empty
// means nothing is approved. Set once in Main before any reader runs.
var approvedProjectMods string

// projectModsHash fingerprints the gated files present in ./.sesh: their
// paths, executable bits, and contents. Empty when there are none.
func projectModsHash() (hash string, found []string) {
	h := sha256.New()
	add := func(rel string) {
		p := filepath.Join(".sesh", rel)
		b, err := os.ReadFile(p)
		if err != nil {
			return
		}
		fi, _ := os.Stat(p)
		exec := fi != nil && fi.Mode()&0o111 != 0
		fmt.Fprintf(h, "%s\x00%v\x00%d\x00", rel, exec, len(b))
		h.Write(b)
		found = append(found, rel)
	}
	for _, rel := range gatedProjectMods {
		fi, err := os.Stat(filepath.Join(".sesh", rel))
		if err != nil {
			continue
		}
		if !fi.IsDir() {
			add(rel)
			continue
		}
		var files []string
		filepath.WalkDir(filepath.Join(".sesh", rel), func(p string, d fs.DirEntry, err error) error {
			if err == nil && !d.IsDir() {
				r, _ := filepath.Rel(".sesh", p)
				files = append(files, r)
			}
			return nil
		})
		sort.Strings(files)
		for _, f := range files {
			add(f)
		}
	}
	if len(found) == 0 {
		return "", nil
	}
	return hex.EncodeToString(h.Sum(nil)), found
}

// projectModsOK reports whether the gated project mods may be used right now:
// approved, and unchanged since.
func projectModsOK() bool {
	if approvedProjectMods == "" {
		return false
	}
	h, _ := projectModsHash()
	return h == approvedProjectMods
}

func trustPath() string { return filepath.Join(os.Getenv("HOME"), ".sesh", "trust.json") }

// trustRecord maps an absolute project directory to the approved hash.
type trustRecord struct {
	Projects map[string]string `json:"projects"`
}

func loadTrust() trustRecord {
	rec := trustRecord{Projects: map[string]string{}}
	if b, err := os.ReadFile(trustPath()); err == nil {
		json.Unmarshal(b, &rec)
		if rec.Projects == nil {
			rec.Projects = map[string]string{}
		}
	}
	return rec
}

func saveTrust(rec trustRecord) error {
	b, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(trustPath()), 0o700); err != nil {
		return err
	}
	tmp := trustPath() + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, trustPath())
}

// resolveProjectTrust settles approvedProjectMods at startup: a recorded
// approval of the current content holds; otherwise -trust or the user's yes
// (confirm, nil when no one can be asked) approves and records it. Anything
// else leaves the gated mods unused and returns a notice saying so.
func resolveProjectTrust(trustFlag bool, confirm func(found []string) bool) (notice string) {
	approvedProjectMods = ""
	hash, found := projectModsHash()
	if hash == "" {
		return ""
	}
	cwd, err := os.Getwd()
	if err != nil {
		return ""
	}
	if abs, err := filepath.EvalSymlinks(cwd); err == nil {
		cwd = abs
	}
	rec := loadTrust()
	if rec.Projects[cwd] == hash {
		approvedProjectMods = hash
		return ""
	}
	if trustFlag || (confirm != nil && confirm(found)) {
		approvedProjectMods = hash
		rec.Projects[cwd] = hash
		if err := saveTrust(rec); err != nil {
			return fmt.Sprintf("note: could not record project trust (%v); approved for this run only", err)
		}
		return ""
	}
	return fmt.Sprintf("note: ignoring this project's .sesh/ %s: not trusted (approve when asked at startup, or pass -trust)", strings.Join(found, ", "))
}

// askTrust is the interactive confirm: one line on stderr, the answer read a
// byte at a time so nothing meant for the input editor is swallowed.
func askTrust(found []string) bool {
	fmt.Fprintf(os.Stderr, "%sThis project's .sesh/ contains %s, which can run code, route your API keys, or replace sesh's own prompts.\nUse them? Approval is remembered until they change. [y/N] %s",
		yellow, strings.Join(found, ", "), reset)
	var line []byte
	b := make([]byte, 1)
	for {
		n, err := os.Stdin.Read(b)
		if n == 0 || err != nil || b[0] == '\n' {
			break
		}
		line = append(line, b[0])
	}
	ans := strings.TrimSpace(string(line))
	return ans == "y" || ans == "Y" || strings.EqualFold(ans, "yes")
}
