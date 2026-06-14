// First-run scaffold: ~/.sesh is the global mod mount point, and a fresh
// install should explain itself. Every file ships inside the binary (one
// binary, no sidecars) and is written only if absent: an edited file is
// never overwritten, and truncating one to empty silences it for good.
package harness

import (
	"embed"
	"os"
	"path/filepath"
)

//go:embed scaffold
var scaffoldFS embed.FS

// scaffoldFiles maps embedded files to their ~/.sesh destinations. The
// .example files are inert by name: activating one is a deliberate rename by
// the user, never an installer side effect.
var scaffoldFiles = map[string]string{
	"scaffold/README.md":           "README.md",
	"scaffold/prompts-README.md":   filepath.Join("prompts", "README.md"),
	"scaffold/tools-README.md":     filepath.Join("tools", "README.md"),
	"scaffold/skills-README.md":    filepath.Join("skills", "README.md"),
	"scaffold/statusline.example":  "statusline.example",
	"scaffold/gate.example":        "gate.example",
	"scaffold/mcp.json.example":    "mcp.json.example",
	"scaffold/tuning.json.example": "tuning.json.example",
	// The report-issue skill ships active (not an .example): a public tool
	// should let any user file a well-formed issue out of the box.
	"scaffold/skills/report-issue/SKILL.md":                   filepath.Join("skills", "report-issue", "SKILL.md"),
	"scaffold/skills/report-issue/scripts/collect-context.sh": filepath.Join("skills", "report-issue", "scripts", "collect-context.sh"),
	"scaffold/skills/report-issue/references/templates.md":    filepath.Join("skills", "report-issue", "references", "templates.md"),
}

// scaffoldHome populates ~/.sesh on first run and fills gaps on later runs.
func scaffoldHome() {
	home := os.Getenv("HOME")
	if home == "" {
		return
	}
	root := filepath.Join(home, ".sesh")
	for _, d := range []string{"", "prompts", "tools", "skills", "sessions", "chains"} {
		os.MkdirAll(filepath.Join(root, d), 0o755)
	}
	for src, dst := range scaffoldFiles {
		path := filepath.Join(root, dst)
		if _, err := os.Stat(path); err == nil {
			continue // present, possibly edited: theirs, not ours
		}
		b, err := scaffoldFS.ReadFile(src)
		if err != nil {
			continue
		}
		os.MkdirAll(filepath.Dir(path), 0o755) // nested scaffold files (e.g. a shipped skill)
		os.WriteFile(path, b, 0o644)
	}
}
