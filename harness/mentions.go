// Inline mentions in the input line: "#name" references an installed skill,
// "@path" references a file in the working tree. Both tab-complete, both render
// highlighted once recognized, and an @path normalizes to its working-directory
// relative path on the next space. Mentions are a soft cue: the recognized text
// rides along in the message, and the model loads the skill or reads the file
// with its own tools. Nothing here injects content or forces a load.
//
// Two sigils, two namespaces, kept disjoint from the existing grammar: "/"
// starts commands, "#" starts skills, "@" starts files.
package harness

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// mentionWalkCap bounds the tree walk that resolves a bare @name to a unique
// path, so a huge repo can never stall a keystroke. Past it, resolution gives
// up and the token is left as typed.
const mentionWalkCap = 20000

// mentions is everything the input editor needs to recognize, complete, and
// highlight #skill and @file mentions: the installed skill names, the working
// directory that @paths resolve against, its gitignore rules, and the SGR color
// for a recognized mention ("" disables highlighting, e.g. under NO_COLOR).
type mentions struct {
	skills map[string]bool
	root   string
	ignore []ignoreRule
	sgr    string
}

// newMentions snapshots the installed skills (project-over-global, already
// deduped by loadSkills) and the working directory. Rebuilt on /reload so newly
// added skills become recognizable.
func newMentions(sgr string) *mentions {
	root, _ := os.Getwd()
	skills := map[string]bool{}
	entries, _ := loadSkills()
	for _, e := range entries {
		skills[e.name] = true
	}
	return &mentions{skills: skills, root: root, ignore: loadGitignore(root), sgr: sgr}
}

// complete returns the candidate tokens a Tab would extend the cursor token to:
// matching skill names for "#", working-tree paths for "@".
func (m *mentions) complete(token string) []string {
	if token == "" {
		return nil
	}
	switch token[0] {
	case '#':
		pre := token[1:]
		var out []string
		for name := range m.skills {
			if strings.HasPrefix(name, pre) {
				out = append(out, "#"+name)
			}
		}
		sort.Strings(out)
		return out
	case '@':
		return m.completePath(token[1:])
	}
	return nil
}

// completePath lists the entries of the partial path's directory whose name
// extends the last segment, confined to the working directory. Directories come
// back with a trailing slash so completion can drill in.
func (m *mentions) completePath(partial string) []string {
	dir, seg := "", partial
	if i := strings.LastIndex(partial, "/"); i >= 0 {
		dir, seg = partial[:i+1], partial[i+1:]
	}
	clean := filepath.Clean(dir)
	if filepath.IsAbs(dir) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return nil
	}
	entries, err := os.ReadDir(filepath.Join(m.root, dir))
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		name := e.Name()
		if !strings.HasPrefix(name, seg) {
			continue
		}
		rel := filepath.ToSlash(filepath.Join(dir, name))
		if m.skip(rel, name, e.IsDir()) {
			continue
		}
		tok := "@" + rel
		if e.IsDir() {
			tok += "/"
		}
		out = append(out, tok)
	}
	sort.Strings(out)
	return out
}

// resolve normalizes an @token to "@<relpath>": an existing path is cleaned in
// place, otherwise a bare name that matches exactly one tree file is rewritten
// to that file's relative path. ok is false (token unchanged) when the name is
// unknown or matches more than one file, so an ambiguous mention stays as typed
// and unhighlighted rather than silently picking one.
func (m *mentions) resolve(token string) (string, bool) {
	if len(token) < 2 || token[0] != '@' {
		return token, false
	}
	text := token[1:]
	if rel, ok := m.existsRel(text); ok {
		return "@" + rel, true
	}
	if rel, ok := m.walkUnique(text); ok {
		return "@" + rel, true
	}
	return token, false
}

// ok reports whether a token names something real, for highlighting. It stays
// cheap enough to run on every redraw: a map lookup for #skills, a single stat
// for @files (the tree walk that resolves bare names runs only on space).
func (m *mentions) ok(token string) bool {
	if len(token) < 2 {
		return false
	}
	name := token[1:]
	switch token[0] {
	case '#':
		return m.skills[name]
	case '@':
		_, ok := m.existsRel(name)
		return ok
	}
	return false
}

// spans returns the [start,end) rune ranges of every recognized mention in buf,
// for highlighting.
func (m *mentions) spans(buf []rune) [][2]int {
	var out [][2]int
	for i := 0; i < len(buf); {
		for i < len(buf) && wordBreak(buf[i]) {
			i++
		}
		start := i
		for i < len(buf) && !wordBreak(buf[i]) {
			i++
		}
		if i > start {
			if c := buf[start]; (c == '#' || c == '@') && m.ok(string(buf[start:i])) {
				out = append(out, [2]int{start, i})
			}
		}
	}
	return out
}

// existsRel cleans text to a working-directory relative path and reports whether
// it points at a real file or directory. Escapes (absolute paths, "..") and the
// ~/.sesh directory are refused, matching the file tools' boundary.
func (m *mentions) existsRel(text string) (string, bool) {
	if text == "" {
		return "", false
	}
	clean := filepath.Clean(text)
	if filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", false
	}
	abs := filepath.Join(m.root, clean)
	if m.underSesh(abs) {
		return "", false
	}
	if _, err := os.Stat(abs); err != nil {
		return "", false
	}
	return filepath.ToSlash(clean), true
}

// walkUnique finds the single tree file whose basename or path suffix is text,
// honoring gitignore and the usual skips. More than one match, or none, returns
// false: ambiguity is never guessed.
func (m *mentions) walkUnique(text string) (string, bool) {
	var found string
	count, visited := 0, 0
	filepath.WalkDir(m.root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if visited++; visited > mentionWalkCap {
			return filepath.SkipAll
		}
		rel, _ := filepath.Rel(m.root, p)
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			if rel != "." && m.skip(rel, d.Name(), true) {
				return filepath.SkipDir
			}
			return nil
		}
		if m.skip(rel, d.Name(), false) {
			return nil
		}
		if rel == text || filepath.Base(rel) == text || strings.HasSuffix(rel, "/"+text) {
			found = rel
			if count++; count > 1 {
				return filepath.SkipAll
			}
		}
		return nil
	})
	if count == 1 {
		return found, true
	}
	return "", false
}

// skip applies the same exclusions as the search tool: the heavy build dirs,
// gitignored paths, and the credential-bearing ~/.sesh directory.
func (m *mentions) skip(rel, name string, isDir bool) bool {
	if isDir {
		switch name {
		case ".git", "node_modules", "dist":
			return true
		}
	}
	if ignoredBy(m.ignore, rel, isDir) {
		return true
	}
	return m.underSesh(filepath.Join(m.root, rel))
}

func (m *mentions) underSesh(abs string) bool {
	hd, err := filepath.Abs(seshDir())
	if err != nil {
		return false
	}
	return abs == hd || strings.HasPrefix(abs, hd+string(os.PathSeparator))
}

// mentionToken returns the token ending at pos: the run of non-break runes
// whose first rune is a mention sigil. ok is false when the cursor is not at the
// end of such a token.
func mentionToken(buf []rune, pos int) (start int, token string, ok bool) {
	if pos == 0 {
		return 0, "", false
	}
	start = pos
	for start > 0 && !wordBreak(buf[start-1]) {
		start--
	}
	if start >= pos {
		return 0, "", false
	}
	if c := buf[start]; c != '#' && c != '@' {
		return 0, "", false
	}
	return start, string(buf[start:pos]), true
}

func wordBreak(r rune) bool { return r == ' ' || r == '\n' || r == '\t' }
