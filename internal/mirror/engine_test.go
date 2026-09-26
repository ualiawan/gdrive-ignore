package mirror

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"

	"gdrive-ignore/internal/ignore"
)

type fixture struct {
	t              *testing.T
	src, dst, mani string
}

func newFixture(t *testing.T) *fixture {
	root := t.TempDir()
	f := &fixture{t: t, src: filepath.Join(root, "src"), dst: filepath.Join(root, "dst"), mani: filepath.Join(root, "state", "m.json")}
	if err := os.MkdirAll(f.src, 0o755); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *fixture) write(rel, content string) {
	f.t.Helper()
	p := filepath.Join(f.src, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fixture) engine(mode Mode, rules string) *Engine {
	f.t.Helper()
	e, err := New(Options{
		Source: f.src, Target: f.dst, ManifestPath: f.mani, Mode: mode,
		Rules: ignore.NewSet(ignore.Parse(rules, "pair", "")),
	})
	if err != nil {
		f.t.Fatal(err)
	}
	return e
}

func (f *fixture) sync(e *Engine) Stats {
	f.t.Helper()
	st, err := e.Reconcile("", true)
	if err != nil {
		f.t.Fatal(err)
	}
	if st.ErrorCount > 0 {
		f.t.Fatalf("sync errors: %v", st.Errors)
	}
	return st
}

// tree lists target files and dirs ("dir/") sorted.
func (f *fixture) tree() []string {
	f.t.Helper()
	var out []string
	_ = filepath.WalkDir(f.dst, func(p string, d os.DirEntry, err error) error {
		if err != nil || p == f.dst {
			return nil
		}
		rel, _ := filepath.Rel(f.dst, p)
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

func (f *fixture) wantTree(want ...string) {
	f.t.Helper()
	sort.Strings(want)
	got := f.tree()
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		f.t.Fatalf("target tree:\n got %v\nwant %v", got, want)
	}
}

func (f *fixture) read(rel string) string {
	f.t.Helper()
	b, err := os.ReadFile(filepath.Join(f.dst, filepath.FromSlash(rel)))
	if err != nil {
		f.t.Fatal(err)
	}
	return string(b)
}

func (f *fixture) sameFile(rel string) bool {
	a, err1 := os.Stat(filepath.Join(f.src, rel))
	b, err2 := os.Stat(filepath.Join(f.dst, rel))
	return err1 == nil && err2 == nil && os.SameFile(a, b)
}

func TestMirrorBasics(t *testing.T) {
	for _, mode := range []Mode{ModeHardlink, ModeCopy} {
		t.Run(string(mode), func(t *testing.T) {
			f := newFixture(t)
			f.write("a.txt", "a")
			f.write("debug.log", "log")
			f.write("node_modules/pkg/index.js", "js")
			f.write("app/.driveignore", "secret.txt\n")
			f.write("app/secret.txt", "s")
			f.write("app/main.go", "go")
			f.write("app/sub/x.txt", "x")
			if err := os.MkdirAll(filepath.Join(f.src, "empty"), 0o755); err != nil {
				t.Fatal(err)
			}

			e := f.engine(mode, "*.log\nnode_modules/")
			st := f.sync(e)
			f.wantTree("a.txt", "app/", "app/.driveignore", "app/main.go", "app/sub/", "app/sub/x.txt", "empty/")
			if st.Ignored != 3 {
				t.Errorf("ignored = %d, want 3", st.Ignored)
			}
			if f.read("app/main.go") != "go" {
				t.Error("wrong content")
			}
			if got := f.sameFile("a.txt"); got != (mode == ModeHardlink) {
				t.Errorf("sameFile = %v", got)
			}

			// A second pass does nothing.
			if st := f.sync(e); st.Changed() {
				t.Errorf("second pass changed things: %+v", st)
			}

			// Deleting a source file and adding a rule removes target entries.
			if err := os.Remove(filepath.Join(f.src, "a.txt")); err != nil {
				t.Fatal(err)
			}
			f.write("app/.driveignore", "secret.txt\nsub/\n")
			st = f.sync(e)
			f.wantTree("app/", "app/.driveignore", "app/main.go", "empty/")
			if st.Removed != 3 {
				t.Errorf("removed = %d, want 3", st.Removed)
			}
		})
	}
}

func TestAtomicSaveRelinks(t *testing.T) {
	f := newFixture(t)
	f.write("doc.txt", "v1")
	e := f.engine(ModeHardlink, "")
	f.sync(e)

	// Simulate an editor's atomic save: write a new file and rename it over.
	tmp := filepath.Join(f.src, "doc.txt.tmp")
	if err := os.WriteFile(tmp, []byte("version two"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, filepath.Join(f.src, "doc.txt")); err != nil {
		t.Fatal(err)
	}
	if f.sameFile("doc.txt") {
		t.Fatal("expected link to be broken by atomic save")
	}
	st := f.sync(e)
	if st.Linked != 1 || !f.sameFile("doc.txt") || f.read("doc.txt") != "version two" {
		t.Fatalf("relink failed: %+v content=%q", st, f.read("doc.txt"))
	}
	f.wantTree("doc.txt")
}

// On NTFS the directory entry of the other link name is not refreshed by an
// in-place write, so Drive's watcher would miss it. Reconcile must notice and
// touch the file through the target path.
func TestInPlaceEditIsTouched(t *testing.T) {
	f := newFixture(t)
	f.write("log.txt", "one")
	e := f.engine(ModeHardlink, "")
	f.sync(e)

	time.Sleep(20 * time.Millisecond)
	fh, err := os.OpenFile(filepath.Join(f.src, "log.txt"), os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fh.WriteString(" two"); err != nil {
		t.Fatal(err)
	}
	fh.Close()

	st := f.sync(e)
	if st.Linked != 0 {
		t.Errorf("in-place edit should not relink: %+v", st)
	}
	if runtime.GOOS == "windows" && st.Touched != 1 {
		t.Errorf("touched = %d, want 1 (stale directory entry expected on NTFS)", st.Touched)
	}
	// After touching, the target's directory entry matches the source.
	entries, _ := os.ReadDir(f.dst)
	info, _ := entries[0].Info()
	if info.Size() != int64(len("one two")) {
		t.Errorf("target dir entry size = %d after touch", info.Size())
	}
	if st := f.sync(e); st.Touched != 0 {
		t.Errorf("second pass touched again: %+v", st)
	}
}

func TestCopyModeUpdates(t *testing.T) {
	f := newFixture(t)
	f.write("a.txt", "1")
	e := f.engine(ModeCopy, "")
	f.sync(e)
	time.Sleep(20 * time.Millisecond)
	f.write("a.txt", "22")
	st := f.sync(e)
	if st.Copied != 1 || f.read("a.txt") != "22" {
		t.Fatalf("copy update: %+v", st)
	}
	a, _ := os.Stat(filepath.Join(f.src, "a.txt"))
	b, _ := os.Stat(filepath.Join(f.dst, "a.txt"))
	if !a.ModTime().Equal(b.ModTime()) {
		t.Error("mtime not preserved")
	}
}

func TestForeignFilesAreKept(t *testing.T) {
	f := newFixture(t)
	f.write("a.txt", "a")
	if err := os.MkdirAll(f.dst, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.dst, "mine.txt"), []byte("user"), 0o644); err != nil {
		t.Fatal(err)
	}

	e := f.engine(ModeAuto, "")
	if !e.NeedsAdopt() {
		t.Fatal("expected NeedsAdopt for non-empty target")
	}
	if _, err := e.Reconcile("", true); !errors.Is(err, ErrTargetNotEmpty) {
		t.Fatalf("err = %v, want ErrTargetNotEmpty", err)
	}

	e, err := New(Options{Source: f.src, Target: f.dst, ManifestPath: f.mani, Adopt: true})
	if err != nil {
		t.Fatal(err)
	}
	f.sync(e)
	f.wantTree("a.txt", "mine.txt")

	// Even when rules change, a file we did not create is never deleted.
	if err := os.Remove(filepath.Join(f.src, "a.txt")); err != nil {
		t.Fatal(err)
	}
	f.sync(e)
	f.wantTree("mine.txt")

	// After the first sync, the target counts as ours: no adopt needed.
	e2 := f.engine(ModeAuto, "")
	if e2.NeedsAdopt() {
		t.Error("NeedsAdopt after manifest exists")
	}
}

func TestCaseOnlyRename(t *testing.T) {
	if !ignore.FoldCase {
		t.Skip("case-sensitive file system")
	}
	f := newFixture(t)
	f.write("Report.TXT", "r")
	e := f.engine(ModeHardlink, "")
	f.sync(e)
	if err := os.Rename(filepath.Join(f.src, "Report.TXT"), filepath.Join(f.src, "report.txt")); err != nil {
		t.Fatal(err)
	}
	st := f.sync(e)
	if st.Renamed != 1 {
		t.Errorf("renamed = %d", st.Renamed)
	}
	f.wantTree("report.txt")
}

func TestReadOnlySourceStaysIntact(t *testing.T) {
	f := newFixture(t)
	f.write("ro.txt", "keep")
	p := filepath.Join(f.src, "ro.txt")
	if err := os.Chmod(p, 0o444); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(p, 0o644) })

	e := f.engine(ModeHardlink, "")
	f.sync(e)
	// Ignoring it now must delete the (read-only) link without touching the source.
	e = f.engine(ModeHardlink, "ro.txt")
	f.sync(e)
	f.wantTree()
	fi, err := os.Stat(p)
	if err != nil {
		t.Fatal("source was deleted")
	}
	if fi.Mode().Perm()&0o200 != 0 {
		t.Error("source lost its read-only attribute")
	}
}

func TestNonRecursiveScope(t *testing.T) {
	f := newFixture(t)
	f.write("a/one.txt", "1")
	f.write("a/deep/two.txt", "2")
	e := f.engine(ModeHardlink, "")
	f.sync(e)

	f.write("a/new.txt", "n")
	st, err := e.Reconcile("a", false)
	if err != nil {
		t.Fatal(err)
	}
	if st.Linked != 1 || st.Removed != 0 {
		t.Errorf("non-recursive pass: %+v", st)
	}
	f.wantTree("a/", "a/deep/", "a/deep/two.txt", "a/new.txt", "a/one.txt")

	// A removed subtree is pruned when reconciling its parent.
	if err := os.RemoveAll(filepath.Join(f.src, "a", "deep")); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Reconcile("a", false); err != nil {
		t.Fatal(err)
	}
	f.wantTree("a/", "a/new.txt", "a/one.txt")

	// Reconciling a path that no longer exists prunes it too.
	if err := os.RemoveAll(filepath.Join(f.src, "a")); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Reconcile("a", true); err != nil {
		t.Fatal(err)
	}
	f.wantTree()
}

func TestPreviewIsDry(t *testing.T) {
	f := newFixture(t)
	f.write("a.txt", "a")
	f.write("build/out.bin", "12345")
	f.write("build/more/x.bin", "123")
	e := f.engine(ModeAuto, "build/")
	res, err := e.Preview(true)
	if err != nil {
		t.Fatal(err)
	}
	st, ignored := res.Stats, res.Ignored
	if st.Files != 1 || len(ignored) != 1 {
		t.Fatalf("preview: %+v %+v", st, ignored)
	}
	ig := ignored[0]
	if ig.Path != "build" || !ig.Dir || ig.Size != 8 || ig.Count != 2 || ig.Rule != "build/" {
		t.Errorf("ignored entry: %+v", ig)
	}
	if _, err := os.Stat(f.dst); !errors.Is(err, os.ErrNotExist) {
		t.Error("preview created the target")
	}
	if _, err := os.Stat(f.mani); !errors.Is(err, os.ErrNotExist) {
		t.Error("preview wrote the manifest")
	}
}

func TestValidate(t *testing.T) {
	root := t.TempDir()
	src := filepath.Join(root, "src")
	if err := os.MkdirAll(filepath.Join(src, "inner"), 0o755); err != nil {
		t.Fatal(err)
	}
	bad := []struct{ s, d string }{
		{src, src},
		{src, filepath.Join(src, "inner")},
		{src, strings.ToUpper(filepath.Join(src, "inner", "x"))},
		{filepath.Join(src, "inner"), src},
		{src, filepath.VolumeName(src) + string(filepath.Separator)},
		{filepath.Join(root, "missing"), filepath.Join(root, "dst")},
		{"relative", filepath.Join(root, "dst")},
	}
	for _, c := range bad {
		if c.d == strings.ToUpper(filepath.Join(src, "inner", "x")) && !ignore.FoldCase {
			continue
		}
		if err := Validate(c.s, c.d); err == nil {
			t.Errorf("Validate(%q, %q) should fail", c.s, c.d)
		}
	}
	if err := Validate(src, filepath.Join(root, "dst")); err != nil {
		t.Errorf("valid pair rejected: %v", err)
	}
}

func TestPurgeRemovesOnlyOwned(t *testing.T) {
	f := newFixture(t)
	f.write("a/b.txt", "b")
	f.write("a/.driveignore", "*.bak\n")
	e := f.engine(ModeAuto, "")
	f.sync(e)
	if err := os.WriteFile(filepath.Join(f.dst, "a", "user.txt"), []byte("u"), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err := e.Preview(false)
	if err != nil || len(res.IgnoreFiles) != 1 || res.IgnoreFiles[0] != "a/.driveignore" {
		t.Fatalf("ignore files: %v %v", res.IgnoreFiles, err)
	}
	if _, err := e.Purge(); err != nil {
		t.Fatal(err)
	}
	f.wantTree("a/", "a/user.txt")
	if _, err := os.Stat(f.mani); !errors.Is(err, os.ErrNotExist) {
		t.Error("manifest kept after purge")
	}
	if _, err := os.Stat(filepath.Join(f.src, "a", "b.txt")); err != nil {
		t.Error("purge touched the source")
	}
}

// Switching a pair from hardlink to copy mode must never write through the
// old links into the source.
func TestSwitchToCopyNeverTouchesSource(t *testing.T) {
	f := newFixture(t)
	f.write("a.txt", "original")
	f.sync(f.engine(ModeHardlink, ""))
	if !f.sameFile("a.txt") {
		t.Fatal("expected hardlink")
	}
	time.Sleep(20 * time.Millisecond)
	f.write("b.txt", "new") // something to copy
	// Make the link's directory entry look stale so copy mode rewrites it.
	if err := os.Chtimes(filepath.Join(f.dst, "a.txt"), time.Now(), time.Unix(1, 0)); err != nil {
		t.Fatal(err)
	}
	f.sync(f.engine(ModeCopy, ""))
	b, err := os.ReadFile(filepath.Join(f.src, "a.txt"))
	if err != nil || string(b) != "original" {
		t.Fatalf("source changed: %q %v", b, err)
	}
	if f.sameFile("a.txt") {
		t.Error("target still linked to source in copy mode")
	}
	if f.read("a.txt") != "original" || f.read("b.txt") != "new" {
		t.Error("wrong target content")
	}
}

// Copy mode writes under the final name: no temp files ever appear.
func TestCopyModeUsesNoTempFiles(t *testing.T) {
	f := newFixture(t)
	f.write("a.txt", "1")
	e := f.engine(ModeCopy, "")
	f.sync(e)
	time.Sleep(20 * time.Millisecond)
	f.write("a.txt", "22")
	f.sync(e)
	f.wantTree("a.txt")
}

func TestSameMTime(t *testing.T) {
	src := time.Date(2026, 9, 25, 8, 26, 0, 842149800, time.UTC)
	cases := []struct {
		dst  time.Time
		want bool
	}{
		{src, true},
		{time.Date(2026, 9, 25, 8, 26, 0, 842000000, time.UTC), true},  // Drive virtual drive: ms truncated
		{time.Date(2026, 9, 25, 8, 26, 0, 840000000, time.UTC), true},  // 10 ms
		{time.Date(2026, 9, 25, 8, 26, 0, 0, time.UTC), true},          // 2 s (FAT) truncated
		{time.Date(2026, 9, 25, 8, 26, 2, 0, time.UTC), false},         // 2 s rounding goes to :00, not :02
		{time.Date(2026, 9, 25, 8, 26, 0, 841000000, time.UTC), false}, // 1 ms off: a real change
		{src.Add(time.Second), false},
	}
	for _, c := range cases {
		if got := sameMTime(src, c.dst); got != c.want {
			t.Errorf("sameMTime(%v, %v) = %v, want %v", src, c.dst, got, c.want)
		}
	}
}
