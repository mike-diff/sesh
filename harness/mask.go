// Secret masking for model-facing tool output. bash output is where keys
// actually leak: `env`, `cat .env`, a git remote printed with its embedded
// token. The README's standing answer was that stored keys are encrypted at
// rest and "bash remains a hole"; this narrows the hole at the point where the
// bytes would leave for the provider.
//
// The masking is one-way and heuristic, and both halves are deliberate. No
// recovery mechanism exists because none is needed: the model never requires
// the true value, only to know that a value was there. Heuristic because a
// perfect secret detector is not a real thing; the patterns below are the
// shapes secrets actually take in command output, chosen so that ordinary
// output survives untouched.
package harness

import "regexp"

// secretKeyRe matches an assignment's KEY: optionally after `export `, an
// identifier, then `=`. The value's shape is matched separately so quoting can
// be preserved around the mask (API_KEY="[redacted]", not API_KEY=[redacted]).
var secretKeyRe = regexp.MustCompile(`(?:^|[\s;(&|])export\s+([A-Za-z_][A-Za-z0-9_]*)=|(^|[\s;(&|])([A-Za-z_][A-Za-z0-9_]*)=`)

// sensitiveKey reports whether an assignment key names something secret. The
// compound spellings (password, api_key, private_key, access_key) match as
// substrings, case-insensitive, so MY_API_KEY and Db_Password both hit. The
// bare words token and secret must be a whole underscore segment instead:
// GITHUB_TOKEN matches while tokenizer_path does not. The segment rule exists
// because substring matching alone eats tokenizer_path, and a masked
// tokenizer path is an agent working blind.
func sensitiveKey(key string) bool {
	k := lowerASCII(key)
	for _, needle := range []string{
		"password", "passwd", "api_key", "apikey", "private_key", "access_key",
	} {
		if contains(k, needle) {
			return true
		}
	}
	for _, seg := range splitOn(k, '_') {
		if seg == "token" || seg == "secret" {
			return true
		}
	}
	return false
}

func splitOn(s string, sep byte) []string {
	var out []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == sep {
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	return append(out, s[start:])
}
func lowerASCII(s string) string {
	b := []byte(s)
	for i, c := range b {
		if c >= 'A' && c <= 'Z' {
			b[i] = c + ('a' - 'A')
		}
	}
	return string(b)
}

func contains(s, sub string) bool {
	return len(sub) == 0 || indexOf(s, sub) >= 0
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

// tokenShapeRe matches well-known credential token shapes wherever they
// appear, not only in assignments: a key pasted bare into output still leaks.
// The anchors (prefix plus a following credential-character run) keep ordinary
// prose from tripping them: "task-force" is not sk- plus credential chars.
var tokenShapeRe = regexp.MustCompile(
	`(?:sk-ant-|sk-proj-|sk-)[A-Za-z0-9_-]{12,}` +
		`|ghp_[A-Za-z0-9]{20,}|gho_[A-Za-z0-9]{20,}|ghu_[A-Za-z0-9]{20,}|ghr_[A-Za-z0-9]{20,}|github_pat_[A-Za-z0-9_]{20,}` +
		`|AKIA[0-9A-Z]{16}` +
		`|xox[abprs]-[A-Za-z0-9-]{10,}`)

const redacted = "[redacted]"

// maskSecrets replaces secret values and credential tokens in s with
// [redacted], leaving the surrounding structure (the key name, the quotes, the
// line) intact so the output stays readable and the model can still see that a
// secret was present and where.
func maskSecrets(s string) string {
	if s == "" {
		return s
	}
	s = maskAssignments(s)
	s = tokenShapeRe.ReplaceAllString(s, redacted)
	return s
}

// maskAssignments walks the assignment matches and masks each sensitive key's
// value. The value is whatever follows the `=`: a double-quoted, single-quoted,
// or bare (up to whitespace) span. The mask replaces the value's interior so
// quoting survives verbatim.
func maskAssignments(s string) string {
	var out []byte
	last := 0
	for _, loc := range secretKeyRe.FindAllSubmatchIndex([]byte(s), -1) {
		// group 1/2/3 hold the key depending on whether `export ` preceded it
		keyStart, keyEnd := loc[2], loc[3]
		if keyStart < 0 {
			keyStart, keyEnd = loc[6], loc[7]
		}
		key := s[keyStart:keyEnd]
		if !sensitiveKey(key) {
			continue
		}
		eq := loc[1] - 1 // the match ends at the '=', so it is the byte before
		if eq < 0 || s[eq] != '=' {
			continue
		}
		maskStart, maskEnd, ok := maskSpan(s, eq+1)
		if !ok {
			continue
		}
		out = append(out, s[last:maskStart]...)
		out = append(out, []byte(redacted)...)
		last = maskEnd
	}
	if last == 0 {
		return s
	}
	return string(append(out, s[last:]...))
}

// maskSpan returns the half-open byte range whose replacement hides the value
// starting at i. For a quoted value the range is the interior only, so the
// quotes survive around the mask (API_KEY="[redacted]"); an unterminated
// quote masks to the end of input. A bare value runs to the next whitespace.
// ok is false when nothing maskable follows (end of input, or `KEY=`).
func maskSpan(s string, i int) (start, end int, ok bool) {
	if i >= len(s) {
		return 0, 0, false
	}
	switch s[i] {
	case '"', '\'':
		quote := s[i]
		for j := i + 1; j < len(s); j++ {
			if s[j] == quote {
				return i + 1, j, true
			}
		}
		return i + 1, len(s), true // unterminated quote: mask to the end
	}
	j := i
	for j < len(s) && !isSpaceByte(s[j]) {
		j++
	}
	if j == i {
		return 0, 0, false // `KEY=` with no value: nothing to mask
	}
	return i, j, true
}

func isSpaceByte(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\r'
}
