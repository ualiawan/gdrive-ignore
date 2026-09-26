// Package twoway keeps a source folder and a hardlinked mirror folder (which
// Google Drive for Desktop syncs) in two-way sync, with gitignore-style rules.
//
// Behavior matches Drive's own two-way sync between the source and Drive:
// adds, edits and deletes flow both ways. Deletions that start by hand in
// the mirror never reach the source: the engine asks a Classifier where each
// mirror-side deletion came from and only deletes the source copy (to the
// Recycle Bin) when Drive deleted it first. Anything uncertain is left for
// the user to decide.
package twoway

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"gdrive-ignore/internal/ignore"
)

// Origin says where a mirror-side deletion started.
type Origin int

const (
	OriginUnknown Origin = iota // cannot tell: leave it for the user
	OriginWait                  // evidence not complete yet: decide later
	OriginDrive                 // deleted in Drive first: delete the source copy
	OriginMirror                // deleted by hand in the mirror: restore it
)

// Deletion describes a synced entry that disappeared from the mirror.
type Deletion struct {
	Path     string
	Dir      bool
	Gone     time.Time // when it was seen disappearing; zero if unknown
	DriveIDs []int64   // Drive item ids of the entry (and files below a folder)
}

// Classifier decides where a mirror-side deletion came from.
type Classifier interface {
	Classify(Deletion) Origin
}

// ErrTargetNotEmpty is returned on the first sync into a non-empty target
// unless Options.Adopt is set.
var ErrTargetNotEmpty = errors.New("target folder is not empty; confirm merging it first")

// Options configures an Engine.
type Options struct {
	Source, Target string
	StatePath      string
	IgnoreFiles    []string
	Rules          *ignore.Set
	Adopt          bool // allow the first sync to merge into a non-empty target
	Log            *slog.Logger

	Classifier    Classifier                       // nil: every mirror-side deletion needs a decision
	Recycle       func(path string) error          // moves a source entry to the Recycle Bin (required)
	DriveIDs      func() (map[uint64]int64, error) // file ID → Drive item id (optional)
	MaxAutoDelete int                              // source files deleted per pass without asking (default 100)
	LegacyState   string                           // a v1 manifest; if it exists the target counts as adopted
	Now           func() time.Time                 // for tests
}

// Stats summarizes one pass.
type Stats struct {
	Files, Dirs    int
	Bytes          int64
	Ignored        int
	Pushed         int // source → mirror (new or changed)
	Pulled         int // mirror → source (new or changed)
	Touched        int // in-place edits signalled to Drive
	Renamed        int
	Conflicts      int
	DeletedInDrive int // mirror entries removed because they were deleted in the source
	DeletedHere    int // source entries moved to the Recycle Bin because they were deleted in Drive
	Restored       int // mirror entries put back after a deletion by hand in the mirror
	Pending        int // deletions waiting for the user
	NextCheck      time.Time
	ErrorCount     int
	Errors         []string
	Duration       time.Duration
}

func (s *Stats) addErr(err error) {
	s.ErrorCount++
	if len(s.Errors) < 20 {
		s.Errors = append(s.Errors, err.Error())
	}
}

// Changed reports whether the pass changed anything on disk.
func (s *Stats) Changed() bool {
	return s.Pushed+s.Pulled+s.Touched+s.Renamed+s.Conflicts+s.DeletedInDrive+s.DeletedHere+s.Restored > 0
}

// Engine syncs one pair. It is safe for concurrent use.
type Engine struct {
	opt       Options
	srcLoader *ignore.Loader
	dstLoader *ignore.Loader
	log       *slog.Logger

	mu       sync.Mutex
	st       *state
	fresh    bool
	nonEmpty bool

	goneMu sync.Mutex
	gone   map[string]time.Time

	ignMu       sync.RWMutex
	ignoredDirs map[string]bool
}

