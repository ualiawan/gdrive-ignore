package origin

import (
	"errors"
	"testing"
	"time"

	"gdrive-ignore/internal/drivelog"
	"gdrive-ignore/internal/twoway"
)

type fakeLog struct {
	from   time.Time // covered from
	events []drivelog.Event
}

func (f *fakeLog) Poll()                   {}
func (f *fakeLog) Covers(t time.Time) bool { return !t.Before(f.from) }
func (f *fakeLog) Any(k drivelog.Kind, a string, from, to time.Time) bool {
	for _, e := range f.events {
		if e.Kind == k && e.Account == a && !e.Time.Before(from) && !e.Time.After(to) {
			return true
		}
	}
	return false
}

const acct = "100000000000000000001"

func TestClassify(t *testing.T) {
	gone := time.Date(2026, 9, 26, 20, 13, 59, 762e6, time.UTC)
	later := gone.Add(10 * time.Second)
	ev := func(k drivelog.Kind, d time.Duration) drivelog.Event {
		return drivelog.Event{Kind: k, Account: acct, Time: gone.Add(d)}
	}
	trashed := func([]int64) (bool, error) { return true, nil }
	notTrashed := func([]int64) (bool, error) { return false, nil }
	dbErr := func([]int64) (bool, error) { return false, errors.New("locked") }

	cases := []struct {
		name    string
		now     time.Time
		gone    time.Time
		from    time.Time
		events  []drivelog.Event
		deleted func([]int64) (bool, error)
		want    twoway.Origin
	}{
		// Real timings from the experiment.
		{"drive-first (online delete)", later, gone, gone.Add(-time.Hour),
			[]drivelog.Event{ev(drivelog.Download, -48*time.Millisecond)}, trashed, twoway.OriginDrive},
		{"mirror-first (hand delete)", later, gone, gone.Add(-time.Hour),
			[]drivelog.Event{ev(drivelog.Upload, 1100*time.Millisecond)}, notTrashed, twoway.OriginMirror},
		{"mirror-first even if Drive already trashed it", later, gone, gone.Add(-time.Hour),
			[]drivelog.Event{ev(drivelog.Upload, 1100*time.Millisecond)}, trashed, twoway.OriginMirror},

		// Everything below must never become OriginDrive.
		{"too early to decide", gone.Add(2 * time.Second), gone, gone.Add(-time.Hour), nil, trashed, twoway.OriginWait},
		{"no timestamp", later, time.Time{}, gone.Add(-time.Hour), nil, trashed, twoway.OriginUnknown},
		{"log not covering that time", later, gone, gone.Add(-time.Second),
			[]drivelog.Event{ev(drivelog.Download, -48*time.Millisecond)}, trashed, twoway.OriginUnknown},
		{"download but Drive does not show it trashed (offline/paused)", later, gone, gone.Add(-time.Hour),
			[]drivelog.Event{ev(drivelog.Download, -48*time.Millisecond)}, notTrashed, twoway.OriginUnknown},
		{"download but database unreadable", later, gone, gone.Add(-time.Hour),
			[]drivelog.Event{ev(drivelog.Download, -48*time.Millisecond)}, dbErr, twoway.OriginUnknown},
		{"both download and upload around it", later, gone, gone.Add(-time.Hour),
			[]drivelog.Event{ev(drivelog.Download, -48*time.Millisecond), ev(drivelog.Upload, time.Second)}, trashed, twoway.OriginUnknown},
		{"no Drive activity at all", later, gone, gone.Add(-time.Hour), nil, trashed, twoway.OriginUnknown},
		{"download long before is unrelated", later, gone, gone.Add(-time.Hour),
			[]drivelog.Event{ev(drivelog.Download, -10*time.Second)}, trashed, twoway.OriginUnknown},
		{"other account's events", later, gone, gone.Add(-time.Hour),
			[]drivelog.Event{{Kind: drivelog.Download, Account: "other", Time: gone.Add(-48 * time.Millisecond)}}, trashed, twoway.OriginUnknown},
	}
	for _, c := range cases {
		ch := &Checker{Log: &fakeLog{from: c.from, events: c.events}, Account: acct, RemoteDeleted: c.deleted,
			Now: func() time.Time { return c.now }}
		got := ch.Classify(twoway.Deletion{Path: "x.txt", Gone: c.gone, DriveIDs: []int64{1}})
		if got != c.want {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
	var nilChecker *Checker
	if nilChecker.Classify(twoway.Deletion{Gone: gone}) != twoway.OriginUnknown {
		t.Error("nil checker must be uncertain")
	}
}
