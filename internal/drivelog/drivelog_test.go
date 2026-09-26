package drivelog

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Lines captured from a real Drive for Desktop log (2026-09-26), account ID replaced.
const (
	acct         = "100000000000000000001"
	downloadLine = "2026-09-26T20:13:59.714ZI [36628:mirror_100000000000000000001_COM] cloud_subsystem.cc:192:OnChangeNotificationReceived Generated 1 download events from 1 changelog entries"
	uploadLine   = "2026-09-26T20:10:36.765ZI [21416:mirror_local_100000000000000000001] local_differ.cc:1081:ProcessObservation Generated 1 upload events"
	scanLine     = "2026-09-26T20:14:00.773ZI [40936:mirror_disk_io_100000000000000000001_COM] tree_scanner.cc:155:ProcessChangeSet Completed scan of 1 paths"
	otherLine    = "2026-09-26T20:10:34.309ZI [23188:core_100000000000000000001] drive_v2_cloud_store.cc:2010:operator() Getting another page of changes for My Drive."
)

func TestParseLine(t *testing.T) {
	ev, ok := ParseLine(downloadLine)
	if !ok || ev.Kind != Download || ev.Account != acct || ev.Time != time.Date(2026, 9, 26, 20, 13, 59, 714e6, time.UTC) {
		t.Errorf("download: %+v %v", ev, ok)
	}
	ev, ok = ParseLine(uploadLine)
	if !ok || ev.Kind != Upload || ev.Account != acct {
		t.Errorf("upload: %+v %v", ev, ok)
	}
	for _, l := range []string{scanLine, otherLine, "", "garbage", "2026-09-26 not a log line"} {
		if _, ok := ParseLine(l); ok {
			t.Errorf("should not match: %q", l)
		}
	}
}

func appendLines(t *testing.T, p string, lines ...string) {
	t.Helper()
	f, err := os.OpenFile(p, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range lines {
		f.WriteString(l + "\r\n")
	}
	f.Close()
}

func line(tm time.Time, thread, msg string) string {
	return tm.UTC().Format("2006-01-02T15:04:05.000") + "ZI [1:" + thread + "] x.cc:1:F " + msg
}

func TestTailFollowsAndRotates(t *testing.T) {
	dir := t.TempDir()
	cur := filepath.Join(dir, "drive_fs.txt")
	now := time.Now().Truncate(time.Millisecond)
	appendLines(t, cur, "partial first line from the middle of the file", line(now.Add(-time.Minute), "core_x", "hello"))

	tail := New(dir)
	tail.Poll()
	if !tail.Covers(now.Add(-30 * time.Second)) {
		t.Fatal("should cover times after the first seen line")
	}
	if tail.Covers(now.Add(-2 * time.Minute)) {
		t.Fatal("should not cover times before the log history it saw")
	}

	d := now.Add(time.Second)
	appendLines(t, cur, line(d, "mirror_"+acct+"_COM", "Generated 1 download events from 1 changelog entries"))
	tail.Poll()
	if !tail.Any(Download, acct, d.Add(-time.Millisecond), d.Add(time.Millisecond)) {
		t.Fatal("download event not seen")
	}
	if tail.Any(Upload, acct, now.Add(-time.Hour), now.Add(time.Hour)) {
		t.Fatal("no upload expected")
	}
	if tail.Any(Download, "someone-else", now.Add(-time.Hour), now.Add(time.Hour)) {
		t.Fatal("other account must not match")
	}

	// A line written in two pieces is only parsed once complete.
	u := now.Add(2 * time.Second)
	full := line(u, "mirror_local_"+acct, "Generated 1 upload events")
	f, _ := os.OpenFile(cur, os.O_APPEND|os.O_WRONLY, 0)
	f.WriteString(full[:40])
	f.Close()
	tail.Poll()
	if tail.Any(Upload, acct, u.Add(-time.Second), u.Add(time.Second)) {
		t.Fatal("partial line parsed")
	}
	f, _ = os.OpenFile(cur, os.O_APPEND|os.O_WRONLY, 0)
	f.WriteString(full[40:] + "\r\n")
	f.Close()
	tail.Poll()
	if !tail.Any(Upload, acct, u.Add(-time.Second), u.Add(time.Second)) {
		t.Fatal("completed line not parsed")
	}

	// Rotation: current file renamed to drive_fs_1.txt after one more line,
	// then a new drive_fs.txt starts. Both lines must be seen.
	r1 := now.Add(3 * time.Second)
	appendLines(t, cur, line(r1, "mirror_local_"+acct, "Generated 2 upload events"))
	if err := os.Rename(cur, filepath.Join(dir, "drive_fs_1.txt")); err != nil {
		t.Fatal(err)
	}
	r2 := now.Add(4 * time.Second)
	appendLines(t, cur, line(r2, "mirror_"+acct+"_COM", "Generated 3 download events from 3 changelog entries"))
	tail.Poll()
	if !tail.Any(Upload, acct, r1, r1) || !tail.Any(Download, acct, r2, r2) {
		t.Fatal("lines around rotation lost")
	}
	if !tail.Covers(r2) {
		t.Fatal("clean rotation should keep coverage")
	}

	// Truncation in place loses coverage until now.
	before := time.Now()
	time.Sleep(5 * time.Millisecond)
	os.WriteFile(cur, []byte("x\r\n"), 0o644)
	tail.Poll()
	if tail.Covers(before) {
		t.Fatal("truncation must drop coverage of earlier times")
	}
}
