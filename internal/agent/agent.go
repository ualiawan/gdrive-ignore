// Package agent runs all sync pairs and exposes them to the UI.
package agent

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"gdrive-ignore/internal/config"
	"gdrive-ignore/internal/drivefs"
	"gdrive-ignore/internal/ignore"
	"gdrive-ignore/internal/mirror"
	"gdrive-ignore/internal/runner"
)

// Agent owns the configuration and the running pairs.
type Agent struct {
	Version  string
	log      *slog.Logger
	onChange func()
	changes  chan struct{}

	mu     sync.Mutex
	cfg    *config.Config
	global string
	live   map[string]*livePair

	driveMu sync.Mutex
	drive   drivefs.Info
	driveAt time.Time
}

type livePair struct {
	eng *mirror.Engine
	run *runner.Runner
	err string // setup error (e.g. source missing)
}

// PairView is a pair plus its live state, as shown in the UI.
type PairView struct {
	config.Pair
	Status          runner.Status     `json:"status"`
	SetupError      string            `json:"setupError,omitempty"`
	Location        *drivefs.Location `json:"location"`
	HardlinkCapable bool              `json:"hardlinkCapable"`
}

// New loads the configuration. Call Start to begin syncing.
func New(version string, log *slog.Logger, onChange func()) (*Agent, error) {
	cfg, err := config.Load()
	if err != nil {
		return nil, fmt.Errorf("loading config: %w", err)
	}
	global, err := config.GlobalRules()
	if err != nil {
		return nil, fmt.Errorf("loading global rules: %w", err)
	}
	if onChange == nil {
		onChange = func() {}
	}
	a := &Agent{Version: version, log: log, onChange: onChange, cfg: cfg, global: global, live: map[string]*livePair{}, changes: make(chan struct{}, 1)}
	go a.deliverChanges()
	return a, nil
}

// notify schedules an onChange call. It never blocks, so it is safe to call
// while holding locks; bursts are coalesced.
func (a *Agent) notify() {
	select {
	case a.changes <- struct{}{}:
	default:
	}
}

func (a *Agent) deliverChanges() {
	for range a.changes {
		a.onChange()
		time.Sleep(200 * time.Millisecond)
	}
}

// Start launches a runner per pair.
func (a *Agent) Start() {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, p := range a.cfg.Pairs {
		a.startLocked(p)
	}
}

// Stop ends all runners.
func (a *Agent) Stop() {
	a.mu.Lock()
	defer a.mu.Unlock()
	for id := range a.live {
		a.stopLocked(id)
	}
}

func (a *Agent) startLocked(p config.Pair) {
	lp := &livePair{}
	a.live[p.ID] = lp
	eng, err := a.engineFor(p, config.ManifestPath(p.ID))
	if err != nil {
		lp.err = err.Error()
		a.log.Warn("pair not started", "pair", p.Name, "err", err)
		return
	}
	lp.eng = eng
	lp.run = runner.Start(eng, p.Source, runner.Options{
		Interval: time.Duration(a.cfg.IntervalMinutes) * time.Minute,
		Paused:   p.Paused,
		Log:      a.log.With("pair", p.Name),
		OnChange: a.notify,
	})
}

func (a *Agent) stopLocked(id string) {
	if lp, ok := a.live[id]; ok {
		if lp.run != nil {
			lp.run.Stop()
		}
		delete(a.live, id)
	}
}

func (a *Agent) engineFor(p config.Pair, manifest string) (*mirror.Engine, error) {
	var groups [][]ignore.Rule
	if p.UseGlobal {
		groups = append(groups, ignore.Parse(a.global, "global rules", ""))
	}
	groups = append(groups, ignore.Parse(p.Rules, "pair rules", ""))
	files := []string{ignore.DefaultFileName}
	if p.HonorGitignore {
		files = append(files, ".gitignore")
	}
	return mirror.New(mirror.Options{
		Source: p.Source, Target: p.Target, ManifestPath: manifest,
		IgnoreFiles: files, Rules: ignore.NewSet(groups...),
		Mode: p.Mode, Adopt: p.Adopted, Log: a.log.With("pair", p.Name),
	})
}

// Drive returns Drive for Desktop's locations, cached for a few seconds.
func (a *Agent) Drive(refresh bool) drivefs.Info {
	a.driveMu.Lock()
	defer a.driveMu.Unlock()
	if refresh || time.Since(a.driveAt) > 10*time.Second {
		a.drive, a.driveAt = drivefs.Discover(), time.Now()
	}
	return a.drive
}

