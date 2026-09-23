package mirror

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gdrive-ignore/internal/ignore"
)

// Validate refuses pairs that could damage data: overlapping folders, a
// missing source, or a drive root as target.
func Validate(source, target string) error {
	if !filepath.IsAbs(source) || !filepath.IsAbs(target) {
		return errors.New("source and target must be absolute paths")
	}
	fi, err := os.Stat(source)
	if err != nil {
		return fmt.Errorf("source folder: %w", err)
	}
	if !fi.IsDir() {
		return errors.New("source is not a folder")
	}
	t := filepath.Clean(target)
	if filepath.Dir(t) == t {
		return errors.New("target cannot be a drive root")
	}
	if fi, err := os.Stat(t); err == nil && !fi.IsDir() {
		return errors.New("target exists and is not a folder")
	}
	s := realPath(source)
	rt := realPath(t)
	if samePath(s, rt) {
		return errors.New("source and target are the same folder")
	}
	if isWithin(rt, s) {
		return errors.New("target cannot be inside the source folder")
	}
	if isWithin(s, rt) {
		return errors.New("source cannot be inside the target folder")
	}
	return nil
}

// realPath resolves symlinks and junctions in the existing part of p.
func realPath(p string) string {
	p = filepath.Clean(p)
	existing := nearestExisting(p)
	resolved, err := filepath.EvalSymlinks(existing)
	if err != nil {
		return p
	}
	rest, _ := filepath.Rel(existing, p)
	return filepath.Join(resolved, rest)
}

func samePath(a, b string) bool {
	if ignore.FoldCase {
		return strings.EqualFold(a, b)
	}
	return a == b
}

// isWithin reports whether p is strictly inside dir.
func isWithin(p, dir string) bool {
	rel, err := filepath.Rel(dir, p)
	if err != nil {
		return false
	}
	if ignore.FoldCase {
		// filepath.Rel is case-sensitive; compare folded copies.
		rel, err = filepath.Rel(strings.ToLower(dir), strings.ToLower(p))
		if err != nil {
			return false
		}
	}
	return rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}
