package twoway

import (
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"gdrive-ignore/internal/ignore"
)

// fakeClassifier answers per path (or a default) and records what it saw.
type fakeClassifier struct {
	def   Origin
	by    map[string]Origin
	calls []Deletion
}

func (f *fakeClassifier) Classify(d Deletion) Origin {
	f.calls = append(f.calls, d)
	if o, ok := f.by[d.Path]; ok {
		return o
	}
	return f.def
}

type fx struct {
	t              *testing.T
	root, src, dst string
	bin            string
	cls            *fakeClassifier
	rules          string
	max            int
	adopt          bool
	recycled       []string
}

func newFx(t *testing.T) *fx {
	root := t.TempDir()
	f := &fx{t: t, root: root, src: filepath.Join(root, "src"), dst: filepath.Join(root, "mirror"), bin: filepath.Join(root, "bin"),
		cls: &fakeClassifier{def: OriginUnknown, by: map[string]Origin{}}}
	for _, d := range []string{f.src, f.bin} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return f
}

func (f *fx) engine() *Engine {
	f.t.Helper()
	e, err := New(Options{
		Source: f.src, Target: f.dst, StatePath: filepath.Join(f.root, "state.json"),
		Rules: ignore.NewSet(ignore.Parse(f.rules, "pair", "")), Adopt: f.adopt,
		Classifier: f.cls, MaxAutoDelete: f.max,
		Recycle: func(p string) error {
			rel, _ := filepath.Rel(f.src, p)
			f.recycled = append(f.recycled, filepath.ToSlash(rel))
			return os.Rename(p, filepath.Join(f.bin, strings.ReplaceAll(rel, string(filepath.Separator), "_")))
		},
	})
	if err != nil {
		f.t.Fatal(err)
	}
	return e
}

func (f *fx) sync(e *Engine) Stats {
	f.t.Helper()
	st, err := e.Reconcile()
	if err != nil {
		f.t.Fatal(err)
	}
	if st.ErrorCount > 0 {
		f.t.Fatalf("errors: %v", st.Errors)
	}
	return st
}

func (f *fx) dir(sd side) string {
	if sd == src {
		return f.src
	}
	return f.dst
}

func (f *fx) write(sd side, rel, content string) {
	f.t.Helper()
	p := filepath.Join(f.dir(sd), filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		f.t.Fatal(err)
	}
}

// replace writes new content as a new file and renames it over rel, the way
// editors save and the way Drive applies downloads (new file identity).
func (f *fx) replace(sd side, rel, content string) {
	f.t.Helper()
	p := filepath.Join(f.dir(sd), filepath.FromSlash(rel))
	tmp := p + ".swap"
	if err := os.WriteFile(tmp, []byte(content), 0o644); err != nil {
		f.t.Fatal(err)
	}
	if err := os.Rename(tmp, p); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fx) appendTo(sd side, rel, content string) {
	f.t.Helper()
	time.Sleep(15 * time.Millisecond)
	h, err := os.OpenFile(filepath.Join(f.dir(sd), filepath.FromSlash(rel)), os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		f.t.Fatal(err)
	}
	h.WriteString(content)
	h.Close()
}

func (f *fx) remove(sd side, rel string) {
	f.t.Helper()
	if err := os.RemoveAll(filepath.Join(f.dir(sd), filepath.FromSlash(rel))); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fx) rename(sd side, from, to string) {
	f.t.Helper()
	p := filepath.Join(f.dir(sd), filepath.FromSlash(to))
	os.MkdirAll(filepath.Dir(p), 0o755)
	if err := os.Rename(filepath.Join(f.dir(sd), filepath.FromSlash(from)), p); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fx) read(sd side, rel string) string {
	f.t.Helper()
	b, err := os.ReadFile(filepath.Join(f.dir(sd), filepath.FromSlash(rel)))
	if err != nil {
		f.t.Fatalf("read %s: %v", rel, err)
	}
	return string(b)
}

func (f *fx) exists(sd side, rel string) bool {
	_, err := os.Lstat(filepath.Join(f.dir(sd), filepath.FromSlash(rel)))
	return err == nil
}

func (f *fx) linked(rel string) bool {
	a, err1 := fileID(filepath.Join(f.src, filepath.FromSlash(rel)))
	b, err2 := fileID(filepath.Join(f.dst, filepath.FromSlash(rel)))
	return err1 == nil && err2 == nil && a == b
}