// Pairs returns all pairs with their status.
func (a *Agent) Pairs() []PairView {
	drive := a.Drive(false)
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]PairView, 0, len(a.cfg.Pairs))
	for _, p := range a.cfg.Pairs {
		v := PairView{Pair: p, Location: drive.Covering(p.Target)}
		v.HardlinkCapable = mirror.HardlinkCapable(p.Source, p.Target)
		if lp := a.live[p.ID]; lp != nil {
			v.SetupError = lp.err
			if lp.run != nil {
				v.Status = lp.run.Status()
			}
		}
		out = append(out, v)
	}
	return out
}

// Summary is a one-line status for the tray tooltip.
func (a *Agent) Summary() string {
	pairs := a.Pairs()
	if len(pairs) == 0 {
		return "No folders set up"
	}
	var syncing, problems, paused int
	for _, p := range pairs {
		switch {
		case p.SetupError != "" || p.Status.State == runner.StateError || p.Status.State == runner.StateNeedsAdopt:
			problems++
		case p.Status.State == runner.StatePaused:
			paused++
		case p.Status.State == runner.StateSyncing || p.Status.State == runner.StateStarting:
			syncing++
		}
	}
	parts := []string{fmt.Sprintf("%d folder(s)", len(pairs))}
	if syncing > 0 {
		parts = append(parts, fmt.Sprintf("%d syncing", syncing))
	}
	if paused > 0 {
		parts = append(parts, fmt.Sprintf("%d paused", paused))
	}
	if problems > 0 {
		parts = append(parts, fmt.Sprintf("%d need attention", problems))
	}
	if syncing+paused+problems == 0 {
		parts = append(parts, "up to date")
	}
	return strings.Join(parts, ", ")
}

// validate checks a pair on its own and against the other pairs.
func (a *Agent) validate(p config.Pair) error {
	p.Source, p.Target = filepath.Clean(p.Source), filepath.Clean(p.Target)
	if err := mirror.Validate(p.Source, p.Target); err != nil {
		return err
	}
	if a.Drive(false).IsLocationRoot(p.Target) {
		return errors.New("choose a subfolder inside the Drive location, not the location itself")
	}
	for _, o := range a.cfg.Pairs {
		if o.ID == p.ID {
			continue
		}
		switch {
		case overlaps(p.Target, o.Target):
			return fmt.Errorf("target overlaps the target of %q", o.Name)
		case within(p.Target, o.Source):
			return fmt.Errorf("target is inside the source of %q", o.Name)
		case within(p.Source, o.Target):
			return fmt.Errorf("source is inside the target of %q", o.Name)
		}
	}
	return nil
}

func overlaps(a, b string) bool { return within(a, b) || within(b, a) }

// within reports whether p is dir or inside it (case-insensitive).
func within(p, dir string) bool {
	p, dir = strings.ToLower(filepath.Clean(p)), strings.ToLower(filepath.Clean(dir))
	return p == dir || strings.HasPrefix(p, strings.TrimRight(dir, `\/`)+string(filepath.Separator))
}

func normalize(p *config.Pair) {
	p.Source = filepath.Clean(strings.TrimSpace(p.Source))
	p.Target = filepath.Clean(strings.TrimSpace(p.Target))
	p.Name = strings.TrimSpace(p.Name)
	if p.Name == "" {
		p.Name = filepath.Base(p.Source)
	}
	if p.Mode == "" {
		p.Mode = mirror.ModeAuto
	}
}

// AddPair validates, saves and starts a new pair.
func (a *Agent) AddPair(p config.Pair) (config.Pair, error) {
	normalize(&p)
	p.ID = config.NewID()
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := a.validate(p); err != nil {
		return p, err
	}
	a.cfg.Pairs = append(a.cfg.Pairs, p)
	if err := a.cfg.Save(); err != nil {
		a.cfg.Pairs = a.cfg.Pairs[:len(a.cfg.Pairs)-1]
		return p, err
	}
	a.startLocked(p)
	a.log.Info("pair added", "pair", p.Name, "source", p.Source, "target", p.Target)
	a.notify()
	return p, nil
}