// New validates the pair and loads its state.
func New(opt Options) (*Engine, error) {
	if err := Validate(opt.Source, opt.Target); err != nil {
		return nil, err
	}
	if !hardlinkCapable(opt.Source, opt.Target) {
		return nil, errors.New("the target must be on the same NTFS drive as the source (hardlinks are required)")
	}
	if opt.Recycle == nil {
		return nil, errors.New("a Recycle Bin function is required")
	}
	if opt.Rules == nil {
		opt.Rules = ignore.NewSet()
	}
	if len(opt.IgnoreFiles) == 0 {
		opt.IgnoreFiles = []string{ignore.DefaultFileName}
	}
	if opt.Log == nil {
		opt.Log = slog.Default()
	}
	if opt.MaxAutoDelete <= 0 {
		opt.MaxAutoDelete = 100
	}
	if opt.Now == nil {
		opt.Now = time.Now
	}
	st, exists, err := loadState(opt.StatePath)
	if err != nil {
		return nil, fmt.Errorf("reading sync state: %w", err)
	}
	e := &Engine{
		opt:         opt,
		srcLoader:   &ignore.Loader{Root: opt.Source, FileNames: opt.IgnoreFiles, Base: opt.Rules},
		dstLoader:   &ignore.Loader{Root: opt.Target, FileNames: opt.IgnoreFiles, Base: opt.Rules},
		log:         opt.Log,
		st:          st,
		fresh:       !exists,
		gone:        map[string]time.Time{},
		ignoredDirs: map[string]bool{},
	}
	if e.fresh {
		st.dirty = true
		if opt.LegacyState != "" {
			if _, err := os.Stat(opt.LegacyState); err == nil {
				e.opt.Adopt = true
			}
		}
		entries, err := os.ReadDir(opt.Target)
		e.nonEmpty = err == nil && len(entries) > 0
	}
	return e, nil
}

// NeedsAdopt reports whether the first sync would be refused.
func (e *Engine) NeedsAdopt() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.fresh && e.nonEmpty && !e.opt.Adopt
}

// Loader exposes the source-side rules (for explaining single paths).
func (e *Engine) Loader() *ignore.Loader { return e.srcLoader }

// NoteGone records that a mirror entry was seen disappearing at t.
func (e *Engine) NoteGone(rel string, t time.Time) {
	k := key(cleanRel(rel))
	e.goneMu.Lock()
	defer e.goneMu.Unlock()
	if _, ok := e.gone[k]; !ok {
		e.gone[k] = t
	}
	cut := t.Add(-time.Hour)
	for gk, gt := range e.gone {
		if gt.Before(cut) {
			delete(e.gone, gk)
		}
	}
}

// goneTime returns when k (or its nearest ancestor) was seen disappearing.
func (e *Engine) goneTime(k string) time.Time {
	e.goneMu.Lock()
	defer e.goneMu.Unlock()
	for {
		if t, ok := e.gone[k]; ok {
			return t
		}
		if k == "" {
			return time.Time{}
		}
		k = parentKey(k)
	}
}

func (e *Engine) forgetGone(k string) {
	e.goneMu.Lock()
	defer e.goneMu.Unlock()
	for gk := range e.gone {
		if underPrefix(gk, k) {
			delete(e.gone, gk)
		}
	}
}

// InIgnoredDir reports whether rel lies strictly inside an ignored folder.
func (e *Engine) InIgnoredDir(rel string) bool {
	k := key(cleanRel(rel))
	e.ignMu.RLock()
	defer e.ignMu.RUnlock()
	return k != "" && selfOrAncestor(e.ignoredDirs, parentKey(k)) && len(e.ignoredDirs) > 0
}

// IsIgnoredDir reports whether rel itself is an ignored folder.
func (e *Engine) IsIgnoredDir(rel string) bool {
	e.ignMu.RLock()
	defer e.ignMu.RUnlock()
	return e.ignoredDirs[key(cleanRel(rel))]
}