func (f *fx) tree(sd side) []string {
	var out []string
	root := f.dir(sd)
	filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil || p == root {
			return nil
		}
		rel, _ := filepath.Rel(root, p)
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			rel += "/"
		}
		out = append(out, rel)
		return nil
	})
	sort.Strings(out)
	return out
}

func (f *fx) wantBoth(want ...string) {
	f.t.Helper()
	sort.Strings(want)
	for _, sd := range []side{src, dst} {
		got := f.tree(sd)
		if strings.Join(got, "|") != strings.Join(want, "|") {
			name := "source"
			if sd == dst {
				name = "mirror"
			}
			f.t.Fatalf("%s tree:\n got %v\nwant %v", name, got, want)
		}
	}
}

func TestNewFilesFlowBothWays(t *testing.T) {
	f := newFx(t)
	f.write(src, "a.txt", "from pc")
	f.write(src, "docs/b.txt", "from pc")
	e := f.engine()
	st := f.sync(e)
	if st.Pushed != 3 {
		t.Errorf("pushed = %d", st.Pushed)
	}
	f.write(dst, "remote.txt", "from drive")
	f.write(dst, "shared/c.txt", "from drive")
	st = f.sync(e)
	if st.Pulled != 3 {
		t.Errorf("pulled = %d", st.Pulled)
	}
	f.wantBoth("a.txt", "docs/", "docs/b.txt", "remote.txt", "shared/", "shared/c.txt")
	for _, p := range []string{"a.txt", "docs/b.txt", "remote.txt", "shared/c.txt"} {
		if !f.linked(p) {
			t.Errorf("%s not hardlinked", p)
		}
	}
	if st := f.sync(e); st.Changed() {
		t.Errorf("idle pass changed things: %+v", st)
	}
}

func TestEditsFlowBothWays(t *testing.T) {
	f := newFx(t)
	f.write(src, "inplace.txt", "v1")
	f.write(src, "atomic.txt", "v1")
	f.write(src, "remote.txt", "v1")
	e := f.engine()
	f.sync(e)

	f.appendTo(src, "inplace.txt", "+local") // editor writing in place
	f.replace(src, "atomic.txt", "v2 local") // editor saving atomically
	f.replace(dst, "remote.txt", "v2 drive") // Drive downloading a new version
	st := f.sync(e)
	if st.Touched != 1 || st.Pushed != 1 || st.Pulled != 1 || st.Conflicts != 0 {
		t.Fatalf("stats: %+v", st)
	}
	for p, want := range map[string]string{"inplace.txt": "v1+local", "atomic.txt": "v2 local", "remote.txt": "v2 drive"} {
		if f.read(src, p) != want || f.read(dst, p) != want || !f.linked(p) {
			t.Errorf("%s: src=%q dst=%q linked=%v", p, f.read(src, p), f.read(dst, p), f.linked(p))
		}
	}
	// Drive writing in place changes both names at once; nothing to do.
	f.appendTo(dst, "remote.txt", "+drive")
	st = f.sync(e)
	if st.Pushed+st.Pulled+st.Conflicts != 0 || f.read(src, "remote.txt") != "v2 drive+drive" {
		t.Fatalf("in-place remote edit: %+v", st)
	}
	if st := f.sync(e); st.Changed() {
		t.Errorf("idle pass changed things: %+v", st)
	}
}

func TestSourceDeleteRemovesFromMirror(t *testing.T) {
	f := newFx(t)
	f.write(src, "keep.txt", "k")
	f.write(src, "gone.txt", "g")
	f.write(src, "folder/x.txt", "x")
	e := f.engine()
	f.sync(e)
	f.remove(src, "gone.txt")
	f.remove(src, "folder")
	st := f.sync(e)
	if st.DeletedInDrive != 3 || len(f.cls.calls) != 0 {
		t.Fatalf("stats %+v, classifier calls %v", st, f.cls.calls)
	}
	f.wantBoth("keep.txt")
}

func TestDriveFirstDeleteGoesToRecycleBin(t *testing.T) {
	f := newFx(t)
	f.write(src, "a.txt", "a")
	f.write(src, "dir/b.txt", "b")
	f.write(src, "stay.txt", "s")
	e := f.engine()
	f.sync(e)
	f.cls.def = OriginDrive
	f.remove(dst, "a.txt")
	f.remove(dst, "dir")
	st := f.sync(e)
	if st.DeletedHere != 3 {
		t.Fatalf("stats %+v", st)
	}
	sort.Strings(f.recycled)
	if strings.Join(f.recycled, ",") != "a.txt,dir" {
		t.Fatalf("recycled %v", f.recycled)
	}
	f.wantBoth("stay.txt")
}

