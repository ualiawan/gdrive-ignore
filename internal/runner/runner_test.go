package runner

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"gdrive-ignore/internal/ignore"
	"gdrive-ignore/internal/mirror"
)

func write(t *testing.T, root, rel, content string) {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for: %s", what)
}

func exists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

func content(p string) string {
	b, _ := os.ReadFile(p)
	return string(b)
}

func TestRunnerFollowsChanges(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("watcher is windows only for now")
	}
	root := t.TempDir()
	src, dst := filepath.Join(root, "src"), filepath.Join(root, "dst")
	write(t, src, "keep.txt", "k")
	write(t, src, "node_modules/x.js", "x")

	eng, err := mirror.New(mirror.Options{
		Source: src, Target: dst, ManifestPath: filepath.Join(root, "m.json"),
		Rules: ignore.NewSet(ignore.Parse("node_modules/", "global", "")),
	})
	if err != nil {
		t.Fatal(err)
	}
	r := Start(eng, src, Options{Debounce: 50 * time.Millisecond, MaxWait: 200 * time.Millisecond})
	defer r.Stop()

	eventually(t, "initial sync", func() bool { return exists(filepath.Join(dst, "keep.txt")) })
	eventually(t, "watching", func() bool { return r.Status().Watching && r.Status().State == StateIdle })
	if exists(filepath.Join(dst, "node_modules")) {
		t.Fatal("ignored dir mirrored")
	}

	// New nested file.
	write(t, src, "a/b/new.txt", "n")
	eventually(t, "new nested file", func() bool { return content(filepath.Join(dst, "a", "b", "new.txt")) == "n" })

	// Atomic save.
	write(t, src, "keep.txt.tmp", "v2")
	if err := os.Rename(filepath.Join(src, "keep.txt.tmp"), filepath.Join(src, "keep.txt")); err != nil {
		t.Fatal(err)
	}
	eventually(t, "atomic save", func() bool { return content(filepath.Join(dst, "keep.txt")) == "v2" })

	// A new .driveignore removes matching files.
	write(t, src, "a/.driveignore", "b/\n")
	eventually(t, "rule applied", func() bool { return !exists(filepath.Join(dst, "a", "b")) })

	// Renaming a folder moves its content.
	if err := os.Rename(filepath.Join(src, "a"), filepath.Join(src, "renamed")); err != nil {
		t.Fatal(err)
	}
	eventually(t, "folder rename", func() bool {
		return !exists(filepath.Join(dst, "a")) && exists(filepath.Join(dst, "renamed", ".driveignore"))
	})

	// A storm inside an ignored dir does not trigger work.
	before := r.Status().LastSync
	for i := 0; i < 300; i++ {
		write(t, src, filepath.ToSlash(filepath.Join("node_modules", "pkg", "f"+string(rune('a'+i%26))+".js")), "x")
	}
	time.Sleep(400 * time.Millisecond)
	if !r.Status().LastSync.Equal(before) {
		t.Error("events inside an ignored dir triggered a sync")
	}

	// Paused: changes wait until resumed.
	r.SetPaused(true)
	write(t, src, "later.txt", "l")
	time.Sleep(400 * time.Millisecond)
	if exists(filepath.Join(dst, "later.txt")) {
		t.Fatal("synced while paused")
	}
	r.SetPaused(false)
	eventually(t, "resume", func() bool { return exists(filepath.Join(dst, "later.txt")) })
}

func TestRunnerNeedsAdopt(t *testing.T) {
	root := t.TempDir()
	src, dst := filepath.Join(root, "src"), filepath.Join(root, "dst")
	write(t, src, "a.txt", "a")
	write(t, dst, "existing.txt", "e")
	eng, err := mirror.New(mirror.Options{Source: src, Target: dst, ManifestPath: filepath.Join(root, "m.json")})
	if err != nil {
		t.Fatal(err)
	}
	r := Start(eng, src, Options{})
	defer r.Stop()
	eventually(t, "needs-adopt state", func() bool { return r.Status().State == StateNeedsAdopt })
	if exists(filepath.Join(dst, "a.txt")) {
		t.Fatal("synced into non-empty target without adopt")
	}
	if _, err := eng.Reconcile("", true); !errors.Is(err, mirror.ErrTargetNotEmpty) {
		t.Fatal(err)
	}
}

func TestCoalesce(t *testing.T) {
	got := coalesce(map[string]bool{"a": true, "a/b": false, "a/c": true, "x": false, "ab": false})
	want := []job{{"a", true}, {"ab", false}, {"x", false}}
	if len(got) != len(want) {
		t.Fatalf("got %v", got)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
	if got := coalesce(map[string]bool{"": true, "a": false}); len(got) != 1 || got[0].rel != "" {
		t.Fatalf("root recursive should cover all: %v", got)
	}
}
