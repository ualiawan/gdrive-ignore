package twoway

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"gdrive-ignore/internal/ignore"
)

// entry is the state of one path at the last successful sync. Sizes and
// mtimes are kept per side because with hardlinks each name's directory
// entry is only refreshed when written through that name.
type entry struct {
	Path   string `json:"p"`            // real-case relative path, "/"-separated
	Dir    bool   `json:"d,omitempty"`  //
	ID     uint64 `json:"id,omitempty"` // file: shared file ID; dir: the mirror folder's ID
	SSize  int64  `json:"ss,omitempty"` // source directory entry
	SMTime int64  `json:"sm,omitempty"`
	MSize  int64  `json:"ms,omitempty"` // mirror directory entry
	MMTime int64  `json:"mm,omitempty"`
	Drive  int64  `json:"g,omitempty"` // Drive item id, 0 if unknown
}

// Pending is a deletion the tool could not attribute safely; it waits for
// the user to decide. While pending, the path is left completely alone.
type Pending struct {
	Path    string    `json:"path"`
	Dir     bool      `json:"dir"`
	Since   time.Time `json:"since"`
	Reason  string    `json:"reason"`
	Resolve string    `json:"resolve,omitempty"` // "", "delete" or "restore"
}

// Decision values for Engine.Decide.
const (
	DecideDelete  = "delete"  // delete the source copy (to the Recycle Bin)
	DecideRestore = "restore" // put it back into the mirror (and Drive)
)

type stateFile struct {
	Version int        `json:"version"`
	Source  string     `json:"source"`
	Target  string     `json:"target"`
	Entries []entry    `json:"entries"`
	Pending []*Pending `json:"pending,omitempty"`
}

type state struct {
	path    string
	entries map[string]entry
	pending map[string]*Pending
	dirty   bool
}

func key(rel string) string {
	if ignore.FoldCase {
		return strings.ToLower(rel)
	}
	return rel
}

func loadState(path string) (*state, bool, error) {
	s := &state{path: path, entries: map[string]entry{}, pending: map[string]*Pending{}}
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return s, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	var f stateFile
	if err := json.Unmarshal(b, &f); err != nil {
		return nil, true, err
	}
	for _, e := range f.Entries {
		s.entries[key(e.Path)] = e
	}
	for _, p := range f.Pending {
		s.pending[key(p.Path)] = p
	}
	return s, true, nil
}

func (s *state) set(e entry) {
	s.entries[key(e.Path)] = e
	s.dirty = true
}

func (s *state) del(k string) {
	if _, ok := s.entries[k]; ok {
		delete(s.entries, k)
		s.dirty = true
	}
}

// delTree removes k and everything below it.
func (s *state) delTree(k string) {
	s.del(k)
	for ck := range s.entries {
		if strings.HasPrefix(ck, k+"/") {
			delete(s.entries, ck)
			s.dirty = true
		}
	}
}

// under returns entries strictly below k.
func (s *state) under(k string) []entry {
	var out []entry
	for ck, e := range s.entries {
		if strings.HasPrefix(ck, k+"/") {
			out = append(out, e)
		}
	}
	return out
}

// move renames k (and its subtree) to newPath.
func (s *state) move(k, newPath string) {
	nk := key(newPath)
	for ck, e := range s.entries {
		if ck == k || strings.HasPrefix(ck, k+"/") {
			delete(s.entries, ck)
			e.Path = newPath + e.Path[len(k):]
			s.entries[nk+ck[len(k):]] = e
		}
	}
	s.dirty = true
}

func (s *state) setPending(p *Pending) {
	s.pending[key(p.Path)] = p
	s.dirty = true
}

func (s *state) clearPending(k string) {
	if _, ok := s.pending[k]; ok {
		delete(s.pending, k)
		s.dirty = true
	}
}

func (s *state) save(source, target string) error {
	if !s.dirty {
		return nil
	}
	f := stateFile{Version: 2, Source: source, Target: target}
	for _, e := range s.entries {
		f.Entries = append(f.Entries, e)
	}
	sort.Slice(f.Entries, func(i, j int) bool { return f.Entries[i].Path < f.Entries[j].Path })
	for _, p := range s.pending {
		f.Pending = append(f.Pending, p)
	}
	sort.Slice(f.Pending, func(i, j int) bool { return f.Pending[i].Path < f.Pending[j].Path })
	b, err := json.Marshal(f)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, s.path); err != nil {
		return err
	}
	s.dirty = false
	return nil
}