func TestHandDeleteInMirrorIsRestored(t *testing.T) {
	f := newFx(t)
	f.write(src, "a.txt", "a")
	f.write(src, "dir/b.txt", "b")
	e := f.engine()
	f.sync(e)
	before, _ := fileID(filepath.Join(f.src, "a.txt"))
	f.cls.def = OriginMirror
	f.remove(dst, "a.txt")
	f.remove(dst, "dir")
	st := f.sync(e)
	if st.Restored != 3 || len(f.recycled) != 0 || st.DeletedHere != 0 {
		t.Fatalf("stats %+v recycled %v", st, f.recycled)
	}
	f.wantBoth("a.txt", "dir/", "dir/b.txt")
	after, _ := fileID(filepath.Join(f.src, "a.txt"))
	if before != after || !f.linked("a.txt") || f.read(src, "dir/b.txt") != "b" {
		t.Error("source must be untouched and relinked")
	}
}

func TestUncertainDeleteWaitsForDecision(t *testing.T) {
	f := newFx(t)
	f.write(src, "a.txt", "a")
	f.write(src, "b.txt", "b")
	e := f.engine()
	f.sync(e)
	f.remove(dst, "a.txt")
	f.remove(dst, "b.txt")
	for i := 0; i < 2; i++ {
		st := f.sync(e)
		if st.Pending != 2 || st.DeletedHere != 0 || st.Restored != 0 {
			t.Fatalf("pass %d: %+v", i, st)
		}
	}
	if !f.exists(src, "a.txt") || !f.exists(src, "b.txt") || f.exists(dst, "a.txt") {
		t.Fatal("nothing may change while a decision is pending")
	}
	if p := e.Pending(); len(p) != 2 || p[0].Path != "a.txt" {
		t.Fatalf("pending %+v", p)
	}
	// State survives a restart.
	e = f.engine()
	if len(e.Pending()) != 2 {
		t.Fatal("pending lost on restart")
	}
	if err := e.Decide("a.txt", DecideRestore); err != nil {
		t.Fatal(err)
	}
	if err := e.Decide("b.txt", DecideDelete); err != nil {
		t.Fatal(err)
	}
	st := f.sync(e)
	if st.Restored != 1 || st.DeletedHere != 1 || st.Pending != 0 {
		t.Fatalf("after decisions: %+v", st)
	}
	f.wantBoth("a.txt")
	if len(f.recycled) != 1 || f.recycled[0] != "b.txt" {
		t.Fatalf("recycled %v", f.recycled)
	}
}

func TestWaitDefersDecision(t *testing.T) {
	f := newFx(t)
	f.write(src, "a.txt", "a")
	e := f.engine()
	f.sync(e)
	f.cls.def = OriginWait
	f.remove(dst, "a.txt")
	e.NoteGone("a.txt", time.Now())
	st := f.sync(e)
	if st.NextCheck.IsZero() || st.Pending != 0 || !f.exists(src, "a.txt") || f.exists(dst, "a.txt") {
		t.Fatalf("wait: %+v", st)
	}
	if got := f.cls.calls[0]; got.Gone.IsZero() || got.Path != "a.txt" {
		t.Fatalf("classifier input %+v", got)
	}
}

func TestEditBeatsDelete(t *testing.T) {
	f := newFx(t)
	f.write(src, "local-edit.txt", "v1")
	f.write(src, "remote-edit.txt", "v1")
	e := f.engine()
	f.sync(e)
	f.cls.def = OriginDrive
	f.replace(src, "local-edit.txt", "v2 local")
	f.remove(dst, "local-edit.txt") // deleted in Drive
	f.replace(dst, "remote-edit.txt", "v2 drive")
	f.remove(src, "remote-edit.txt") // deleted on the PC
	st := f.sync(e)
	if len(f.recycled) != 0 || st.DeletedInDrive != 0 {
		t.Fatalf("edits must win: %+v recycled %v", st, f.recycled)
	}
	f.wantBoth("local-edit.txt", "remote-edit.txt")
	if f.read(dst, "local-edit.txt") != "v2 local" || f.read(src, "remote-edit.txt") != "v2 drive" {
		t.Fatal("wrong content")
	}
}

