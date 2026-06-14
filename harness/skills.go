// Skills engine: progressive disclosure for instructions. A skill is a
// directory holding a SKILL.md (agentskills.io format: YAML frontmatter with
// name and description, then markdown instructions, plus optional bundled
// files). The manifest (one "name: description" line per skill) rides in the
// skill tool's description, so the model always knows what exists and loads a
// body only when a task calls for it.
//
// Mounts: .sesh/skills/ (project, same trust class as AGENTS.md, tagged
// [project]) over ~/.sesh/skills/ (global). Project shadows global by name.
// A skill that violates the spec does not load; the violation is a note.
// Scanning is cheap (measured ~2ms per 200 skills), so the scan runs live at
// startup with no cache. The skill tool is also the sanctioned door through
// the "read tools refuse ~/.sesh" boundary; the boundary itself stays intact.
package harness

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"sesh/agent"
)

type skillEntry struct {
	name, desc, dir string
	project         bool
}

var skillName = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`)

// loadSkills scans both mounts, validates strictly, and resolves shadowing.
// The returned order is deterministic: project skills first, then global,
// each lexically sorted, so the manifest never shifts between runs.
func loadSkills() ([]skillEntry, []string) {
	var entries []skillEntry
	var notes []string
	seen := map[string]bool{}
	mounts := []struct {
		dir     string
		project bool
	}{
		{filepath.Join(".sesh", "skills"), true},
		{filepath.Join(os.Getenv("HOME"), ".sesh", "skills"), false},
	}
	for _, m := range mounts {
		dirs, err := os.ReadDir(m.dir)
		if err != nil {
			continue
		}
		var batch []skillEntry
		for _, d := range dirs {
			if !d.IsDir() {
				continue
			}
			dir := filepath.Join(m.dir, d.Name())
			e, err := loadSkill(dir, d.Name())
			if err != nil {
				notes = append(notes, fmt.Sprintf("skill %s: %v; excluded", dir, err))
				continue
			}
			if seen[e.name] {
				continue // project shadows global, first mount wins
			}
			e.project = m.project
			batch = append(batch, e)
		}
		sort.Slice(batch, func(i, j int) bool { return batch[i].name < batch[j].name })
		for _, e := range batch {
			seen[e.name] = true
		}
		entries = append(entries, batch...)
	}
	return entries, notes
}

// loadSkill reads and validates one skill directory against the spec:
// SKILL.md present, frontmatter parseable, name matching the directory and
// the charset rule, description present and bounded.
func loadSkill(dir, dirname string) (skillEntry, error) {
	b, err := os.ReadFile(filepath.Join(dir, "SKILL.md"))
	if err != nil {
		return skillEntry{}, fmt.Errorf("no SKILL.md")
	}
	name, desc, err := parseFrontmatter(string(b))
	if err != nil {
		return skillEntry{}, err
	}
	if len(name) > 64 || !skillName.MatchString(name) {
		return skillEntry{}, fmt.Errorf("invalid name %q (max 64 chars of [a-z0-9-], no edge hyphens)", name)
	}
	if name != dirname {
		return skillEntry{}, fmt.Errorf("name %q does not match directory %q", name, dirname)
	}
	if desc == "" || len(desc) > 1024 {
		return skillEntry{}, fmt.Errorf("description missing or over 1024 chars")
	}
	return skillEntry{name: name, desc: desc, dir: dir}, nil
}

// parseFrontmatter extracts name and description from the YAML frontmatter.
// It speaks the subset real skills use: plain scalars, quoted scalars, and
// folded/literal block scalars (">" and "|") with indented continuations.
// A full YAML parser would be a dependency; this is the measured 95% case.
func parseFrontmatter(s string) (name, desc string, err error) {
	if !strings.HasPrefix(s, "---\n") {
		return "", "", fmt.Errorf("SKILL.md must open with YAML frontmatter (--- line)")
	}
	rest := s[4:]
	end := strings.Index(rest, "\n---")
	if end < 0 {
		return "", "", fmt.Errorf("frontmatter never closes (missing --- line)")
	}
	lines := strings.Split(rest[:end], "\n")
	for i := 0; i < len(lines); i++ {
		key, val, found := strings.Cut(lines[i], ":")
		if !found || strings.HasPrefix(lines[i], " ") || strings.HasPrefix(lines[i], "\t") {
			continue
		}
		val = strings.TrimSpace(val)
		if val == ">" || val == "|" || val == ">-" || val == "|-" {
			var parts []string
			for i+1 < len(lines) && (strings.HasPrefix(lines[i+1], " ") || lines[i+1] == "") {
				i++
				parts = append(parts, strings.TrimSpace(lines[i]))
			}
			val = strings.TrimSpace(strings.Join(parts, " "))
		} else {
			val = strings.Trim(val, `"'`)
		}
		switch strings.TrimSpace(key) {
		case "name":
			name = val
		case "description":
			desc = val
		}
	}
	if name == "" {
		return "", "", fmt.Errorf("frontmatter has no name")
	}
	return name, desc, nil
}

