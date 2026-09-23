// Package mirror keeps a target folder an exact, filtered, one-way mirror of
// a source folder, using hardlinks where possible and copies otherwise.
package mirror

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
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

// Mode selects how files are placed in the target.
type Mode string

const (
	ModeAuto     Mode = "auto"
	ModeHardlink Mode = "hardlink"
	ModeCopy     Mode = "copy"
)

const tmpPrefix = ".gdi-tmp-"

// ErrTargetNotEmpty is returned on the first sync into a folder that already
// has content, unless Options.Adopt is set.
var ErrTargetNotEmpty = errors.New("target folder is not empty; confirm adopting it first")

// Options configures an Engine.
type Options struct {
	Source       string      // absolute source folder, never written to
	Target       string      // absolute target folder inside a Drive-synced location
	ManifestPath string      // where the engine records what it owns
	IgnoreFiles  []string    // per-directory ignore file names to honor
	Rules        *ignore.Set // global + pair rules
	Mode         Mode
	Adopt        bool // allow the first sync into a non-empty target
	Log          *slog.Logger
}

// Stats summarizes one reconcile pass.
type Stats struct {
	Files, Dirs    int   // included entries
	Bytes          int64 // included file bytes
	Ignored        int   // ignored entries (an ignored dir counts once)
	Skipped        int   // symlinks, junctions and other special files
	Linked, Copied int   // new or replaced target files
	Touched        int   // in-place edits signalled to Drive
	Renamed        int   // case-only renames
	Removed        int   // target entries deleted
	DirsCreated    int
	ErrorCount     int
	Errors         []string // first few errors
	Duration       time.Duration
}

func (s *Stats) addErr(err error) {
	s.ErrorCount++
	if len(s.Errors) < 20 {
		s.Errors = append(s.Errors, err.Error())
	}
}

// Changed reports whether the pass modified the target.
func (s *Stats) Changed() bool {
	return s.Linked+s.Copied+s.Touched+s.Renamed+s.Removed+s.DirsCreated > 0
}

// IgnoredEntry describes a path excluded by a rule (used by Preview).
type IgnoredEntry struct {
	Path   string `json:"path"`
	Dir    bool   `json:"dir"`
	Rule   string `json:"rule"`
	Source string `json:"source"`
	Line   int    `json:"line"`
	Size   int64  `json:"size"`  // bytes, including everything below a dir
	Count  int    `json:"count"` // files, including everything below a dir
}

// Engine mirrors one source/target pair. It is safe for concurrent use.
type Engine struct {
	opt    Options
	loader *ignore.Loader
	log    *slog.Logger

	mu       sync.Mutex
	mode     Mode
	man      *manifest
	fresh    bool // no manifest yet
	nonEmpty bool // target had content before we ever ran

	ignMu       sync.RWMutex
	ignoredDirs map[string]bool // dirs found ignored by the last walks
}

// InIgnoredDir reports whether rel lies strictly inside a directory that was
// ignored when last walked. Watch events there can be dropped cheaply.
func (e *Engine) InIgnoredDir(rel string) bool {
	k := key(cleanRel(rel))
	e.ignMu.RLock()
	defer e.ignMu.RUnlock()
	if len(e.ignoredDirs) == 0 {
		return false
	}
	for i := strings.LastIndexByte(k, '/'); i > 0; i = strings.LastIndexByte(k[:i], '/') {
		if e.ignoredDirs[k[:i]] {
			return true
		}
	}
	return false
}

// IsIgnoredDir reports whether rel itself was ignored as a directory when last walked.
func (e *Engine) IsIgnoredDir(rel string) bool {
	e.ignMu.RLock()
	defer e.ignMu.RUnlock()
	return e.ignoredDirs[key(cleanRel(rel))]
}

func (e *Engine) setIgnoredDir(rel string, ignored bool) {
	e.ignMu.Lock()
	defer e.ignMu.Unlock()
	if ignored {
		e.ignoredDirs[key(rel)] = true
	} else {
		delete(e.ignoredDirs, key(rel))
	}
}

