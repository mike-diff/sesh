package harness

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeSkill drops a spec-shaped skill into root (a mount's skills dir).
func writeSkill(t *testing.T, root, name, desc, body string) {
	t.Helper()
	dir := filepath.Join(root, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	md := fmt.Sprintf("---\nname: %s\ndescription: %s\n---\n%s", name, desc, body)
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(md), 0o644); err != nil {
		t.Fatal(err)
	}
}

// skillMounts isolates both mounts in temp dirs and returns them.
func skillMounts(t *testing.T) (project, global string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	work := t.TempDir()
	old, _ := os.Getwd()
	if err := os.Chdir(work); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chdir(old) })
	return filepath.Join(work, ".sesh", "skills"), filepath.Join(home, ".sesh", "skills")
}

// Breaker: delete any single validation branch in loadSkill and the matching
// row here fails.
func TestSkillValidationRejectsSpecViolations(t *testing.T) {
	_, global := skillMounts(t)
	long := strings.Repeat("a", 65)
	writeSkill(t, global, long, "too long a name", "x")
	writeSkill(t, global, "Bad_Name", "bad charset", "x")
	writeSkill(t, global, "edge-", "edge hyphen", "x")
	writeSkill(t, global, "nodesc", "", "x")
	writeSkill(t, global, "hugedesc", strings.Repeat("d", 1025), "x")
	os.MkdirAll(filepath.Join(global, "empty"), 0o755) // no SKILL.md at all
	mismatch := filepath.Join(global, "mismatch")
	os.MkdirAll(mismatch, 0o755)
	os.WriteFile(filepath.Join(mismatch, "SKILL.md"), []byte("---\nname: other\ndescription: dir does not match\n---\nx"), 0o644)
	writeSkill(t, global, "good-skill", "the one valid skill", "body")

	entries, notes := loadSkills()
	if len(entries) != 1 || entries[0].name != "good-skill" {
		t.Fatalf("want exactly good-skill to load, got %+v", entries)
	}
	if len(notes) != 7 {
		t.Fatalf("want 7 exclusion notes naming each violation, got %d: %v", len(notes), notes)
	}
}

// Breaker: drop the ".." rejection in confineSkillPath.
func TestSkillBundledFileConfinement(t *testing.T) {
	_, global := skillMounts(t)
	writeSkill(t, global, "confined", "a skill with files", "see references/x.md")
	os.MkdirAll(filepath.Join(global, "confined", "references"), 0o755)
	os.WriteFile(filepath.Join(global, "confined", "references", "x.md"), []byte("inside"), 0o644)
	os.WriteFile(filepath.Join(filepath.Dir(global), "credentials.json"), []byte("SECRET"), 0o600)

	tool, _, ok := skillTool()
	if !ok {
		t.Fatal("skill tool did not activate")
	}
	for _, escape := range []string{"../credentials.json", "../../credentials.json", "/etc/passwd", "references/../../credentials.json"} {
		raw, _ := json.Marshal(map[string]string{"name": "confined", "file": escape})
		out, isErr := tool.Run(nil, raw)
		if !isErr || strings.Contains(out, "SECRET") {
			t.Fatalf("traversal %q escaped: %q", escape, out)
		}
	}
	raw, _ := json.Marshal(map[string]string{"name": "confined", "file": "references/x.md"})
	out, isErr := tool.Run(nil, raw)
	if isErr || out != "inside" {
		t.Fatalf("legitimate bundled file failed: %q err=%v", out, isErr)
	}
}

// Breaker: render the manifest by ranging over the byName map.
func TestSkillManifestDeterministic(t *testing.T) {
	project, global := skillMounts(t)
	for i := 0; i < 5; i++ {
		writeSkill(t, global, fmt.Sprintf("g-%d", i), "global skill", "x")
		writeSkill(t, project, fmt.Sprintf("p-%d", i), "project skill", "x")
	}
	entries, _ := loadSkills()
	first := skillManifest(entries)
	for i := 0; i < 10; i++ {
		entries, _ := loadSkills()
		if got := skillManifest(entries); got != first {
			t.Fatalf("manifest shifted between identical runs:\n%s\nvs\n%s", first, got)
		}
	}
	if !strings.Contains(first, "p-0 [project]") {
		t.Fatalf("project skills must be tagged [project]:\n%s", first)
	}
}