// UpdatePair replaces a pair's settings and restarts it. Changing the
// target of a pair removes the old mirror's files that the app created.
func (a *Agent) UpdatePair(p config.Pair) error {
	normalize(&p)
	a.mu.Lock()
	defer a.mu.Unlock()
	cur := a.cfg.Find(p.ID)
	if cur == nil {
		return errors.New("pair not found")
	}
	if err := a.validate(p); err != nil {
		return err
	}
	old := *cur
	p.Adopted, p.Paused = old.Adopted, old.Paused
	if !strings.EqualFold(old.Target, p.Target) || !strings.EqualFold(old.Source, p.Source) {
		p.Adopted = false
	}
	*cur = p
	if err := a.cfg.Save(); err != nil {
		*cur = old
		return err
	}
	a.stopLocked(p.ID)
	if !strings.EqualFold(old.Target, p.Target) || !strings.EqualFold(old.Source, p.Source) {
		a.purge(old)
	}
	a.startLocked(p)
	a.notify()
	return nil
}

// purge removes what the app created for p. Errors are logged.
func (a *Agent) purge(p config.Pair) {
	eng, err := a.engineFor(p, config.ManifestPath(p.ID))
	if err != nil {
		_ = os.Remove(config.ManifestPath(p.ID))
		return
	}
	if st, err := eng.Purge(); err != nil {
		a.log.Warn("purge incomplete", "pair", p.Name, "err", err)
	} else {
		a.log.Info("mirror removed", "pair", p.Name, "removed", st.Removed)
	}
}

// RemovePair stops and deletes a pair. With deleteMirror, files the app
// created in the target are deleted; otherwise they are left in place.
func (a *Agent) RemovePair(id string, deleteMirror bool) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	for i, p := range a.cfg.Pairs {
		if p.ID != id {
			continue
		}
		a.stopLocked(id)
		a.cfg.Pairs = append(a.cfg.Pairs[:i], a.cfg.Pairs[i+1:]...)
		if err := a.cfg.Save(); err != nil {
			return err
		}
		if deleteMirror {
			a.purge(p)
		} else {
			_ = os.Remove(config.ManifestPath(id))
		}
		a.log.Info("pair removed", "pair", p.Name, "deleteMirror", deleteMirror)
		a.notify()
		return nil
	}
	return errors.New("pair not found")
}

// Adopt allows the first sync into a non-empty target.
func (a *Agent) Adopt(id string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	p := a.cfg.Find(id)
	if p == nil {
		return errors.New("pair not found")
	}
	p.Adopted = true
	if err := a.cfg.Save(); err != nil {
		return err
	}
	a.stopLocked(id)
	a.startLocked(*p)
	return nil
}

// SetPaused pauses or resumes one pair ("" = all).
func (a *Agent) SetPaused(id string, paused bool) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	found := false
	for i := range a.cfg.Pairs {
		p := &a.cfg.Pairs[i]
		if id != "" && p.ID != id {
			continue
		}
		found = true
		p.Paused = paused
		if lp := a.live[p.ID]; lp != nil && lp.run != nil {
			lp.run.SetPaused(paused)
		}
	}
	if !found && id != "" {
		return errors.New("pair not found")
	}
	return a.cfg.Save()
}

// AllPaused reports whether every pair is paused.
func (a *Agent) AllPaused() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, p := range a.cfg.Pairs {
		if !p.Paused {
			return false
		}
	}
	return len(a.cfg.Pairs) > 0
}

// SyncNow triggers a full pass for one pair ("" = all). Pairs that failed
// to start (e.g. the source was missing) are restarted.
func (a *Agent) SyncNow(id string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, p := range a.cfg.Pairs {
		if id != "" && p.ID != id {
			continue
		}
		lp := a.live[p.ID]
		if lp == nil || lp.run == nil {
			a.stopLocked(p.ID)
			a.startLocked(p)
			continue
		}
		lp.run.SyncNow()
	}
}

// Global returns the global rules text.
func (a *Agent) Global() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.global
}

// SetGlobal saves the global rules and restarts pairs that use them.
func (a *Agent) SetGlobal(text string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := config.SetGlobalRules(text); err != nil {
		return err
	}
	a.global = text
	for _, p := range a.cfg.Pairs {
		if p.UseGlobal {
			a.stopLocked(p.ID)
			a.startLocked(p)
		}
	}
	a.notify()
	return nil
}

// Settings are app-wide options shown in the UI.
type Settings struct {
	IntervalMinutes int `json:"intervalMinutes"`
}

// Settings returns the current settings.
func (a *Agent) Settings() Settings {
	a.mu.Lock()
	defer a.mu.Unlock()
	return Settings{IntervalMinutes: a.cfg.IntervalMinutes}
}