// forgetIgnoredDirs drops remembered ignored dirs at or below rel.
func (e *Engine) forgetIgnoredDirs(rel string) {
	e.ignMu.Lock()
	defer e.ignMu.Unlock()
	k := key(rel)
	for d := range e.ignoredDirs {
		if k == "" || d == k || strings.HasPrefix(d, k+"/") {
			delete(e.ignoredDirs, d)
		}
	}
}

// New validates the pair and loads its manifest.
func New(opt Options) (*Engine, error) {
	if err := Validate(opt.Source, opt.Target); err != nil {
		return nil, err
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
	man, exists, err := loadManifest(opt.ManifestPath)
	if err != nil {
		return nil, fmt.Errorf("reading manifest: %w", err)
	}
	e := &Engine{
		opt:    opt,
		loader: &ignore.Loader{Root: opt.Source, FileNames: opt.IgnoreFiles, Base: opt.Rules},
		log:    opt.Log,
		man:    man,
		fresh:  !exists,

		ignoredDirs: map[string]bool{},
	}
	if e.fresh {
		entries, err := os.ReadDir(opt.Target)
		e.nonEmpty = err == nil && len(entries) > 0
		man.dirty = true
	}
	e.mode = opt.Mode
	if e.mode == "" || e.mode == ModeAuto {
		e.mode = ModeCopy
		if hardlinkCapable(opt.Source, opt.Target) {
			e.mode = ModeHardlink
		}
	}
	return e, nil
}

// Mode returns the effective mode (auto resolved).
func (e *Engine) Mode() Mode {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.mode
}

// NeedsAdopt reports whether the first sync would be refused.
func (e *Engine) NeedsAdopt() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.fresh && e.nonEmpty && !e.opt.Adopt
}

// Loader exposes the rule loader (for checking single paths).
func (e *Engine) Loader() *ignore.Loader { return e.loader }

// Reconcile brings the target in line with the source below rel ("" = the
// whole pair). With recursive false only rel's direct children are synced.
func (e *Engine) Reconcile(rel string, recursive bool) (Stats, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.fresh && e.nonEmpty && !e.opt.Adopt {
		return Stats{}, ErrTargetNotEmpty
	}
	r := e.newRun(false)
	if err := r.reconcile(rel, recursive); err != nil {
		return r.stats, err
	}
	if err := e.man.save(e.opt.Source, e.opt.Target); err != nil {
		r.stats.addErr(fmt.Errorf("saving manifest: %w", err))
	} else {
		e.fresh = false
	}
	r.stats.Duration = time.Since(r.start)
	return r.stats, nil
}

// Preview runs a dry pass over the whole pair and lists what is ignored.
// With measure set, ignored folders are walked to total their size.
func (e *Engine) Preview(measure bool) (Stats, []IgnoredEntry, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	r := e.newRun(true)
	r.measure = measure
	err := r.reconcile("", true)
	r.stats.Duration = time.Since(r.start)
	return r.stats, r.ignored, err
}

type run struct {
	e       *Engine
	dry     bool
	measure bool
	start   time.Time
	stats   Stats
	desired map[string]bool
	failed  []string // source dirs that could not be read; never prune below them
	ignored []IgnoredEntry
}

func (e *Engine) newRun(dry bool) *run {
	return &run{e: e, dry: dry, start: time.Now(), desired: map[string]bool{}}
}

func (r *run) reconcile(rel string, recursive bool) error {
	rel = cleanRel(rel)
	e := r.e
	if _, err := os.Stat(e.opt.Target); errors.Is(err, fs.ErrNotExist) && !r.dry {
		if err := os.MkdirAll(e.opt.Target, 0o755); err != nil {
			return err
		}
	}
	set, ok, err := r.setFor(rel)
	if err != nil {
		return err
	}
	if ok {
		if rel != "" {
			r.desired[key(rel)] = true
			if err := r.ensureParents(rel); err != nil {
				return err
			}
		}
		if recursive && !r.dry {
			e.forgetIgnoredDirs(rel)
		}
		r.walk(rel, set, recursive)
	}
	r.prune(rel, recursive, !ok)
	return nil
}

