// Package runner keeps one pair in sync: it watches the source, turns
// change events into targeted reconcile jobs and runs periodic full passes.
package runner

import (
	"errors"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"runtime/debug"
	"sort"
	"strings"
	"sync"
	"time"

	"gdrive-ignore/internal/mirror"
	"gdrive-ignore/internal/watch"
)

// State is a pair's current activity.
type State string

const (
	StateStarting   State = "starting"
	StateIdle       State = "idle"
	StateSyncing    State = "syncing"
	StatePaused     State = "paused"
	StateNeedsAdopt State = "needs-adopt"
	StateError      State = "error"
)

// Status is a snapshot for the UI.
type Status struct {
	State    State        `json:"state"`
	Mode     mirror.Mode  `json:"mode"`
	Watching bool         `json:"watching"`
	LastSync time.Time    `json:"lastSync"`
	LastFull time.Time    `json:"lastFull"`
	Full     mirror.Stats `json:"full"`   // last full pass: totals
	Recent   mirror.Stats `json:"recent"` // last incremental pass
	Error    string       `json:"error"`
	Pending  int          `json:"pending"`
}

// Options tune a Runner. Zero values get sensible defaults.
type Options struct {
	Interval time.Duration // full pass interval (default 15m)
	Debounce time.Duration // quiet time before syncing events (default 500ms)
	MaxWait  time.Duration // max delay for a busy tree (default 3s)
	MaxJobs  int           // above this, do a full pass instead (default 200)
	Paused   bool
	Log      *slog.Logger
	OnChange func() // called after every status change
}

// Runner drives one Engine.
type Runner struct {
	eng    *mirror.Engine
	source string
	opt    Options

	mu     sync.Mutex
	st     Status
	paused bool

	full   chan struct{}
	resume chan struct{}
	stop   chan struct{}
	done   chan struct{}
}

// Start begins watching and syncing. Call Stop to end it.
func Start(eng *mirror.Engine, source string, opt Options) *Runner {
	if opt.Interval <= 0 {
		opt.Interval = 15 * time.Minute
	}
	if opt.Debounce <= 0 {
		opt.Debounce = 500 * time.Millisecond
	}
	if opt.MaxWait <= 0 {
		opt.MaxWait = 3 * time.Second
	}
	if opt.MaxJobs <= 0 {
		opt.MaxJobs = 200
	}
	if opt.Log == nil {
		opt.Log = slog.Default()
	}
	r := &Runner{
		eng: eng, source: source, opt: opt, paused: opt.Paused,
		st:     Status{State: StateStarting, Mode: eng.Mode()},
		full:   make(chan struct{}, 1),
		resume: make(chan struct{}, 1),
		stop:   make(chan struct{}),
		done:   make(chan struct{}),
	}
	if r.paused {
		r.st.State = StatePaused
	}
	go r.loop()
	return r
}

// Status returns a snapshot.
func (r *Runner) Status() Status {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.st
}

// SyncNow requests a full pass.
func (r *Runner) SyncNow() {
	select {
	case r.full <- struct{}{}:
	default:
	}
}

// SetPaused pauses or resumes syncing. Events keep being collected.
func (r *Runner) SetPaused(p bool) {
	r.mu.Lock()
	r.paused = p
	if p {
		r.st.State = StatePaused
	} else if r.st.State == StatePaused {
		r.st.State = StateIdle
	}
	r.mu.Unlock()
	r.changed()
	if !p {
		select {
		case r.resume <- struct{}{}:
		default:
		}
	}
}

// Stop ends the runner and waits for it.
func (r *Runner) Stop() {
	select {
	case <-r.stop:
	default:
		close(r.stop)
	}
	<-r.done
}

func (r *Runner) isPaused() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.paused
}

func (r *Runner) update(f func(*Status)) {
	r.mu.Lock()
	f(&r.st)
	r.mu.Unlock()
	r.changed()
}

func (r *Runner) changed() {
	if r.opt.OnChange != nil {
		r.opt.OnChange()
	}
}

// job is one reconcile call.
type job struct {
	rel       string
	recursive bool
}

func (r *Runner) loop() {
	defer close(r.done)
	var (
		w          *watch.Watcher
		watchC     <-chan []string
		watchErr   <-chan error
		retryWatch <-chan time.Time
		jobs       = map[string]bool{} // rel -> recursive
		needFull   = true
		first      time.Time
		debounce   *time.Timer
		debounceC  <-chan time.Time
	)
	startWatch := func() {
		var err error
		w, err = watch.New(r.source)
		if err != nil {
			r.opt.Log.Warn("watch failed, relying on periodic sync", "source", r.source, "err", err)
			w, watchC, watchErr = nil, nil, nil
			if !errors.Is(err, watch.ErrUnsupported) {
				retryWatch = time.After(30 * time.Second)
			}
			r.update(func(s *Status) { s.Watching = false })
			return
		}
		watchC, watchErr, retryWatch = w.C, w.Err, nil
		r.update(func(s *Status) { s.Watching = true })
	}
	startWatch()
	defer func() {
		if w != nil {
			w.Close()
		}
	}()

	ticker := time.NewTicker(r.opt.Interval)
	defer ticker.Stop()

	schedule := func() {
		now := time.Now()
		if first.IsZero() {
			first = now
		}
		wait := r.opt.Debounce
		if left := r.opt.MaxWait - now.Sub(first); left < wait {
			wait = max(left, 0)
		}
		if debounce == nil {
			debounce = time.NewTimer(wait)
		} else {
			debounce.Reset(wait)
		}
		debounceC = debounce.C
	}

	run := func() {
		if r.isPaused() {
			r.update(func(s *Status) { s.Pending = len(jobs) })
			return
		}
		first, debounceC = time.Time{}, nil
		if needFull || len(jobs) > r.opt.MaxJobs {
			clear(jobs)
			needFull = false
			r.runFull()
			return
		}
		list := coalesce(jobs)
		clear(jobs)
		r.runJobs(list)
	}

	run() // initial full pass
	for {
		select {
		case <-r.stop:
			return
		case batch, ok := <-watchC:
			if !ok {
				watchC = nil
				continue
			}
			if w.TakeOverflow() {
				needFull = true
			}
			for _, p := range batch {
				r.addJobs(jobs, p)
			}
			if needFull || len(jobs) > 0 {
				schedule()
			}
		case err := <-watchErr:
			r.opt.Log.Warn("watcher stopped", "source", r.source, "err", err)
			w.Close()
			w, watchC, watchErr = nil, nil, nil
			needFull = true
			retryWatch = time.After(30 * time.Second)
			r.update(func(s *Status) { s.Watching = false })
		case <-retryWatch:
			startWatch()
			if w != nil {
				needFull = true
				run()
			}
		case <-debounceC:
			run()
		case <-ticker.C:
			needFull = true
			run()
		case <-r.full:
			needFull = true
			run()
		case <-r.resume:
			if needFull || len(jobs) > 0 {
				run()
			}
		}
	}
}