// skillManifest renders the model-facing description: a preamble teaching the
// loading discipline, then one line per skill. Overflow past the tuning cap
// is loud: the model is told how many skills it cannot see and why.
func skillManifest(entries []skillEntry) string {
	var b strings.Builder
	b.WriteString("Load a skill: focused instructions for a specific kind of task. " +
		"Each line below is one installed skill as \"name: when to use it\". " +
		"When a task matches a description, call this tool with that name BEFORE " +
		"attempting the task, then follow the returned instructions. Pass file to " +
		"fetch a bundled resource the instructions reference. Entries tagged " +
		"[project] come from this project's .sesh/skills.\n\nSkills:\n")
	max := tune.SkillManifestMax
	shown := entries
	if len(shown) > max {
		shown = shown[:max]
	}
	for _, e := range shown {
		tag := ""
		if e.project {
			tag = " [project]"
		}
		fmt.Fprintf(&b, "- %s%s: %s\n", e.name, tag, e.desc)
	}
	if n := len(entries) - len(shown); n > 0 {
		fmt.Fprintf(&b, "(%d more skills omitted: the skill_manifest_max dial is %d; raise it in tuning.json)\n", n, max)
	}
	return b.String()
}

// skillTool builds the engine's tool. ok is false when no skills are
// installed: the tool then stays out of the toolset entirely, costing zero
// tokens, which is the whole point of progressive disclosure.
func skillTool() (agent.Tool, []string, bool) {
	entries, notes := loadSkills()
	if len(entries) == 0 {
		return agent.Tool{}, notes, false
	}
	byName := map[string]skillEntry{}
	var names []string
	for _, e := range entries {
		byName[e.name] = e
		names = append(names, e.name)
	}
	return agent.Tool{
		Def: agent.ToolDef{
			Name:        "skill",
			Description: skillManifest(entries),
			Schema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"name": map[string]any{"type": "string", "description": "The skill to load, exactly as named in the manifest."},
					"file": map[string]any{"type": "string", "description": "Optional bundled file within the skill (e.g. references/details.md). Omit to load the skill's instructions."},
				},
				"required": []string{"name"},
			},
		},
		Run: func(_ context.Context, raw json.RawMessage) (string, bool) {
			return runSkill(byName, names, raw)
		},
		Parallel: true, // pure observation
	}, notes, true
}

func runSkill(byName map[string]skillEntry, names []string, raw json.RawMessage) (string, bool) {
	var in struct{ Name, File string }
	if err := json.Unmarshal(raw, &in); err != nil {
		return "invalid tool input: " + err.Error(), true
	}
	e, ok := byName[in.Name]
	if !ok {
		return fmt.Sprintf("no skill named %q; installed skills: %s", in.Name, strings.Join(names, ", ")), true
	}
	if in.File != "" {
		p, err := confineSkillPath(e.dir, in.File)
		if err != nil {
			return err.Error(), true
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return fmt.Sprintf("skill %s has no file %q; load the skill without file to see its bundled files", e.name, in.File), true
		}
		return string(b), false
	}
	b, err := os.ReadFile(filepath.Join(e.dir, "SKILL.md"))
	if err != nil {
		return "skill vanished since startup: " + err.Error(), true
	}
	out := string(b)
	if files := bundledFiles(e.dir); len(files) > 0 {
		out += "\n\nBundled files (fetch with the file parameter):\n- " + strings.Join(files, "\n- ")
	}
	return out, false
}

// confineSkillPath keeps bundled-file access inside the skill directory:
// lexical confinement (clean, relative, no upward escape), mirroring the
// boundary discipline of the file tools.
func confineSkillPath(dir, file string) (string, error) {
	if filepath.IsAbs(file) {
		return "", fmt.Errorf("file must be a relative path within the skill")
	}
	clean := filepath.Clean(file)
	if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("file must stay within the skill directory")
	}
	return filepath.Join(dir, clean), nil
}

func bundledFiles(dir string) []string {
	var files []string
	filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		rel, _ := filepath.Rel(dir, path)
		if rel != "SKILL.md" {
			files = append(files, rel)
		}
		return nil
	})
	sort.Strings(files)
	return files
}