// setFor loads the rules that apply inside rel. ok is false when rel is
// ignored, missing or not a directory, meaning nothing below it is wanted.
func (r *run) setFor(rel string) (*ignore.Set, bool, error) {
	l := r.e.loader
	set, err := l.ForDir(l.Base, "")
	if err != nil {
		return nil, false, err
	}
	if rel == "" {
		return set, true, nil
	}
	fi, err := os.Lstat(filepath.Join(r.e.opt.Source, filepath.FromSlash(rel)))
	if err != nil || !fi.IsDir() {
		return nil, false, nil
	}
	parts := strings.Split(rel, "/")
	for i := range parts {
		cur := strings.Join(parts[:i+1], "/")
		if ign, _ := set.Match(cur, true); ign {
			return nil, false, nil
		}
		if set, err = l.ForDir(set, cur); err != nil {
			return nil, false, err
		}
	}
	return set, true, nil
}

// ensureParents creates rel and its ancestors in the target.
func (r *run) ensureParents(rel string) error {
	parts := strings.Split(rel, "/")
	for i := range parts {
		cur := strings.Join(parts[:i+1], "/")
		dst := r.e.target(cur)
		fi, err := os.Lstat(dst)
		if err == nil && fi.IsDir() {
			continue
		}
		if r.dry {
			return nil
		}
		if err == nil {
			if err := removeFile(dst); err != nil {
				return err
			}
		}
		if err := os.Mkdir(dst, 0o755); err != nil && !errors.Is(err, fs.ErrExist) {
			return err
		}
		r.own(cur, true)
		r.stats.DirsCreated++
	}
	return nil
}

func (r *run) walk(relDir string, set *ignore.Set, recursive bool) {
	e := r.e
	srcEntries, err := os.ReadDir(e.source(relDir))
	if err != nil {
		r.failed = append(r.failed, relDir)
		r.stats.addErr(err)
		return
	}
	tgt := map[string]fs.DirEntry{}
	if tgtEntries, err := os.ReadDir(e.target(relDir)); err == nil {
		for _, te := range tgtEntries {
			if strings.HasPrefix(te.Name(), tmpPrefix) {
				if !r.dry {
					_ = removeFile(e.target(join(relDir, te.Name())))
				}
				continue
			}
			tgt[key(te.Name())] = te
		}
	}
	for _, de := range srcEntries {
		name := de.Name()
		rel := join(relDir, name)
		isDir := de.IsDir()
		if !isDir && !de.Type().IsRegular() {
			r.stats.Skipped++
			continue
		}
		if ign, rule := set.Match(rel, isDir); ign {
			r.stats.Ignored++
			if isDir && !r.dry {
				e.setIgnoredDir(rel, true)
			}
			if r.dry {
				r.recordIgnored(rel, isDir, de, rule)
			}
			continue
		}
		r.desired[key(rel)] = true
		te, exists := tgt[key(name)]
		if isDir {
			r.stats.Dirs++
			if !r.dry {
				e.setIgnoredDir(rel, false)
			}
			if !r.syncDir(rel, te, exists) {
				continue
			}
			if recursive {
				child, err := e.loader.ForDir(set, rel)
				if err != nil {
					r.stats.addErr(err)
				}
				r.walk(rel, child, true)
			}
			continue
		}
		info, err := de.Info()
		if err != nil {
			r.stats.addErr(err)
			continue
		}
		r.stats.Files++
		r.stats.Bytes += info.Size()
		if err := r.syncFile(rel, info, te, exists); err != nil {
			r.stats.addErr(fmt.Errorf("%s: %w", rel, err))
		}
	}
}

// syncDir makes sure rel exists as a directory in the target. It returns
// false if that failed and the directory should not be descended into.
func (r *run) syncDir(rel string, te fs.DirEntry, exists bool) bool {
	e := r.e
	dst := e.target(rel)
	if exists && te.IsDir() {
		if err := r.fixCase(rel, te); err != nil {
			r.stats.addErr(err)
		}
		if !r.dry {
			r.own(rel, true)
		}
		return true
	}
	if r.dry {
		r.stats.DirsCreated++
		return true
	}
	if exists {
		if err := removeFile(dst); err != nil {
			r.stats.addErr(err)
			return false
		}
	}
	if err := os.Mkdir(dst, 0o755); err != nil && !errors.Is(err, fs.ErrExist) {
		r.stats.addErr(err)
		return false
	}
	r.own(rel, true)
	r.stats.DirsCreated++
	return true
}

