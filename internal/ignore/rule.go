// Package ignore implements gitignore-style rules for deciding which files
// are mirrored to a Google Drive synced folder.
package ignore

import (
	"runtime"
	"strings"
)

// FoldCase makes matching case-insensitive. Windows and macOS file systems
// are case-insensitive by default, so rules are too.
var FoldCase = runtime.GOOS == "windows" || runtime.GOOS == "darwin"

// Rule is a single parsed ignore pattern.
type Rule struct {
	Text   string // original line, trimmed
	Source string // "global", "pair" or the path of the ignore file
	Line   int    // 1-based line number within Source
	Base   string // directory the rule is relative to, "/"-separated, "" = root

	negate   bool
	dirOnly  bool
	anchored bool     // pattern contains a slash, so it matches from Base
	segs     []string // pattern split on "/", case-folded when FoldCase
	base     []string // Base split on "/", case-folded when FoldCase
}

// Negated reports whether the rule re-includes paths ("!pattern").
func (r *Rule) Negated() bool { return r.negate }

// Parse parses ignore file content. Blank lines and comments are skipped.
func Parse(text, source, base string) []Rule {
	var rules []Rule
	for i, line := range strings.Split(text, "\n") {
		if r, ok := ParseLine(line, source, base, i+1); ok {
			rules = append(rules, r)
		}
	}
	return rules
}

// ParseLine parses one pattern. ok is false for blank lines and comments.
func ParseLine(line, source, base string, lineNo int) (r Rule, ok bool) {
	line = strings.TrimSuffix(line, "\r")
	line = trimTrailingSpaces(line)
	if line == "" || strings.HasPrefix(line, "#") {
		return Rule{}, false
	}
	r = Rule{Text: line, Source: source, Line: lineNo, Base: cleanRel(base)}

	p := line
	switch {
	case strings.HasPrefix(p, "!"):
		r.negate = true
		p = p[1:]
	case strings.HasPrefix(p, `\!`), strings.HasPrefix(p, `\#`):
		p = p[1:]
	}
	p = normalizeSeparators(p)
	if strings.HasSuffix(p, "/") {
		r.dirOnly = true
		p = strings.TrimRight(p, "/")
	}
	if p == "" {
		return Rule{}, false
	}
	if strings.Contains(p, "/") {
		r.anchored = true
		p = strings.TrimPrefix(p, "/")
	}
	if FoldCase {
		p = strings.ToLower(p)
	}
	r.segs = strings.Split(p, "/")
	r.base = splitRel(fold(r.Base))
	return r, true
}

// Match reports whether the rule matches rel ("/"-separated, relative to the
// pair root). It only looks at rel itself, not its parents.
func (r *Rule) Match(rel []string, isDir bool) bool {
	if r.dirOnly && !isDir {
		return false
	}
	if len(rel) <= len(r.base) {
		return false
	}
	for i, b := range r.base {
		if rel[i] != b {
			return false
		}
	}
	rel = rel[len(r.base):]
	if !r.anchored {
		return matchSegment(r.segs[0], rel[len(rel)-1])
	}
	return matchSegments(r.segs, rel)
}

// trimTrailingSpaces removes trailing spaces unless escaped with a backslash.
func trimTrailingSpaces(s string) string {
	for strings.HasSuffix(s, " ") && !strings.HasSuffix(s, `\ `) {
		s = s[:len(s)-1]
	}
	return s
}

// normalizeSeparators turns Windows-style backslash separators into "/",
// keeping backslashes that escape a glob metacharacter.
func normalizeSeparators(p string) string {
	if !strings.Contains(p, `\`) {
		return p
	}
	var b strings.Builder
	for i := 0; i < len(p); i++ {
		c := p[i]
		if c == '\\' {
			if i+1 < len(p) && strings.IndexByte(`*?[]\!# `, p[i+1]) >= 0 {
				b.WriteByte(c)
				b.WriteByte(p[i+1])
				i++
				continue
			}
			b.WriteByte('/')
			continue
		}
		b.WriteByte(c)
	}
	return b.String()
}

func cleanRel(p string) string {
	p = strings.ReplaceAll(p, `\`, "/")
	return strings.Trim(p, "/")
}

func splitRel(p string) []string {
	if p == "" {
		return nil
	}
	return strings.Split(p, "/")
}

func fold(s string) string {
	if FoldCase {
		return strings.ToLower(s)
	}
	return s
}

// matchSegments matches pattern segments against path segments, with "**"
// matching zero or more whole segments.
func matchSegments(pat, path []string) bool {
	for len(pat) > 0 {
		if pat[0] == "**" {
			if len(pat) == 1 {
				// "dir/**" matches everything inside dir, not dir itself.
				return len(path) > 0
			}
			for i := 0; i <= len(path); i++ {
				if matchSegments(pat[1:], path[i:]) {
					return true
				}
			}
			return false
		}
		if len(path) == 0 || !matchSegment(pat[0], path[0]) {
			return false
		}
		pat, path = pat[1:], path[1:]
	}
	return len(path) == 0
}

// matchSegment matches a single path segment against a glob supporting
// "*", "?", "[...]" (with "!" or "^" negation and ranges) and "\" escapes.
func matchSegment(pat, name string) bool {
	p := []rune(pat)
	n := []rune(name)
	pi, ni := 0, 0
	starP, starN := -1, -1
	for ni < len(n) {
		if pi < len(p) {
			switch p[pi] {
			case '*':
				for pi < len(p) && p[pi] == '*' {
					pi++
				}
				starP, starN = pi, ni
				continue
			case '?':
				pi++
				ni++
				continue
			case '[':
				if ok, next, valid := matchClass(p, pi, n[ni]); valid {
					if ok {
						pi = next
						ni++
						continue
					}
				} else if n[ni] == '[' {
					pi++
					ni++
					continue
				}
			case '\\':
				if pi+1 < len(p) && p[pi+1] == n[ni] {
					pi += 2
					ni++
					continue
				}
			default:
				if p[pi] == n[ni] {
					pi++
					ni++
					continue
				}
			}
		}
		if starP >= 0 {
			starN++
			pi, ni = starP, starN
			continue
		}
		return false
	}
	for pi < len(p) && p[pi] == '*' {
		pi++
	}
	return pi == len(p)
}

// matchClass matches c against the bracket expression starting at p[start].
// valid is false when the bracket is not closed (then "[" is literal).
func matchClass(p []rune, start int, c rune) (ok bool, next int, valid bool) {
	i := start + 1
	negate := false
	if i < len(p) && (p[i] == '!' || p[i] == '^') {
		negate = true
		i++
	}
	first := true
	for i < len(p) {
		if p[i] == ']' && !first {
			return ok != negate, i + 1, true
		}
		first = false
		lo := p[i]
		if lo == '\\' && i+1 < len(p) {
			i++
			lo = p[i]
		}
		hi := lo
		if i+2 < len(p) && p[i+1] == '-' && p[i+2] != ']' {
			hi = p[i+2]
			if hi == '\\' && i+3 < len(p) {
				i++
				hi = p[i+2]
			}
			i += 2
		}
		if lo <= c && c <= hi {
			ok = true
		}
		i++
	}
	return false, 0, false
}