func TestDriveFolderDeleteKeepsLocalChanges(t *testing.T) {
	f := newFx(t)
	f.write(src, "proj/old.txt", "o")
	f.write(src, "proj/edited.txt", "v1")
	f.write(src, "proj/sub/untouched.txt", "u")
	e := f.engine()
	f.sync(e)
	f.cls.def = OriginDrive
	f.replace(src, "proj/edited.txt", "v2")
	f.write(src, "proj/new.txt", "n")
	f.remove(dst, "proj")
	f.sync(e)
	f.wantBoth("proj/", "proj/edited.txt", "proj/new.txt")
	sort.Strings(f.recycled)
	if strings.Join(f.recycled, ",") != filepath.ToSlash("proj/old.txt,proj/sub") {
		t.Fatalf("recycled %v", f.recycled)
	}
}

func TestConflictKeepsBothVersions(t *testing.T) {
	f := newFx(t)
	f.write(src, "notes.txt", "v1")
	e := f.engine()
	e.opt.Now = func() time.Time { return time.Date(2026, 9, 26, 22, 0, 0, 0, time.Local) }
	f.sync(e)
	f.replace(dst, "notes.txt", "drive version")
	time.Sleep(20 * time.Millisecond)
	f.replace(src, "notes.txt", "pc version (newer)")
	st := f.sync(e)
	if st.Conflicts != 1 {
		t.Fatalf("stats %+v", st)
	}
	copyName := "notes (conflict 2026-09-26 220000).txt"
	f.wantBoth("notes.txt", copyName)
	if f.read(src, "notes.txt") != "pc version (newer)" || f.read(dst, copyName) != "drive version" {
		t.Fatalf("contents: %q / %q", f.read(src, "notes.txt"), f.read(dst, copyName))
	}
	if !f.linked("notes.txt") || !f.linked(copyName) {
		t.Fatal("both versions must be linked on both sides")
	}
}

func TestRenamesFlowBothWays(t *testing.T) {
	f := newFx(t)
	f.rules = "node_modules/"
	f.write(src, "a.txt", "a")
	f.write(src, "b.txt", "b")
	f.write(src, "projA/main.go", "m")
	f.write(src, "projA/node_modules/dep.js", "d") // ignored, lives only in the source
	f.write(src, "projB/lib.go", "l")
	e := f.engine()
	f.sync(e)

	f.rename(src, "a.txt", "renamed-here.txt")
	f.rename(dst, "b.txt", "renamed-online.txt")   // Drive renames in place (same file)
	f.rename(src, "projA", "projA-renamed")        // folder renamed on the PC
	f.rename(dst, "projB", "projB-renamed-online") // folder renamed online
	st := f.sync(e)
	if st.Renamed != 4 || st.Pending != 0 || len(f.cls.calls) != 0 || len(f.recycled) != 0 {
		t.Fatalf("stats %+v calls %v recycled %v", st, f.cls.calls, f.recycled)
	}
	want := []string{"renamed-here.txt", "renamed-online.txt", "projA-renamed/", "projA-renamed/main.go", "projB-renamed-online/", "projB-renamed-online/lib.go"}
	sort.Strings(want)
	if got := f.tree(dst); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("mirror %v", got)
	}
	if !f.exists(src, "projA-renamed/node_modules/dep.js") || !f.linked("projB-renamed-online/lib.go") {
		t.Fatal("source folder rename lost content")
	}
}

func TestIgnoredNeverCrossEitherWay(t *testing.T) {
	f := newFx(t)
	f.rules = "node_modules/\n*.log"
	f.write(src, "app.js", "a")
	f.write(src, "node_modules/x.js", "local dep")
	f.write(src, "debug.log", "l")
	e := f.engine()
	f.sync(e)
	f.write(dst, "node_modules/remote.js", "from another pc") // uploaded elsewhere
	f.write(dst, "remote.log", "r")
	f.sync(e)
	if f.exists(dst, "node_modules/x.js") || f.exists(dst, "debug.log") {
		t.Fatal("ignored source files pushed")
	}
	if f.exists(src, "node_modules/remote.js") || f.exists(src, "remote.log") {
		t.Fatal("ignored remote files pulled")
	}
	// A rule added later never deletes anything: the file just stops syncing.
	f.write(src, "big.bin", "b")
	f.sync(e)
	f.rules = "node_modules/\n*.log\n*.bin"
	e = f.engine()
	st := f.sync(e)
	if !f.exists(dst, "big.bin") || !f.exists(src, "big.bin") || st.DeletedInDrive+st.DeletedHere != 0 {
		t.Fatalf("newly ignored file was deleted: %+v", st)
	}
}