func (r *run) syncFile(rel string, src fs.FileInfo, te fs.DirEntry, exists bool) error {
	e := r.e
	srcPath, dstPath := e.source(rel), e.target(rel)
	if exists && te.IsDir() {
		return fmt.Errorf("a folder with this name exists in the target")
	}
	if exists {
		if err := r.fixCase(rel, te); err != nil {
			return err
		}
	}
	var tinfo fs.FileInfo
	if exists {
		var err error
		if tinfo, err = te.Info(); err != nil {
			exists = false
		}
	}
	unchanged := exists && tinfo.Size() == src.Size() && tinfo.ModTime().Equal(src.ModTime())

	if e.mode == ModeHardlink {
		if unchanged {
			r.own(rel, false)
			return nil
		}
		if exists {
			// The directory entry differs: either the source was edited in
			// place (same file, stale entry that Drive may not notice) or it
			// was replaced by a new file (atomic save) and the link is stale.
			sfi, err1 := os.Stat(srcPath)
			tfi, err2 := os.Stat(dstPath)
			if err1 == nil && err2 == nil && os.SameFile(sfi, tfi) {
				if !r.dry {
					if err := touch(dstPath, sfi.ModTime()); err != nil {
						return err
					}
					r.own(rel, false)
				}
				r.stats.Touched++
				return nil
			}
		}
		if r.dry {
			r.stats.Linked++
			return nil
		}
		err := linkFile(srcPath, dstPath, exists)
		if err == nil {
			r.own(rel, false)
			r.stats.Linked++
			return nil
		}
		if !isCrossDevice(err) || e.opt.Mode == ModeHardlink {
			return err
		}
		e.log.Warn("hardlinks not possible, switching to copy mode", "target", e.opt.Target, "err", err)
		e.mode = ModeCopy
	}

	if unchanged {
		r.own(rel, false)
		return nil
	}
	if r.dry {
		r.stats.Copied++
		return nil
	}
	if err := copyFile(srcPath, dstPath, src.ModTime()); err != nil {
		return err
	}
	r.own(rel, false)
	r.stats.Copied++
	return nil
}

// fixCase renames a target entry whose name differs from the source only by
// case, so Drive shows the current name.
func (r *run) fixCase(rel string, te fs.DirEntry) error {
	want := path.Base(rel)
	if te.Name() == want {
		return nil
	}
	r.stats.Renamed++
	if r.dry {
		return nil
	}
	dir := path.Dir(rel)
	if dir == "." {
		dir = ""
	}
	old := join(dir, te.Name())
	if err := os.Rename(r.e.target(old), r.e.target(rel)); err != nil {
		return err
	}
	// Move manifest entries to the new spelling.
	if me, ok := r.e.man.get(old); ok {
		r.e.man.remove(old)
		r.e.man.add(rel, me.Dir)
		for _, c := range r.e.man.under(old) {
			r.e.man.remove(c.Path)
			r.e.man.add(rel+c.Path[len(old):], c.Dir)
		}
	}
	return nil
}

// prune deletes owned target entries below rel that are no longer wanted.
// With self set, rel itself is removed as well.
func (r *run) prune(rel string, recursive, self bool) {
	e := r.e
	var files, dirs []manifestEntry
	consider := func(me manifestEntry) {
		if r.desired[key(me.Path)] || r.underFailed(me.Path) {
			return
		}
		if !recursive && !self {
			// Only direct children of rel are in scope; deeper entries
			// belong to children that were not rescanned.
			if child := firstChild(rel, me.Path); r.desired[key(child)] {
				return
			}
		}
		if me.Dir {
			dirs = append(dirs, me)
		} else {
			files = append(files, me)
		}
	}
	for _, me := range e.man.under(rel) {
		consider(me)
	}
	if self && rel != "" {
		if me, ok := e.man.get(rel); ok {
			consider(me)
		}
	}
	for _, me := range files {
		r.stats.Removed++
		if r.dry {
			continue
		}
		if err := removeFile(e.target(me.Path)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			r.stats.addErr(fmt.Errorf("removing %s: %w", me.Path, err))
			continue
		}
		e.man.remove(me.Path)
	}
	sort.Slice(dirs, func(i, j int) bool {
		return strings.Count(dirs[i].Path, "/") > strings.Count(dirs[j].Path, "/")
	})
	for _, me := range dirs {
		r.stats.Removed++
		if r.dry {
			continue
		}
		err := os.Remove(e.target(me.Path))
		if err != nil && !errors.Is(err, fs.ErrNotExist) && len(e.man.under(me.Path)) > 0 {
			// Something we own is still inside (e.g. a locked file); retry later.
			continue
		}
		// Gone, or only holds things we don't own: stop tracking it.
		e.man.remove(me.Path)
	}
}

