package mirror

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gdrive-ignore/internal/ignore"
)

// manifest records every target entry the engine created, so it only ever
// deletes things it owns. Keys are case-folded on case-insensitive systems.
type manifest struct {
	path    string
	entries map[string]manifestEntry
	dirty   bool
}

type manifestEntry struct {
	Path string `json:"p"`           // relative path with its real case, "/"-separated
	Dir  bool   `json:"d,omitempty"` // entry is a directory
}

type manifestFile struct {
	Version int             `json:"version"`
	Source  string          `json:"source"`
	Target  string          `json:"target"`
	Entries []manifestEntry `json:"entries"`
}

func key(rel string) string {
	if ignore.FoldCase {
		return strings.ToLower(rel)
	}
	return rel
}

// loadManifest reads the manifest. exists is false when there is none yet.
func loadManifest(path string) (m *manifest, exists bool, err error) {
	m = &manifest{path: path, entries: map[string]manifestEntry{}}
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return m, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	var f manifestFile
	if err := json.Unmarshal(b, &f); err != nil {
		return nil, true, err
	}
	for _, e := range f.Entries {
		m.entries[key(e.Path)] = e
	}
	return m, true, nil
}

func (m *manifest) add(rel string, dir bool) {
	k := key(rel)
	if e, ok := m.entries[k]; ok && e.Path == rel && e.Dir == dir {
		return
	}
	m.entries[k] = manifestEntry{Path: rel, Dir: dir}
	m.dirty = true
}

func (m *manifest) remove(rel string) {
	k := key(rel)
	if _, ok := m.entries[k]; ok {
		delete(m.entries, k)
		m.dirty = true
	}
}

func (m *manifest) get(rel string) (manifestEntry, bool) {
	e, ok := m.entries[key(rel)]
	return e, ok
}

// under returns entries strictly below rel ("" = everything).
func (m *manifest) under(rel string) []manifestEntry {
	prefix := ""
	if rel != "" {
		prefix = key(rel) + "/"
	}
	var out []manifestEntry
	for k, e := range m.entries {
		if strings.HasPrefix(k, prefix) {
			out = append(out, e)
		}
	}
	return out
}

func (m *manifest) save(source, target string) error {
	if !m.dirty {
		return nil
	}
	f := manifestFile{Version: 1, Source: source, Target: target}
	for _, e := range m.entries {
		f.Entries = append(f.Entries, e)
	}
	sort.Slice(f.Entries, func(i, j int) bool { return f.Entries[i].Path < f.Entries[j].Path })
	b, err := json.Marshal(f)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(m.path), 0o755); err != nil {
		return err
	}
	tmp := m.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, m.path); err != nil {
		return err
	}
	m.dirty = false
	return nil
}
