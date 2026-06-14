// The search tool: shaped for token economy, because the evidence says shape
// is the whole game. Read-type output is ~76% of an agent's tokens, raw grep
// output is ~98% waste, and a capped search that suppresses oversized results
// with guidance measurably beats both verbose paging and silent truncation
// (measured by the search bench). So:
//
//   - smart-case: a pattern with no uppercase matches case-insensitively, an
//     uppercase pattern is exact (the convention every ripgrep user knows)
//   - .gitignore-aware: a documented subset of the root .gitignore (plus the
//     hardcoded .git/node_modules/dist and ~/.sesh skips) keeps junk out
//   - grouped output: hits grouped under one path header instead of repeating
//     the path on every line
//   - over the cap, results are SUPPRESSED, never silently truncated: the
//     model gets match and file counts, the densest files, and an instruction
//     to narrow, because keep-first-N measurably keeps the wrong lines
package harness

import (
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"unicode"
)

const (
	searchCapLines  = 50   // matched lines shown before the summary takes over
	searchCountStop = 2000 // stop counting matches here; report "2000+"
	searchLineClip  = 200  // a matched line longer than this is clipped
	searchTopFiles  = 20   // files listed in the over-cap summary
)

// smartCaseMatcher returns a line matcher: case-insensitive when the pattern
// has no uppercase, exact otherwise.
func smartCaseMatcher(pattern string) func(string) bool {
	if strings.ContainsFunc(pattern, unicode.IsUpper) {
		return func(s string) bool { return strings.Contains(s, pattern) }
	}
	low := strings.ToLower(pattern)
	return func(s string) bool { return strings.Contains(strings.ToLower(s), low) }
}

// ---------------------------------------------------------------------------
// .gitignore, the useful subset: blank lines and comments skipped, `dir/`
// matches directories, a leading `/` anchors to the root, everything else
// matches the basename or any path segment, `*` globs work within a segment.
// Negation (`!`) and `**` are NOT supported; a negated file is simply not
// searched, which costs a miss in search results, never a wrong edit.
// ---------------------------------------------------------------------------

type ignoreRule struct {
	pattern  string
	dirOnly  bool
	anchored bool
}

func loadGitignore(root string) []ignoreRule {
	b, err := os.ReadFile(filepath.Join(root, ".gitignore"))
	if err != nil {
		return nil
	}
	var rules []ignoreRule
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "!") {
			continue
		}
		r := ignoreRule{pattern: line}
		if strings.HasSuffix(r.pattern, "/") {
			r.dirOnly = true
			r.pattern = strings.TrimSuffix(r.pattern, "/")
		}
		if strings.HasPrefix(r.pattern, "/") {
			r.anchored = true
			r.pattern = strings.TrimPrefix(r.pattern, "/")
		}
		if r.pattern != "" {
			rules = append(rules, r)
		}
	}
	return rules
}

// ignoredBy reports whether rel (a slash-separated path relative to the root)
// is matched by any rule. isDir gates dir-only rules; matching a directory
// prunes its whole subtree at the walk.
func ignoredBy(rules []ignoreRule, rel string, isDir bool) bool {
	base := path.Base(rel)
	for _, r := range rules {
		if r.dirOnly && !isDir {
			continue
		}
		if r.anchored {
			if ok, _ := path.Match(r.pattern, rel); ok {
				return true
			}
			if strings.HasPrefix(rel, r.pattern+"/") {
				return true
			}
			continue
		}
		if ok, _ := path.Match(r.pattern, base); ok {
			return true
		}
		for _, seg := range strings.Split(rel, "/") {
			if ok, _ := path.Match(r.pattern, seg); ok {
				return true
			}
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// The walk and the two output shapes.
// ---------------------------------------------------------------------------

type fileMatches struct {
	rel   string
	lines []string // "  NN: text", collected only while under the cap
	count int
}

func doSearch(pattern string) (string, bool) {
	match := smartCaseMatcher(pattern)
	root, _ := os.Getwd()
	rules := loadGitignore(root)
	hd, _ := filepath.Abs(seshDir())
	hdPrefix := hd + string(os.PathSeparator)

	var files []*fileMatches
	totalMatches, shownLines := 0, 0

	filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		rel, _ := filepath.Rel(root, p)
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			switch d.Name() {
			case ".git", "node_modules", "dist":
				return filepath.SkipDir
			}
			if p == hd || strings.HasPrefix(p, hdPrefix) {
				return filepath.SkipDir // never surface the key/credentials
			}
			if rel != "." && ignoredBy(rules, rel, true) {
				return filepath.SkipDir
			}
			return nil
		}
		if ignoredBy(rules, rel, false) {
			return nil
		}
		// Skip big files: keeps memory bounded and filters most binaries.
		if info, err := d.Info(); err != nil || info.Size() > 1<<20 {
			return nil
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return nil
		}
		var fm *fileMatches
		for i, line := range strings.Split(string(b), "\n") {
			if !match(line) {
				continue
			}
			if fm == nil {
				fm = &fileMatches{rel: rel}
				files = append(files, fm)
			}
			fm.count++
			totalMatches++
			if shownLines < searchCapLines {
				text := strings.TrimSpace(line)
				if len(text) > searchLineClip {
					text = text[:searchLineClip] + "..."
				}
				fm.lines = append(fm.lines, fmt.Sprintf("  %d: %s", i+1, text))
				shownLines++
			}
			if totalMatches >= searchCountStop {
				return filepath.SkipAll
			}
		}
		return nil
	})

	if totalMatches == 0 {
		return "no matches", false
	}

	countStr := fmt.Sprintf("%d", totalMatches)
	if totalMatches >= searchCountStop {
		countStr = fmt.Sprintf("%d+", searchCountStop)
	}

	var b strings.Builder
	if totalMatches > searchCapLines {
		// Suppress, summarize, guide: never silently keep the first N.
		fmt.Fprintf(&b, "%s matches in %d files: too many to show. Narrow the pattern (a longer substring or a rarer identifier) and search again.\n", countStr, len(files))
		sort.Slice(files, func(i, j int) bool { return files[i].count > files[j].count })
		fmt.Fprintf(&b, "densest files:\n")
		for i, fm := range files {
			if i >= searchTopFiles {
				fmt.Fprintf(&b, "  ... (%d more files)\n", len(files)-searchTopFiles)
				break
			}
			fmt.Fprintf(&b, "  %4d  %s\n", fm.count, fm.rel)
		}
		return strings.TrimRight(b.String(), "\n"), false
	}

	fmt.Fprintf(&b, "%s matches in %d files\n", countStr, len(files))
	for _, fm := range files {
		fmt.Fprintf(&b, "%s\n", fm.rel)
		for _, l := range fm.lines {
			fmt.Fprintf(&b, "%s\n", l)
		}
	}
	return strings.TrimRight(b.String(), "\n"), false
}