func TestFirstSyncMergesNonEmptyTarget(t *testing.T) {
	f := newFx(t)
	f.write(src, "same.txt", "identical")
	f.write(src, "differs.txt", "pc")
	f.write(src, "pc-only.txt", "p")
	f.write(dst, "same.txt", "identical")
	f.write(dst, "differs.txt", "drive")
	f.write(dst, "drive-only.txt", "d")
	e := f.engine()
	if !e.NeedsAdopt() {
		t.Fatal("expected adopt check")
	}
	if _, err := e.Reconcile(); !errors.Is(err, ErrTargetNotEmpty) {
		t.Fatalf("err %v", err)
	}
	f.adopt = true
	e = f.engine()
	st := f.sync(e)
	if st.Conflicts != 1 || len(f.recycled) != 0 || st.DeletedInDrive != 0 {
		t.Fatalf("merge: %+v", st)
	}
	names := f.tree(src)
	if len(names) != 5 || !f.linked("same.txt") || !f.linked("pc-only.txt") || !f.linked("drive-only.txt") {
		t.Fatalf("merged tree %v", names)
	}
}

func TestLargeDriveDeleteAsks(t *testing.T) {
	f := newFx(t)
	f.max = 3
	for _, n := range []string{"1", "2", "3", "4", "5"} {
		f.write(src, "f"+n+".txt", n)
	}
	e := f.engine()
	f.sync(e)
	f.cls.def = OriginDrive
	for _, n := range []string{"1", "2", "3", "4", "5"} {
		f.remove(dst, "f"+n+".txt")
	}
	st := f.sync(e)
	if st.DeletedHere != 3 || st.Pending != 2 {
		t.Fatalf("stats %+v", st)
	}
	if err := e.Decide("", DecideDelete); err != nil {
		t.Fatal(err)
	}
	st = f.sync(e)
	if st.DeletedHere != 2 || st.Pending != 0 || len(f.tree(src)) != 0 {
		t.Fatalf("after confirm: %+v %v", st, f.tree(src))
	}
}

func TestReadOnlySourceFileStaysReadOnly(t *testing.T) {
	f := newFx(t)
	f.write(src, "ro.txt", "keep")
	p := filepath.Join(f.src, "ro.txt")
	os.Chmod(p, 0o444)
	t.Cleanup(func() { os.Chmod(p, 0o644) })
	e := f.engine()
	f.sync(e)
	os.Chmod(filepath.Join(f.src, "ro.txt"), 0o444)
	f.remove(src, "ro.txt") // deleting the source removes the mirror name only
	f.write(src, "ro2.txt", "x")
	f.sync(e)
	f.wantBoth("ro2.txt")
}

func TestConflictName(t *testing.T) {
	tm := time.Date(2026, 1, 2, 3, 4, 5, 0, time.Local)
	for in, want := range map[string]string{
		"a/report.docx": "a/report (conflict 2026-01-02 030405).docx",
		".env":          ".env (conflict 2026-01-02 030405)",
		"Makefile":      "Makefile (conflict 2026-01-02 030405)",
	} {
		if got := conflictName(in, tm); got != want {
			t.Errorf("%s -> %s, want %s", in, got, want)
		}
	}
}

// Drive for Desktop marks folders in computer folders read-only; deleting a
// folder in the source must still remove it from the mirror.
func TestSourceFolderDeleteRemovesReadOnlyMirrorFolder(t *testing.T) {
	f := newFx(t)
	f.write(src, "proj/sub/a.txt", "a")
	e := f.engine()
	f.sync(e)
	for _, d := range []string{"proj", "proj/sub"} {
		if err := setReadOnlyDir(filepath.Join(f.dst, filepath.FromSlash(d))); err != nil {
			t.Fatal(err)
		}
	}
	f.remove(src, "proj")
	st := f.sync(e)
	if st.ErrorCount != 0 {
		t.Fatalf("errors: %v", st.Errors)
	}
	f.wantBoth()
}
