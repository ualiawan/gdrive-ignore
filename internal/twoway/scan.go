package twoway

import (
	"os"
	"path/filepath"
	"strings"

	"gdrive-ignore/internal/ignore"
)

// tmpPrefix marks the engine's own temporary names (always renamed away
// within the same folder immediately).
const tmpPrefix = ".gdi-tmp-"

// skipName reports names that are never synced: the engine's temp names and
// Drive for Desktop's own staging folders.
func skipName(name string) bool {
	if strings.HasPrefix(name, tmpPrefix) {
		return true
	}
	switch strings.ToLower(name) {
	case ".tmp.driveupload", ".tmp.drivedownload":
		return true
	}
	return false
}

// node is one directory entry as listed by its parent folder.
type node struct {
	Path  string // real-case relative path
	Dir   bool
	Size  int64
	MTime int64 // unix nanoseconds
}

type tree struct {
	root    string
	nodes   map[string]node
	ignored map[string]bool // keys ignored by rules (pruned there)
	failed  map[string]bool // folders that could not be listed
	tmp     []string        // leftover temp names to clean up
}

// scan lists root, applying rules as it descends and skipping ignored
// folders entirely.
func scan(root string, loader *ignore.Loader) *tree {
	t := &tree{root: root, nodes: map[string]node{}, ignored: map[string]bool{}, failed: map[string]bool{}}
	set, err := loader.ForDir(loader.Base, "")
	if err != nil {
		t.failed[""] = true
		return t
	}
	t.walk("", set, loader)
	return t
}

func (t *tree) walk(relDir string, set *ignore.Set, loader *ignore.Loader) {
	entries, err := os.ReadDir(filepath.Join(t.root, filepath.FromSlash(relDir)))
	if err != nil {
		t.failed[key(relDir)] = true
		return
	}
	for _, de := range entries {
		name := de.Name()
		rel := join(relDir, name)
		if strings.HasPrefix(name, tmpPrefix) {
			t.tmp = append(t.tmp, rel)
			continue
		}
		if skipName(name) {
			continue
		}
		isDir := de.IsDir()
		if !isDir && !de.Type().IsRegular() {
			continue // symlinks, junctions, devices
		}
		if ign, _ := set.Match(rel, isDir); ign {
			t.ignored[key(rel)] = true
			continue
		}
		n := node{Path: rel, Dir: isDir}
		if info, err := de.Info(); err == nil {
			n.MTime = info.ModTime().UnixNano()
			if !isDir {
				n.Size = info.Size()
			}
		}
		t.nodes[key(rel)] = n
		if isDir {
			child, err := loader.ForDir(set, rel)
			if err != nil {
				t.failed[key(rel)] = true
				continue
			}
			t.walk(rel, child, loader)
		}
	}
}

func join(dir, name string) string {
	if dir == "" {
		return name
	}
	return dir + "/" + name
}

func parentKey(k string) string {
	if i := strings.LastIndexByte(k, '/'); i >= 0 {
		return k[:i]
	}
	return ""
}

// selfOrAncestor reports whether k or one of its ancestors is in set.
func selfOrAncestor(set map[string]bool, k string) bool {
	if len(set) == 0 {
		return false
	}
	for {
		if set[k] {
			return true
		}
		if k == "" {
			return false
		}
		k = parentKey(k)
	}
}

// underPrefix reports whether k equals p or lies below it.
func underPrefix(k, p string) bool {
	return p == "" || k == p || strings.HasPrefix(k, p+"/")
}