// own records that the engine owns rel in the target. No-op for dry runs.
func (r *run) own(rel string, dir bool) {
	if !r.dry {
		r.e.man.add(rel, dir)
	}
}

func (r *run) underFailed(rel string) bool {
	k := key(rel)
	for _, f := range r.failed {
		if f == "" || k == key(f) || strings.HasPrefix(k, key(f)+"/") {
			return true
		}
	}
	return false
}

func (r *run) recordIgnored(rel string, isDir bool, de fs.DirEntry, rule *ignore.Rule) {
	ie := IgnoredEntry{Path: rel, Dir: isDir}
	if rule != nil {
		ie.Rule, ie.Source, ie.Line = rule.Text, rule.Source, rule.Line
	}
	if !isDir {
		if info, err := de.Info(); err == nil {
			ie.Size, ie.Count = info.Size(), 1
		}
	} else if r.measure {
		_ = filepath.WalkDir(r.e.source(rel), func(_ string, d fs.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if d.Type().IsRegular() {
				if info, err := d.Info(); err == nil {
					ie.Size += info.Size()
					ie.Count++
				}
			}
			return nil
		})
	}
	r.ignored = append(r.ignored, ie)
}

func (e *Engine) source(rel string) string {
	return filepath.Join(e.opt.Source, filepath.FromSlash(rel))
}

func (e *Engine) target(rel string) string {
	return filepath.Join(e.opt.Target, filepath.FromSlash(rel))
}

func join(dir, name string) string {
	if dir == "" {
		return name
	}
	return dir + "/" + name
}

func cleanRel(rel string) string {
	rel = strings.Trim(filepath.ToSlash(rel), "/")
	if rel == "." {
		return ""
	}
	return rel
}

// firstChild returns the direct child of dir on the way to p.
func firstChild(dir, p string) string {
	rest := p
	if dir != "" {
		rest = p[len(dir)+1:]
	}
	if i := strings.IndexByte(rest, '/'); i >= 0 {
		rest = rest[:i]
	}
	return join(dir, rest)
}

func tmpName(dir string) string {
	return filepath.Join(dir, fmt.Sprintf("%s%d", tmpPrefix, time.Now().UnixNano()))
}

// linkFile hardlinks src to dst, atomically replacing dst if it exists.
func linkFile(src, dst string, replace bool) error {
	if !replace {
		return os.Link(src, dst)
	}
	tmp := tmpName(filepath.Dir(dst))
	if err := os.Link(src, tmp); err != nil {
		return err
	}
	if err := replaceFile(tmp, dst); err != nil {
		_ = removeFile(tmp)
		return err
	}
	return nil
}

// copyFile copies src to dst via a temp file and sets dst's mtime.
func copyFile(src, dst string, mtime time.Time) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	tmp := tmpName(filepath.Dir(dst))
	out, err := os.Create(tmp)
	if err != nil {
		return err
	}
	_, err = io.Copy(out, in)
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Chtimes(tmp, mtime, mtime)
	}
	if err == nil {
		err = replaceFile(tmp, dst)
	}
	if err != nil {
		_ = removeFile(tmp)
	}
	return err
}

// touch sets dst's times through its own path so change notifications fire
// in the target directory (which Drive watches).
func touch(dst string, mtime time.Time) error {
	return os.Chtimes(dst, time.Now(), mtime)
}
