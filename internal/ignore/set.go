package ignore

import (
	"errors"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// DefaultFileName is the per-directory ignore file, compatible with
// shilangyu/driveignore.
const DefaultFileName = ".driveignore"

// Set is an immutable, ordered list of rules. Later rules win.
type Set struct {
	rules []Rule
}

// NewSet builds a set from rule groups in increasing precedence.
func NewSet(groups ...[]Rule) *Set {
	s := &Set{}
	for _, g := range groups {
		s.rules = append(s.rules, g...)
	}
	return s
}

// Extend returns a new set with rules appended (higher precedence).
func (s *Set) Extend(rules []Rule) *Set {
	if len(rules) == 0 {
		return s
	}
	out := &Set{rules: make([]Rule, 0, len(s.rules)+len(rules))}
	out.rules = append(out.rules, s.rules...)
	out.rules = append(out.rules, rules...)
	return out
}

// Rules returns the rules in precedence order.
func (s *Set) Rules() []Rule { return s.rules }

// Match decides rel (relative to the pair root) on its own, assuming its
// parents are not ignored. It returns the deciding rule, or nil.
func (s *Set) Match(rel string, isDir bool) (ignored bool, rule *Rule) {
	segs := splitRel(fold(cleanRel(rel)))
	if len(segs) == 0 {
		return false, nil
	}
	for i := len(s.rules) - 1; i >= 0; i-- {
		r := &s.rules[i]
		if r.Match(segs, isDir) {
			return !r.negate, r
		}
	}
	return false, nil
}

// Loader reads nested ignore files while walking a source tree.
type Loader struct {
	Root      string   // absolute source root
	FileNames []string // ignore file names to honor, e.g. .driveignore, .gitignore
	Base      *Set     // global + pair rules
}

// ForDir returns parent extended with the ignore files found in relDir.
func (l *Loader) ForDir(parent *Set, relDir string) (*Set, error) {
	var rules []Rule
	dir := filepath.Join(l.Root, filepath.FromSlash(relDir))
	for _, name := range l.FileNames {
		p := filepath.Join(dir, name)
		b, err := os.ReadFile(p)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return parent, err
		}
		rules = append(rules, Parse(string(b), p, relDir)...)
	}
	return parent.Extend(rules), nil
}

// Result explains why a path is or isn't ignored.
type Result struct {
	Ignored bool
	Rule    *Rule  // deciding rule, nil if none matched
	By      string // the path (itself or an ancestor) the rule matched
}

// Check decides rel the way a full walk would: an ignored ancestor ignores
// everything below it.
func (l *Loader) Check(rel string, isDir bool) (Result, error) {
	rel = cleanRel(rel)
	set := l.Base
	var err error
	if set, err = l.ForDir(set, ""); err != nil {
		return Result{}, err
	}
	parts := splitRel(rel)
	for i := range parts {
		cur := strings.Join(parts[:i+1], "/")
		last := i == len(parts)-1
		dir := !last || isDir
		ignored, r := set.Match(cur, dir)
		if ignored {
			return Result{Ignored: true, Rule: r, By: cur}, nil
		}
		if last {
			return Result{Rule: r, By: cur}, nil
		}
		if set, err = l.ForDir(set, cur); err != nil {
			return Result{}, err
		}
	}
	return Result{}, nil
}

// IsIgnoreFile reports whether name is one of the loader's ignore files.
func (l *Loader) IsIgnoreFile(name string) bool {
	name = path.Base(filepath.ToSlash(name))
	for _, n := range l.FileNames {
		if strings.EqualFold(n, name) {
			return true
		}
	}
	return false
}
