// Package runner keeps one pair in sync: it watches both the source and the
// mirror, runs a two-way pass shortly after changes settle, and rescans
// periodically as a safety net.
package runner

import (
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"gdrive-ignore/internal/twoway"
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
	State      State        `json:"state"`
	Watching   bool         `json:"watching"`
	LastSync   time.Time    `json:"lastSync"`
	Last       twoway.Stats `json:"last"`       // most recent pass (totals + actions)
	Activity   twoway.Stats `json:"activity"`   // most recent pass that changed something
	ActivityAt time.Time    `json:"activityAt"` //
	Error      string       `json:"error"`
}

// Options tune a Runner. Zero values get sensible defaults.
type Options struct {
	Interval time.Duration // full rescan interval (default 15m)
	Debounce time.Duration // quiet time before syncing (default 500ms)
	MaxWait  time.Duration // max delay while changes keep coming (default 3s)
	Paused   bool
	Log      *slog.Logger
	OnChange func()
}

// Runner drives one Engine.
type Runner struct {
	eng            *twoway.Engine
	source, target string
	opt            Options

	mu     sync.Mutex
	st     Status
	paused bool

	kick   chan struct{}
	resume chan struct{}
	stop   chan struct{}
	done   chan struct{}
}

// Start begins watching and syncing. Call Stop to end it.
func Start(eng *twoway.Engine, source, target string, opt Options) *Runner {
	if opt.Interval <= 0 {
		opt.Interval = 15 * time.Minute
	}
	if opt.Debounce <= 0 {
		opt.Debounce = 500 * time.Millisecond
	}
	if opt.MaxWait <= 0 {
		opt.MaxWait = 3 * time.Second
	}
	if opt.Log == nil {
		opt.Log = slog.Default()
	}
	r := &Runner{
		eng: eng, source: source, target: target, opt: opt, paused: opt.Paused,
		st:     Status{State: StateStarting},
		kick:   make(chan struct{}, 1),
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

// SyncNow requests a pass.
func (r *Runner) SyncNow() {
	select {
	case r.kick <- struct{}{}:
	default:
	}
}

// SetPaused pauses or resumes syncing. Changes are still noticed (and
// mirror deletions timestamped) while paused.
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

// side is one watched folder.
type side struct {
	root   string
	mirror bool
	w      *watch.Watcher
	c      <-chan watch.Batch
	errc   <-chan error
	retry  <-chan time.Time
}

func (r *Runner) startWatch(s *side) {
	w, err := watch.New(s.root)
	if err != nil {
		r.opt.Log.Warn("watch failed, relying on periodic sync", "folder", s.root, "err", err)
		s.w, s.c, s.errc = nil, nil, nil
		if !errors.Is(err, watch.ErrUnsupported) {
			s.retry = time.After(30 * time.Second)
		}
		return
	}
	s.w, s.c, s.errc, s.retry = w, w.C, w.Err, nil
}

func (r *Runner) loop() {
	defer close(r.done)
	src := &side{root: r.source}
	dst := &side{root: r.target, mirror: true}
	_ = os.MkdirAll(r.target, 0o755)
	r.startWatch(src)
	r.startWatch(dst)
	defer func() {
		for _, s := range []*side{src, dst} {
			if s.w != nil {
				s.w.Close()
			}
		}
	}()
	watching := func() bool { return src.w != nil && dst.w != nil }
	r.update(func(s *Status) { s.Watching = watching() })

	ticker := time.NewTicker(r.opt.Interval)
	defer ticker.Stop()
	var (
		pending   bool
		first     time.Time
		debounce  *time.Timer
		debounceC <-chan time.Time
		recheck   *time.Timer
		recheckC  <-chan time.Time
	)
	schedule := func() {
		pending = true
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
			return
		}
		pending, first, debounceC = false, time.Time{}, nil
		next := r.pass()
		if !next.IsZero() {
			d := time.Until(next)
			if recheck == nil {
				recheck = time.NewTimer(d)
			} else {
				recheck.Reset(d)
			}
			recheckC = recheck.C
		}
	}

	handle := func(s *side, b watch.Batch) {
		relevant := s.w != nil && s.w.TakeOverflow()
		for _, p := range b.Paths {
			if r.eng.InIgnoredDir(p) {
				continue
			}
			abs := filepath.Join(s.root, filepath.FromSlash(p))
			fi, err := os.Lstat(abs)
			if err == nil && fi.IsDir() && r.eng.IsIgnoredDir(p) {
				continue // content changed inside an ignored folder
			}
			if s.mirror && errors.Is(err, os.ErrNotExist) {
				r.eng.NoteGone(p, b.Time)
			}
			relevant = true
		}
		if relevant {
			schedule()
		}
	}

	run() // initial pass
	for {
		select {
		case <-r.stop:
			return
		case b, ok := <-src.c:
			if !ok {
				src.c = nil
				continue
			}
			handle(src, b)
		case b, ok := <-dst.c:
			if !ok {
				dst.c = nil
				continue
			}
			handle(dst, b)
		case err := <-src.errc:
			r.watchFailed(src, err)
			r.update(func(s *Status) { s.Watching = false })
			schedule()
		case err := <-dst.errc:
			r.watchFailed(dst, err)
			r.update(func(s *Status) { s.Watching = false })
			schedule()
		case <-src.retry:
			r.startWatch(src)
			r.update(func(s *Status) { s.Watching = watching() })
			schedule()
		case <-dst.retry:
			r.startWatch(dst)
			r.update(func(s *Status) { s.Watching = watching() })
			schedule()
		case <-debounceC:
			run()
		case <-recheckC:
			recheckC = nil
			run()
		case <-ticker.C:
			run()
		case <-r.kick:
			run()
		case <-r.resume:
			if pending {
				run()
			}
		}
	}
}

func (r *Runner) watchFailed(s *side, err error) {
	r.opt.Log.Warn("watcher stopped", "folder", s.root, "err", err)
	if s.w != nil {
		s.w.Close()
	}
	s.w, s.c, s.errc = nil, nil, nil
	s.retry = time.After(30 * time.Second)
}

// pass runs one two-way pass and returns when to check again for waiting
// deletions (zero if none).
func (r *Runner) pass() time.Time {
	r.update(func(s *Status) { s.State = StateSyncing })
	st, err := r.eng.Reconcile()
	defer debug.FreeOSMemory()
	now := time.Now()
	r.update(func(s *Status) {
		switch {
		case errors.Is(err, twoway.ErrTargetNotEmpty):
			s.State, s.Error = StateNeedsAdopt, err.Error()
			return
		case err != nil:
			s.State, s.Error = StateError, err.Error()
			return
		}
		s.Last, s.LastSync = st, now
		if st.Changed() {
			s.Activity, s.ActivityAt = st, now
		}
		s.State, s.Error = StateIdle, ""
		if st.ErrorCount > 0 {
			s.Error = strings.Join(st.Errors, "; ")
		}
	})
	if st.Changed() || st.ErrorCount > 0 || err != nil {
		r.opt.Log.Info("sync", "source", r.source,
			"pushed", st.Pushed, "pulled", st.Pulled, "renamed", st.Renamed, "conflicts", st.Conflicts,
			"deletedInDrive", st.DeletedInDrive, "deletedHere", st.DeletedHere, "restored", st.Restored,
			"pending", st.Pending, "errors", st.ErrorCount, "took", st.Duration, "err", err)
	}
	return st.NextCheck
}