// SetSettings saves settings and restarts pairs if the interval changed.
func (a *Agent) SetSettings(s Settings) error {
	if s.IntervalMinutes < 1 || s.IntervalMinutes > 24*60 {
		return errors.New("interval must be between 1 and 1440 minutes")
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.cfg.IntervalMinutes == s.IntervalMinutes {
		return nil
	}
	a.cfg.IntervalMinutes = s.IntervalMinutes
	if err := a.cfg.Save(); err != nil {
		return err
	}
	for _, p := range a.cfg.Pairs {
		a.stopLocked(p.ID)
		a.startLocked(p)
	}
	return nil
}

// PreviewResult is a dry run of a (possibly unsaved) pair.
type PreviewResult struct {
	mirror.PreviewResult
	Truncated       int               `json:"truncated"` // ignored entries not listed
	HardlinkCapable bool              `json:"hardlinkCapable"`
	Location        *drivefs.Location `json:"location"` // Drive location covering the target
}

// SuggestedRoot is a local folder for mirrors on the user's profile drive.
// Added once to Drive as a computer folder, it allows hardlinks.
func SuggestedRoot() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, "DriveMirror")
}

// Preview dry-runs a pair's rules against its source without writing.
func (a *Agent) Preview(p config.Pair) (PreviewResult, error) {
	normalize(&p)
	tmp, err := os.MkdirTemp("", "gdrive-ignore-preview")
	if err != nil {
		return PreviewResult{}, err
	}
	defer os.RemoveAll(tmp)
	realTarget := p.Target
	if p.Target == "." || p.Target == "" || mirror.Validate(p.Source, p.Target) != nil {
		p.Target = filepath.Join(tmp, "target")
		realTarget = ""
	}
	a.mu.Lock()
	eng, err := a.engineFor(p, filepath.Join(tmp, "manifest.json"))
	a.mu.Unlock()
	if err != nil {
		return PreviewResult{}, err
	}
	res, err := eng.Preview(true)
	if err != nil {
		return PreviewResult{}, err
	}
	sort.Slice(res.Ignored, func(i, j int) bool { return res.Ignored[i].Size > res.Ignored[j].Size })
	out := PreviewResult{PreviewResult: res}
	if realTarget != "" {
		out.HardlinkCapable = mirror.HardlinkCapable(p.Source, realTarget)
		out.Location = a.Drive(false).Covering(realTarget)
	}
	const limit = 1000
	if len(out.Ignored) > limit {
		out.Truncated = len(out.Ignored) - limit
		out.Ignored = out.Ignored[:limit]
	}
	return out, nil
}

// Check explains whether an absolute path inside a pair's source is ignored.
func (a *Agent) Check(abs string) (pair string, res ignore.Result, err error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, p := range a.cfg.Pairs {
		if !within(abs, p.Source) {
			continue
		}
		rel, _ := filepath.Rel(p.Source, abs)
		var loader *ignore.Loader
		if lp := a.live[p.ID]; lp != nil && lp.eng != nil {
			loader = lp.eng.Loader()
		} else {
			// Not running (e.g. CLI use): build the rules without syncing.
			eng, err := a.engineFor(p, filepath.Join(os.TempDir(), "gdrive-ignore-check-unused.json"))
			if err != nil {
				return p.Name, res, err
			}
			loader = eng.Loader()
		}
		fi, statErr := os.Stat(abs)
		res, err = loader.Check(filepath.ToSlash(rel), statErr == nil && fi.IsDir())
		return p.Name, res, err
	}
	return "", res, errors.New("path is not inside any source folder")
}

// PairResult is the outcome of one pair in SyncOnce.
type PairResult struct {
	Name  string
	Stats mirror.Stats
	Err   error
}

// SyncOnce runs one full pass for every unpaused pair without watching.
// Use it only when no agent is running.
func (a *Agent) SyncOnce() []PairResult {
	a.mu.Lock()
	pairs := append([]config.Pair(nil), a.cfg.Pairs...)
	a.mu.Unlock()
	var out []PairResult
	for _, p := range pairs {
		if p.Paused {
			continue
		}
		r := PairResult{Name: p.Name}
		eng, err := a.engineFor(p, config.ManifestPath(p.ID))
		if err == nil {
			r.Stats, err = eng.Reconcile("", true)
		}
		r.Err = err
		out = append(out, r)
	}
	return out
}