// Breaker: scan the global mount before the project mount in loadSkills.
func TestProjectSkillShadowsGlobal(t *testing.T) {
	project, global := skillMounts(t)
	writeSkill(t, global, "same-name", "the global one", "GLOBAL BODY")
	writeSkill(t, project, "same-name", "the project one", "PROJECT BODY")

	tool, _, ok := skillTool()
	if !ok {
		t.Fatal("skill tool did not activate")
	}
	if d := tool.Def.Description; strings.Contains(d, "the global one") || !strings.Contains(d, "the project one") {
		t.Fatalf("manifest must show the project skill only:\n%s", d)
	}
	raw, _ := json.Marshal(map[string]string{"name": "same-name"})
	out, _ := tool.Run(nil, raw)
	if !strings.Contains(out, "PROJECT BODY") {
		t.Fatalf("project skill must shadow global, got: %q", out)
	}
}

// Breaker: drop the omitted-count line and truncate silently.
func TestSkillManifestOverflowLoud(t *testing.T) {
	_, global := skillMounts(t)
	for i := 0; i < 3; i++ {
		writeSkill(t, global, fmt.Sprintf("s-%d", i), "a skill", "x")
	}
	saved := tune.SkillManifestMax
	tune.SkillManifestMax = 2
	defer func() { tune.SkillManifestMax = saved }()

	entries, _ := loadSkills()
	m := skillManifest(entries)
	if !strings.Contains(m, "1 more skills omitted") || !strings.Contains(m, "skill_manifest_max") {
		t.Fatalf("overflow must be loud and name the dial:\n%s", m)
	}
}

// Breaker: register the skill tool unconditionally.
func TestNoSkillsMeansNoTool(t *testing.T) {
	skillMounts(t)
	if _, _, ok := skillTool(); ok {
		t.Fatal("skill tool must stay out of the toolset when no skills exist")
	}
}

// Breaker: append the skills system note unconditionally in engineTools.
func TestEngineSystemNoteOnlyWithSkills(t *testing.T) {
	_, global := skillMounts(t)
	if _, _, note := engineTools(); note != "" {
		t.Fatalf("no skills installed must mean no system note, got %q", note)
	}
	writeSkill(t, global, "one-skill", "a skill", "x")
	_, _, note := engineTools()
	if !strings.Contains(note, "<skills>") || !strings.Contains(note, "manifest") {
		t.Fatalf("installed skills must add the skills discipline note, got %q", note)
	}

	// Breaker: ignore SkillNoteOff and emit the note anyway. The note is
	// weak-model scaffolding; a strong-model user reclaims its tokens.
	saved := tune.SkillNoteOff
	tune.SkillNoteOff = true
	defer func() { tune.SkillNoteOff = saved }()
	if _, _, note := engineTools(); note != "" {
		t.Fatalf("skill_note_off must drop the note, got %q", note)
	}
}

// Breaker: parse only plain scalars in parseFrontmatter.
func TestSkillFrontmatterSpeaksBlockScalars(t *testing.T) {
	_, global := skillMounts(t)
	dir := filepath.Join(global, "folded")
	os.MkdirAll(dir, 0o755)
	md := "---\nname: folded\ndescription: >\n  Use when the user has tabular data\n  and wants charts.\nmetadata:\n  author: someone\n---\nbody"
	os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(md), 0o644)

	entries, notes := loadSkills()
	if len(entries) != 1 {
		t.Fatalf("folded-description skill must load, notes: %v", notes)
	}
	want := "Use when the user has tabular data and wants charts."
	if entries[0].desc != want {
		t.Fatalf("folded description parsed wrong: %q", entries[0].desc)
	}
}