// Pending returns deletions waiting for a decision.
func (e *Engine) Pending() []Pending {
	e.mu.Lock()
	defer e.mu.Unlock()
	var out []Pending
	for _, p := range e.st.pending {
		out = append(out, *p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

// Decide resolves a pending deletion ("" rel = all of them). The decision is
// applied on the next Reconcile.
func (e *Engine) Decide(rel, decision string) error {
	if decision != DecideDelete && decision != DecideRestore {
		return errors.New("unknown decision")
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if rel == "" {
		for _, p := range e.st.pending {
			p.Resolve = decision
		}
		e.st.dirty = true
		return nil
	}
	p, ok := e.st.pending[key(cleanRel(rel))]
	if !ok {
		return errors.New("no pending decision for this path")
	}
	p.Resolve = decision
	e.st.dirty = true
	return e.st.save(e.opt.Source, e.opt.Target)
}

// Reconcile runs one full two-way pass.
func (e *Engine) Reconcile() (Stats, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	start := time.Now()
	if e.fresh && e.nonEmpty && !e.opt.Adopt {
		return Stats{}, ErrTargetNotEmpty
	}
	if err := os.MkdirAll(e.opt.Target, 0o755); err != nil {
		return Stats{}, err
	}
	r := &run{e: e, ids: map[string]uint64{}, skip: map[string]bool{}, restoring: map[string]bool{},
		partial: map[string]bool{}, mirrorDel: map[string]bool{}}
	if e.opt.DriveIDs != nil {
		if m, err := e.opt.DriveIDs(); err == nil {
			r.drive = m
		}
	}
	var S, M *tree
	for i := 0; ; i++ {
		S, M = scan(e.opt.Source, e.srcLoader), scan(e.opt.Target, e.dstLoader)
		if S.failed[""] {
			return r.stats, fmt.Errorf("cannot read the source folder %s", e.opt.Source)
		}
		if M.failed[""] {
			return r.stats, fmt.Errorf("cannot read the mirror folder %s", e.opt.Target)
		}
		if i == 3 || r.renames(S, M) == 0 {
			break
		}
		r.ids = map[string]uint64{}
	}
	r.cleanTemp(S)
	r.cleanTemp(M)
	r.sync(S, M)
	r.stats.Pending = len(e.st.pending)
	if err := e.st.save(e.opt.Source, e.opt.Target); err != nil {
		r.stats.addErr(fmt.Errorf("saving sync state: %w", err))
	} else {
		e.fresh = false
	}
	r.stats.Duration = time.Since(start)
	return r.stats, nil
}

type side int

const (
	src side = iota
	dst
)

type run struct {
	e     *Engine
	stats Stats
	drive map[uint64]int64
	ids   map[string]uint64 // "s:"/"m:" + key → file ID cache

	skip      map[string]bool // subtrees handled or left alone this pass
	restoring map[string]bool // mirror subtrees being restored from the source
	partial   map[string]bool // Drive-deleted source folders with local changes inside (value: user-confirmed)
	mirrorDel map[string]bool // mirror folders being removed (deleted in source)
	dirDels   []string        // mirror folders to remove once emptied
	deleted   int             // source files deleted this pass
}

func (r *run) root(sd side) string {
	if sd == src {
		return r.e.opt.Source
	}
	return r.e.opt.Target
}

func (r *run) abs(sd side, rel string) string {
	return filepath.Join(r.root(sd), filepath.FromSlash(rel))
}

func (r *run) id(sd side, rel string) (uint64, error) {
	c := "s:"
	if sd == dst {
		c = "m:"
	}
	c += key(rel)
	if v, ok := r.ids[c]; ok {
		return v, nil
	}
	v, err := fileID(r.abs(sd, rel))
	if err == nil {
		r.ids[c] = v
	}
	return v, err
}

func (r *run) cleanTemp(t *tree) {
	for _, rel := range t.tmp {
		_ = removeFile(filepath.Join(t.root, filepath.FromSlash(rel)))
	}
}

// excluded reports paths ignored on either side (never synced either way).
func excluded(S, M *tree, k string) bool {
	return selfOrAncestor(S.ignored, k) || selfOrAncestor(M.ignored, k)
}

func (r *run) skipped(k string) bool {
	for p := range r.skip {
		if underPrefix(k, p) {
			return true
		}
	}
	return false
}

func underAny(set map[string]bool, k string) (string, bool) {
	for p := range set {
		if underPrefix(k, p) {
			return p, true
		}
	}
	return "", false
}

// ---------- renames ----------

// renames detects entries renamed or moved on one side (same file ID under
// a new path) and applies the rename on the other side. It returns how many
// were applied; the caller rescans afterwards.
func (r *run) renames(S, M *tree) int {
	st := r.e.st
	n := 0
	for _, sides := range [][2]side{{src, dst}, {dst, src}} {
		x, y := sides[0], sides[1] // renamed on x, apply on y
		X, Y := S, M
		if x == dst {
			X, Y = M, S
		}
		// New files on x (not synced before, not on y) by file ID and Drive id.
		newByID := map[uint64]string{}
		newByDrive := map[int64]string{}
		newDirs := map[string]bool{}
		for k, nd := range X.nodes {
			if _, synced := st.entries[k]; synced || excluded(S, M, k) {
				continue
			}
			if _, onY := Y.nodes[k]; onY {
				continue
			}
			if nd.Dir {
				newDirs[k] = true
				continue
			}
			id, err := r.id(x, nd.Path)
			if err != nil {
				continue
			}
			newByID[id] = nd.Path
			if x == dst && r.drive != nil {
				if g, ok := r.drive[id]; ok {
					newByDrive[g] = nd.Path
				}
			}
		}
		if len(newByID) == 0 {
			continue
		}
		// Folder renames first (shallowest first), voting by the files inside.
		var missingDirs []entry
		for k, b := range st.entries {
			if b.Dir && !excluded(S, M, k) {
				if _, onX := X.nodes[k]; !onX {
					if yn, onY := Y.nodes[k]; onY && yn.Dir {
						missingDirs = append(missingDirs, b)
					}
				}
			}
		}
		sort.Slice(missingDirs, func(i, j int) bool {
			return strings.Count(missingDirs[i].Path, "/") < strings.Count(missingDirs[j].Path, "/")
		})
		for _, d := range missingDirs {
			dk := key(d.Path)
			if _, still := st.entries[dk]; !still {
				continue
			}
			votes := map[string]int{}
			for _, c := range st.under(dk) {
				if c.Dir {
					continue
				}
				p2, ok := newByID[c.ID]
				if !ok {
					continue
				}
				suffix := c.Path[len(d.Path):] // "/rest"
				if strings.HasSuffix(key(p2), key(suffix)) {
					votes[p2[:len(p2)-len(suffix)]]++
				}
			}
			best, bestN := "", 0
			for d2, v := range votes {
				if v > bestN && newDirs[key(d2)] {
					best, bestN = d2, v
				}
			}
			if best == "" {
				continue
			}
			if _, exists := Y.nodes[key(best)]; exists {
				continue
			}
			if err := r.renameOn(y, d.Path, best); err != nil {
				r.stats.addErr(err)
				continue
			}
			st.move(dk, best)
			r.stats.Renamed++
			n++
		}
		// File renames.
		for k, b := range st.entries {
			if b.Dir || excluded(S, M, k) {
				continue
			}
			if _, onX := X.nodes[k]; onX {
				continue
			}
			if yn, onY := Y.nodes[k]; !onY || yn.Dir {
				continue
			}
			p2, ok := newByID[b.ID]
			if !ok && x == dst && b.Drive != 0 {
				p2, ok = newByDrive[b.Drive]
			}
			if !ok {
				continue
			}
			if _, exists := Y.nodes[key(p2)]; exists {
				continue
			}
			if err := r.renameOn(y, b.Path, p2); err != nil {
				r.stats.addErr(err)
				continue
			}
			st.move(k, p2)
			r.stats.Renamed++
			n++
		}
	}
	return n
}

func (r *run) renameOn(sd side, from, to string) error {
	dstPath := r.abs(sd, to)
	if err := os.MkdirAll(filepath.Dir(dstPath), 0o755); err != nil {
		return err
	}
	return os.Rename(r.abs(sd, from), dstPath)
}

// ---------- main pass ----------

func (r *run) sync(S, M *tree) {
	e, st := r.e, r.e.st
	// Remember ignored folders on both sides for cheap event filtering.
	ign := map[string]bool{}
	for k := range S.ignored {
		ign[k] = true
	}
	for k := range M.ignored {
		ign[k] = true
	}
	r.stats.Ignored = len(S.ignored)
	e.ignMu.Lock()
	e.ignoredDirs = ign
	e.ignMu.Unlock()

	keys := map[string]bool{}
	for k := range S.nodes {
		keys[k] = true
	}
	for k := range M.nodes {
		keys[k] = true
	}
	for k := range st.entries {
		if excluded(S, M, k) {
			// Newly ignored: stop syncing it, touch nothing.
			st.del(k)
			continue
		}
		keys[k] = true
	}
	for k := range st.pending {
		if excluded(S, M, k) {
			st.clearPending(k)
		}
	}
	sorted := make([]string, 0, len(keys))
	for k := range keys {
		sorted = append(sorted, k)
	}
	sort.Strings(sorted)

	for _, k := range sorted {
		if r.skipped(k) || excluded(S, M, k) || selfOrAncestor(S.failed, k) || selfOrAncestor(M.failed, k) {
			continue
		}
		s, sok := S.nodes[k]
		m, mok := M.nodes[k]
		b, bok := st.entries[k]

		forced := OriginUnknown
		if p, ok := st.pending[k]; ok {
			switch {
			case !sok || mok:
				// Resolved on its own (gone from the source too, or back in the mirror).
				st.clearPending(k)
			case p.Resolve == DecideDelete:
				forced = OriginDrive
			case p.Resolve == DecideRestore:
				forced = OriginMirror
			default:
				r.skip[k] = true
				continue
			}
		}

		switch {
		case sok && mok && s.Dir && m.Dir:
			r.bothDirs(k, s, m, b, bok)
		case sok && mok && !s.Dir && !m.Dir:
			r.bothFiles(k, s, m, b, bok)
		case sok && mok:
			r.typeConflict(k, s, m)
		case sok:
			r.onlySource(S, k, s, b, bok, forced)
		case mok:
			r.onlyMirror(k, m, b, bok)
		default:
			st.del(k)
		}
	}

	// Remove mirror folders whose content was deleted in the source, deepest first.
	sort.Slice(r.dirDels, func(i, j int) bool {
		return strings.Count(r.dirDels[i], "/") > strings.Count(r.dirDels[j], "/")
	})
	for _, rel := range r.dirDels {
		if err := removeDir(r.abs(dst, rel)); err == nil || errors.Is(err, os.ErrNotExist) {
			st.del(key(rel))
		} else {
			// Something we do not track is still inside (e.g. an ignored file): keep the folder.
			r.recordDir(rel)
		}
	}
}

// record stores the synced state of a file present on both sides.
func (r *run) record(rel string) {
	s, err1 := entryInfo(r.abs(src, rel))
	m, err2 := entryInfo(r.abs(dst, rel))
	id, err3 := fileID(r.abs(src, rel))
	if err1 != nil || err2 != nil || err3 != nil {
		return // next pass will look again
	}
	r.e.st.set(entry{Path: rel, ID: id, SSize: s.Size, SMTime: s.MTime, MSize: m.Size, MMTime: m.MTime, Drive: r.drive[id]})
}

func (r *run) recordDir(rel string) {
	id, _ := fileID(r.abs(dst, rel))
	r.e.st.set(entry{Path: rel, Dir: true, ID: id, Drive: r.drive[id]})
}

func (r *run) bothDirs(k string, s, m node, b entry, bok bool) {
	r.stats.Dirs++
	if s.Path != m.Path {
		r.fixCase(k, s, m, b, bok)
	}
	switch {
	case !bok:
		r.recordDir(s.Path)
	case b.Drive == 0 && r.drive != nil:
		if g, ok := r.drive[b.ID]; ok {
			b.Drive = g
			r.e.st.set(b)
		}
	}
}

// fixCase aligns a case-only rename: the side that still has the synced
// spelling takes the other side's new spelling.
func (r *run) fixCase(k string, s, m node, b entry, bok bool) {
	from, to, on := m.Path, s.Path, dst
	if bok && b.Path == s.Path {
		from, to, on = s.Path, m.Path, src
	}
	if err := os.Rename(r.abs(on, from), r.abs(on, to)); err != nil {
		r.stats.addErr(err)
		return
	}
	if bok {
		r.e.st.move(k, to)
	}
	r.stats.Renamed++
}

func sameEntry(size int64, mtime int64, bs int64, bm int64) bool {
	return size == bs && sameMTime(time.Unix(0, mtime), time.Unix(0, bm))
}

func (r *run) bothFiles(k string, s, m node, b entry, bok bool) {
	r.stats.Files++
	r.stats.Bytes += s.Size
	if s.Path != m.Path {
		r.fixCase(k, s, m, b, bok)
		return
	}
	sSame := bok && sameEntry(s.Size, s.MTime, b.SSize, b.SMTime)
	mSame := bok && sameEntry(m.Size, m.MTime, b.MSize, b.MMTime)
	if sSame && mSame {
		if b.Drive == 0 && r.drive != nil {
			if g, ok := r.drive[b.ID]; ok {
				b.Drive = g
				r.e.st.set(b)
			}
		}
		return
	}
	sid, err := r.id(src, s.Path)
	if err != nil {
		r.stats.addErr(err)
		return
	}
	mid, err := r.id(dst, m.Path)
	if err != nil {
		r.stats.addErr(err)
		return
	}
	if sid == mid {
		// Same data under both names. An in-place edit through the source
		// leaves the mirror's directory entry stale, so Drive may not notice:
		// touch the mirror name.
		if bok && !sSame && mSame {
			if err := touch(r.abs(dst, m.Path), time.Unix(0, s.MTime)); err != nil {
				r.stats.addErr(err)
			} else {
				r.stats.Touched++
			}
		}
		r.record(s.Path)
		return
	}
	sEdited := !bok || sid != b.ID || !sSame
	mEdited := !bok || mid != b.ID || !mSame
	switch {
	case sEdited && !mEdited:
		r.link(dst, s.Path, true)
		r.stats.Pushed++
	case !sEdited && mEdited:
		r.link(src, s.Path, true)
		r.stats.Pulled++
	default:
		if same, err := sameContent(r.abs(src, s.Path), r.abs(dst, m.Path)); err == nil && same {
			r.link(dst, s.Path, true)
			return
		}
		r.conflict(k, s, m)
	}
}

// link makes the entry on side `onto` a hardlink to the other side's file.
func (r *run) link(onto side, rel string, replace bool) {
	from := src
	if onto == src {
		from = dst
	}
	target := r.abs(onto, rel)
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		r.stats.addErr(err)
		return
	}
	if err := linkFile(r.abs(from, rel), target, replace); err != nil {
		r.stats.addErr(fmt.Errorf("%s: %w", rel, err))
		return
	}
	r.record(rel)
}

func (r *run) conflict(k string, s, m node) {
	now := r.e.opt.Now()
	loser := src
	if s.MTime > m.MTime {
		loser = dst
	}
	name := conflictName(s.Path, now)
	for i := 2; ; i++ {
		_, e1 := os.Lstat(r.abs(src, name))
		_, e2 := os.Lstat(r.abs(dst, name))
		if errors.Is(e1, os.ErrNotExist) && errors.Is(e2, os.ErrNotExist) {
			break
		}
		name = conflictName(s.Path, now.Add(time.Duration(i)*time.Second))
	}
	winner := dst
	if loser == dst {
		winner = src
	}
	// Keep the loser under the conflict name on both sides, then give the
	// original name the winner's content on both sides.
	if err := os.Rename(r.abs(loser, s.Path), r.abs(loser, name)); err != nil {
		r.stats.addErr(err)
		return
	}
	r.link(winner, name, false)
	r.link(loser, s.Path, false)
	r.stats.Conflicts++
	r.e.log.Info("conflict: kept both versions", "path", s.Path, "copy", name)
}

func conflictName(rel string, t time.Time) string {
	dir, file := path.Split(rel)
	ext := path.Ext(file)
	stem := strings.TrimSuffix(file, ext)
	if stem == "" { // dotfiles like ".env"
		stem, ext = file, ""
	}
	return dir + stem + " (conflict " + t.Format("2006-01-02 150405") + ")" + ext
}

func (r *run) typeConflict(k string, s, m node) {
	// One side has a file, the other a folder: move the file aside.
	on, n := src, s
	if s.Dir {
		on, n = dst, m
	}
	name := conflictName(n.Path, r.e.opt.Now())
	if err := os.Rename(r.abs(on, n.Path), r.abs(on, name)); err != nil {
		r.stats.addErr(err)
	} else {
		r.stats.Conflicts++
	}
	r.skip[k] = true
}

// onlySource handles an entry present in the source but not in the mirror.
func (r *run) onlySource(S *tree, k string, s node, b entry, bok bool, forced Origin) {
	st := r.e.st
	if s.Dir {
		r.stats.Dirs++
	} else {
		r.stats.Files++
		r.stats.Bytes += s.Size
	}
	restore := func() {
		if s.Dir {
			if err := os.MkdirAll(r.abs(dst, s.Path), 0o755); err != nil {
				r.stats.addErr(err)
				return
			}
			r.recordDir(s.Path)
		} else {
			r.link(dst, s.Path, false)
		}
	}
	// New in the source, or inside a folder being restored / partially kept.
	if !bok {
		restore()
		r.stats.Pushed++
		return
	}
	if _, ok := underAny(r.restoring, k); ok {
		restore()
		r.stats.Restored++
		return
	}
	if p, ok := underAny(r.partial, k); ok {
		confirmed := r.partial[p]
		switch {
		case s.Dir && r.hasLocalChanges(S, k):
			r.partial[k] = confirmed
			restore()
			r.stats.Pushed++
		case s.Dir:
			r.deleteHere(S, k, s, confirmed)
		case !sameEntry(s.Size, s.MTime, b.SSize, b.SMTime):
			restore() // a local change beats the remote delete
			r.stats.Pushed++
		default:
			r.deleteHere(S, k, s, confirmed)
		}
		return
	}
	// A file edited locally after it was deleted in Drive: the edit wins.
	if !s.Dir && !sameEntry(s.Size, s.MTime, b.SSize, b.SMTime) {
		restore()
		r.stats.Pushed++
		return
	}

	origin := forced
	if origin == OriginUnknown {
		origin = r.classify(k, s, b)
	}
	switch origin {
	case OriginDrive:
		if s.Dir && r.hasLocalChanges(S, k) {
			// Keep new or changed files, delete the rest.
			r.partial[k] = forced == OriginDrive
			restore()
			r.stats.Pushed++
			return
		}
		r.deleteHere(S, k, s, forced == OriginDrive)
	case OriginMirror:
		r.e.log.Info("restored an entry deleted by hand in the mirror", "path", s.Path)
		st.clearPending(k)
		r.e.forgetGone(k)
		if s.Dir {
			r.restoring[k] = true
		}
		restore()
		r.stats.Restored++
	case OriginWait:
		r.skip[k] = true
		if g := r.e.goneTime(k); !g.IsZero() {
			// Past the classifier's evidence window, and never in the past
			// (that would make the runner spin).
			next := g.Add(7 * time.Second)
			if floor := r.e.opt.Now().Add(time.Second); next.Before(floor) {
				next = floor
			}
			if r.stats.NextCheck.IsZero() || next.Before(r.stats.NextCheck) {
				r.stats.NextCheck = next
			}
		}
	default:
		r.ask(k, s, "It disappeared from the Drive copy, but gdrive-ignore could not confirm that it was deleted in Google Drive.")
	}
}

func (r *run) classify(k string, s node, b entry) Origin {
	if r.e.opt.Classifier == nil {
		return OriginUnknown
	}
	d := Deletion{Path: s.Path, Dir: s.Dir, Gone: r.e.goneTime(k)}
	if b.Drive != 0 {
		d.DriveIDs = append(d.DriveIDs, b.Drive)
	}
	if s.Dir {
		for _, c := range r.e.st.under(k) {
			if c.Drive != 0 && len(d.DriveIDs) < 50 {
				d.DriveIDs = append(d.DriveIDs, c.Drive)
			}
		}
	}
	return r.e.opt.Classifier.Classify(d)
}

func (r *run) ask(k string, s node, reason string) {
	if _, ok := r.e.st.pending[k]; !ok {
		r.e.st.setPending(&Pending{Path: s.Path, Dir: s.Dir, Since: r.e.opt.Now(), Reason: reason})
		r.e.log.Warn("deletion needs a decision", "path", s.Path, "reason", reason)
	}
	r.skip[k] = true
}

// hasLocalChanges reports new or changed source files below folder k.
func (r *run) hasLocalChanges(S *tree, k string) bool {
	for ck, n := range S.nodes {
		if !strings.HasPrefix(ck, k+"/") || n.Dir {
			continue
		}
		b, ok := r.e.st.entries[ck]
		if !ok || !sameEntry(n.Size, n.MTime, b.SSize, b.SMTime) {
			return true
		}
	}
	return false
}

// deleteHere moves a source entry to the Recycle Bin because it was deleted
// in Drive. Large deletions are held for confirmation unless confirmed.
func (r *run) deleteHere(S *tree, k string, s node, confirmed bool) {
	count := 1
	if s.Dir {
		for ck, n := range S.nodes {
			if strings.HasPrefix(ck, k+"/") && !n.Dir {
				count++
			}
		}
	}
	if !confirmed && r.deleted+count > r.e.opt.MaxAutoDelete {
		r.ask(k, s, fmt.Sprintf("Many files were deleted in Google Drive at once (more than %d); confirm before they are removed from this PC.", r.e.opt.MaxAutoDelete))
		return
	}
	if err := r.e.opt.Recycle(r.abs(src, s.Path)); err != nil {
		r.stats.addErr(fmt.Errorf("%s: %w", s.Path, err))
		r.skip[k] = true
		return
	}
	r.deleted += count
	r.stats.DeletedHere += count
	r.e.st.delTree(k)
	r.e.st.clearPending(k)
	r.e.forgetGone(k)
	r.skip[k] = true
	r.e.log.Info("deleted in Google Drive: moved to the Recycle Bin", "path", s.Path)
}

// onlyMirror handles an entry present in the mirror but not in the source.
func (r *run) onlyMirror(k string, m node, b entry, bok bool) {
	st := r.e.st
	pull := func() {
		if m.Dir {
			if err := os.MkdirAll(r.abs(src, m.Path), 0o755); err != nil {
				r.stats.addErr(err)
				return
			}
			r.recordDir(m.Path)
		} else {
			r.link(src, m.Path, false)
		}
		r.stats.Pulled++
	}
	if !bok {
		pull() // new in Drive
		return
	}
	// Deleted in the source. A file changed in Drive meanwhile wins.
	if !m.Dir && !sameEntry(m.Size, m.MTime, b.MSize, b.MMTime) {
		pull()
		return
	}
	if m.Dir {
		r.mirrorDel[k] = true
		r.dirDels = append(r.dirDels, m.Path)
		r.stats.DeletedInDrive++
		return
	}
	if err := removeFile(r.abs(dst, m.Path)); err != nil && !errors.Is(err, os.ErrNotExist) {
		r.stats.addErr(fmt.Errorf("%s: %w", m.Path, err))
		return
	}
	st.del(k)
	r.stats.DeletedInDrive++
}

// ---------- helpers ----------

func cleanRel(rel string) string {
	rel = strings.Trim(filepath.ToSlash(rel), "/")
	if rel == "." {
		return ""
	}
	return rel
}

func tmpName(dir string) string {
	return filepath.Join(dir, fmt.Sprintf("%s%d", tmpPrefix, time.Now().UnixNano()))
}

// linkFile hardlinks src to dst, atomically replacing dst if it exists.
func linkFile(srcPath, dstPath string, replace bool) error {
	if !replace {
		return os.Link(srcPath, dstPath)
	}
	if _, err := os.Lstat(dstPath); errors.Is(err, os.ErrNotExist) {
		return os.Link(srcPath, dstPath)
	}
	tmp := tmpName(filepath.Dir(dstPath))
	if err := os.Link(srcPath, tmp); err != nil {
		return err
	}
	if err := replaceFile(tmp, dstPath); err != nil {
		_ = removeFile(tmp)
		return err
	}
	return nil
}

func touch(p string, mtime time.Time) error {
	return os.Chtimes(p, time.Now(), mtime)
}

// sameMTime allows for coarser target timestamps (ms, 10 ms, 1 s, 2 s).
func sameMTime(a, b time.Time) bool {
	if a.Equal(b) {
		return true
	}
	for _, g := range []time.Duration{time.Microsecond, time.Millisecond, 10 * time.Millisecond, time.Second, 2 * time.Second} {
		if a.Truncate(g).Equal(b) || a.Round(g).Equal(b) || b.Truncate(g).Equal(a) || b.Round(g).Equal(a) {
			return true
		}
	}
	return false
}

func sameContent(a, b string) (bool, error) {
	fa, err := os.Open(a)
	if err != nil {
		return false, err
	}
	defer fa.Close()
	fb, err := os.Open(b)
	if err != nil {
		return false, err
	}
	defer fb.Close()
	ia, _ := fa.Stat()
	ib, _ := fb.Stat()
	if ia.Size() != ib.Size() {
		return false, nil
	}
	ba, bb := make([]byte, 64<<10), make([]byte, 64<<10)
	for {
		na, ea := io.ReadFull(fa, ba)
		nb, eb := io.ReadFull(fb, bb)
		if na != nb || !bytes.Equal(ba[:na], bb[:nb]) {
			return false, nil
		}
		if ea != nil || eb != nil {
			return ea == eb || (errors.Is(ea, io.ErrUnexpectedEOF) || errors.Is(ea, io.EOF)) && (errors.Is(eb, io.ErrUnexpectedEOF) || errors.Is(eb, io.EOF)), nil
		}
	}
}
