// Package drivelog follows Google Drive for Desktop's log file and records
// when Drive applied changes from the cloud ("download events") and when it
// pushed local changes up ("upload events") for computer folders.
//
// The tool uses the order of these events to tell a deletion that started
// in Drive (cloud change applied first, then the local file disappears) from
// one made by hand in the mirror (local file disappears first, then Drive
// uploads the deletion). Anything it cannot see clearly is reported as not
// covered, and callers must then treat the situation as uncertain.
package drivelog

import (
	"bufio"
	"bytes"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

// Kind of Drive event.
type Kind int

const (
	Download Kind = iota // a cloud change is being applied locally
	Upload               // a local change is being pushed to the cloud
)

// keep is how long events are remembered.
const keep = 30 * time.Minute

// seed is how much of the existing log is read at start to learn history.
const seed = 512 << 10

var lineRE = regexp.MustCompile(`^(\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}\.\d{3})Z[A-Z] \[\d+:([^\]]*)\] \S+ (.*)$`)

// Event is one recognized log line.
type Event struct {
	Time    time.Time
	Kind    Kind
	Account string
}

// ParseLine recognizes the two event lines. ok is false for anything else.
func ParseLine(line string) (ev Event, ok bool) {
	m := lineRE.FindStringSubmatch(line)
	if m == nil {
		return Event{}, false
	}
	t, err := time.Parse("2006-01-02T15:04:05.000", m[1])
	if err != nil {
		return Event{}, false
	}
	thread, msg := m[2], m[3]
	switch {
	case strings.HasPrefix(thread, "mirror_local_") && strings.Contains(msg, "Generated") && strings.Contains(msg, "upload events"):
		return Event{Time: t.UTC(), Kind: Upload, Account: strings.TrimPrefix(thread, "mirror_local_")}, true
	case strings.HasPrefix(thread, "mirror_") && strings.HasSuffix(thread, "_COM") && !strings.Contains(thread, "disk_io") &&
		strings.Contains(msg, "Generated") && strings.Contains(msg, "download events"):
		acct := strings.TrimSuffix(strings.TrimPrefix(thread, "mirror_"), "_COM")
		return Event{Time: t.UTC(), Kind: Download, Account: acct}, true
	}
	return Event{}, false
}

// isTimestamped reports whether a line has Drive's log timestamp format.
func isTimestamped(line string) bool { return lineRE.MatchString(line) }

// Tail follows drive_fs.txt.
type Tail struct {
	dir string

	mu          sync.Mutex
	events      []Event
	coveredFrom time.Time // events are complete from this time on (zero = never)
	lastPoll    time.Time
	file        os.FileInfo // identity of the file being read
	off         int64
	partial     []byte

	stop chan struct{}
	done chan struct{}
}

// LogDir is Drive for Desktop's log folder.
func LogDir() string {
	return filepath.Join(os.Getenv("LOCALAPPDATA"), "Google", "DriveFS", "Logs")
}

// New creates a tail on dir (use LogDir) without starting it.
func New(dir string) *Tail {
	return &Tail{dir: dir, stop: make(chan struct{}), done: make(chan struct{})}
}

// Start begins following the log in the background.
func (t *Tail) Start() {
	t.Poll()
	go func() {
		defer close(t.done)
		tick := time.NewTicker(200 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-t.stop:
				return
			case <-tick.C:
				t.Poll()
			}
		}
	}()
}

// Stop ends the background polling.
func (t *Tail) Stop() {
	select {
	case <-t.stop:
	default:
		close(t.stop)
		<-t.done
	}
}

func (t *Tail) current() string { return filepath.Join(t.dir, "drive_fs.txt") }

// Poll reads everything new in the log now.
func (t *Tail) Poll() {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := time.Now()
	defer func() { t.lastPoll = now }()

	fi, err := identity(t.current())
	if err != nil {
		t.lose()
		return
	}
	switch {
	case t.file == nil:
		// First look: learn recent history from the end of the file.
		t.file = fi
		t.off = max(fi.Size()-seed, 0)
		first := t.readFrom(t.current(), true)
		if !first.IsZero() {
			t.coveredFrom = first
		} else {
			t.coveredFrom = now
		}
	case !os.SameFile(fi, t.file):
		// Rotated: the file we were reading was renamed. Finish it first.
		if old := t.findRotated(); old != "" {
			t.readFrom(old, false)
		} else {
			t.lose()
		}
		t.file, t.off, t.partial = fi, 0, nil
		t.readFrom(t.current(), false)
	case fi.Size() < t.off:
		// Truncated in place: we may have missed lines.
		t.lose()
		t.file, t.off, t.partial = fi, 0, nil
		t.readFrom(t.current(), false)
	default:
		t.file = fi
		t.readFrom(t.current(), false)
	}
	t.prune(now)
}

// lose records that events may have been missed until now.
func (t *Tail) lose() {
	t.coveredFrom = time.Now()
	t.file, t.off, t.partial = nil, 0, nil
}

// findRotated returns the path the previously read file was renamed to.
func (t *Tail) findRotated() string {
	for i := 1; i <= 3; i++ {
		p := filepath.Join(t.dir, "drive_fs_"+itoa(i)+".txt")
		if fi, err := identity(p); err == nil && os.SameFile(fi, t.file) {
			return p
		}
	}
	return ""
}

// readFrom reads new complete lines from path starting at t.off. With
// skipFirst, the first (probably partial) line is dropped. It returns the
// time of the first timestamped line read.
func (t *Tail) readFrom(path string, skipFirst bool) (first time.Time) {
	f, err := os.Open(path)
	if err != nil {
		t.lose()
		return
	}
	defer f.Close()
	if _, err := f.Seek(t.off, io.SeekStart); err != nil {
		t.lose()
		return
	}
	data, err := io.ReadAll(f)
	if err != nil {
		t.lose()
		return
	}
	t.off += int64(len(data))
	data = append(t.partial, data...)
	t.partial = nil
	if i := bytes.LastIndexByte(data, '\n'); i < len(data)-1 {
		t.partial = append([]byte(nil), data[i+1:]...)
		data = data[:i+1]
	}
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(make([]byte, 64<<10), 4<<20)
	for sc.Scan() {
		line := strings.TrimRight(sc.Text(), "\r")
		if skipFirst {
			skipFirst = false
			continue
		}
		if first.IsZero() && isTimestamped(line) {
			first, _ = time.Parse("2006-01-02T15:04:05.000", line[:23])
		}
		if ev, ok := ParseLine(line); ok {
			t.events = append(t.events, ev)
		}
	}
	return first
}

func (t *Tail) prune(now time.Time) {
	cut := now.Add(-keep)
	i := 0
	for i < len(t.events) && t.events[i].Time.Before(cut) {
		i++
	}
	t.events = t.events[i:]
}

// Covers reports whether events are known to be complete at time tm
// (the tail was running and saw the log around then).
func (t *Tail) Covers(tm time.Time) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return !t.coveredFrom.IsZero() && !tm.Before(t.coveredFrom)
}

// Any reports whether an event of kind for account happened in [from, to].
func (t *Tail) Any(kind Kind, account string, from, to time.Time) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, ev := range t.events {
		if ev.Kind == kind && ev.Account == account && !ev.Time.Before(from) && !ev.Time.After(to) {
			return true
		}
	}
	return false
}

func itoa(i int) string { return string(rune('0' + i)) }

// identity returns FileInfo read through an open handle. On Windows, os.Stat
// records only the path and resolves the file ID lazily, so comparing an old
// Stat result after a rename would compare the new file with itself.
func identity(path string) (os.FileInfo, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return f.Stat()
}