// addJobs turns a changed path into reconcile jobs.
func (r *Runner) addJobs(jobs map[string]bool, p string) {
	if p == "" || r.eng.InIgnoredDir(p) {
		return
	}
	parent := path.Dir(p)
	if parent == "." {
		parent = ""
	}
	name := path.Base(p)
	add := func(rel string, rec bool) {
		jobs[rel] = jobs[rel] || rec
	}
	fi, err := os.Lstat(filepath.Join(r.source, filepath.FromSlash(p)))
	isDir := err == nil && fi.IsDir()
	if isDir && r.eng.IsIgnoredDir(p) {
		// Content changes inside an ignored folder touch the folder itself.
		return
	}
	add(parent, false)
	if r.eng.Loader().IsIgnoreFile(name) {
		add(parent, true)
		return
	}
	if isDir {
		// A new or renamed folder: sync everything inside it.
		add(p, true)
	}
}

// coalesce drops jobs covered by a recursive job on a strict ancestor.
func coalesce(jobs map[string]bool) []job {
	var recs []string
	for rel, rec := range jobs {
		if rec {
			recs = append(recs, rel)
		}
	}
	var out []job
	for rel, rec := range jobs {
		if !coveredByAncestor(recs, rel) {
			out = append(out, job{rel, rec})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].rel < out[j].rel })
	return out
}

// coveredByAncestor reports whether a strict ancestor of rel is in recs.
func coveredByAncestor(recs []string, rel string) bool {
	if rel == "" {
		return false
	}
	lr := strings.ToLower(rel)
	for _, a := range recs {
		la := strings.ToLower(a)
		if la != lr && (a == "" || strings.HasPrefix(lr, la+"/")) {
			return true
		}
	}
	return false
}

func (r *Runner) runFull() {
	r.update(func(s *Status) { s.State = StateSyncing; s.Pending = 0 })
	st, err := r.eng.Reconcile("", true)
	// A full pass allocates per file; hand that memory back to Windows so the
	// idle agent stays small.
	defer debug.FreeOSMemory()
	now := time.Now()
	r.update(func(s *Status) {
		s.Mode = r.eng.Mode()
		s.Pending = 0
		switch {
		case errors.Is(err, mirror.ErrTargetNotEmpty):
			s.State, s.Error = StateNeedsAdopt, err.Error()
			return
		case err != nil:
			s.State, s.Error = StateError, err.Error()
			return
		}
		s.Full, s.LastFull, s.LastSync = st, now, now
		s.State, s.Error = StateIdle, ""
		if st.ErrorCount > 0 {
			s.Error = strings.Join(st.Errors, "; ")
		}
	})
	if st.Changed() || err != nil {
		r.opt.Log.Info("full sync", "source", r.source, "linked", st.Linked, "copied", st.Copied,
			"touched", st.Touched, "removed", st.Removed, "errors", st.ErrorCount, "took", st.Duration, "err", err)
	}
}

func (r *Runner) runJobs(list []job) {
	r.update(func(s *Status) { s.State = StateSyncing })
	var total mirror.Stats
	var firstErr error
	for _, j := range list {
		st, err := r.eng.Reconcile(j.rel, j.recursive)
		if err != nil && firstErr == nil {
			firstErr = err
		}
		total.Linked += st.Linked
		total.Copied += st.Copied
		total.Touched += st.Touched
		total.Renamed += st.Renamed
		total.Removed += st.Removed
		total.DirsCreated += st.DirsCreated
		total.ErrorCount += st.ErrorCount
		total.Errors = append(total.Errors, st.Errors...)
		total.Duration += st.Duration
	}
	now := time.Now()
	r.update(func(s *Status) {
		s.Mode = r.eng.Mode()
		s.Recent, s.LastSync, s.Pending = total, now, 0
		s.State, s.Error = StateIdle, ""
		if errors.Is(firstErr, mirror.ErrTargetNotEmpty) {
			s.State, s.Error = StateNeedsAdopt, firstErr.Error()
		} else if firstErr != nil {
			s.State, s.Error = StateError, firstErr.Error()
		} else if total.ErrorCount > 0 {
			s.Error = strings.Join(total.Errors, "; ")
		}
	})
	if total.Changed() || total.ErrorCount > 0 {
		r.opt.Log.Info("sync", "source", r.source, "jobs", len(list), "linked", total.Linked, "copied", total.Copied,
			"touched", total.Touched, "removed", total.Removed, "errors", total.ErrorCount)
	}
}
