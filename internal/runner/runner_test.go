package runner

import (
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	"gdrive-ignore/internal/ignore"
	"gdrive-ignore/internal/twoway"
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
	deadline := time.Now().Add(15 * time.Second)
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

// recorder is a classifier that answers "deleted by hand in the mirror" once
// a vanish time is known, and records what it was asked.
type recorder struct {
	mu    sync.Mutex
	calls []twoway.Deletion
}

func (c *recorder) Classify(d twoway.Deletion) twoway.Origin {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls = append(c.calls, d)
	if d.Gone.IsZero() {
		return twoway.OriginUnknown
	}
	if time.Since(d.Gone) < 300*time.Millisecond {
		return twoway.OriginWait
	}
	return twoway.OriginMirror
}

func TestRunnerTwoWay(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("watcher is windows only for now")
	}
	root := t.TempDir()
	src, dst := filepath.Join(root, "src"), filepath.Join(root, "mirror")
	write(t, src, "keep.txt", "k")
	write(t, src, "node_modules/x.js", "x")
	cls := &recorder{}
	eng, err := twoway.New(twoway.Options{
		Source: src, Target: dst, StatePath: filepath.Join(root, "state.json"),
		Rules:      ignore.NewSet(ignore.Parse("node_modules/", "global", "")),
		Classifier: cls,
		Recycle:    func(string) error { t.Error("nothing should be recycled"); return nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	r := Start(eng, src, dst, Options{Debounce: 50 * time.Millisecond, MaxWait: 200 * time.Millisecond})
	defer r.Stop()

	eventually(t, "initial sync", func() bool { return exists(filepath.Join(dst, "keep.txt")) })
	eventually(t, "watching", func() bool { return r.Status().Watching && r.Status().State == StateIdle })
	if exists(filepath.Join(dst, "node_modules")) {
		t.Fatal("ignored folder pushed")
	}

	// PC → Drive.
	write(t, src, "a/b/new.txt", "n")
	eventually(t, "push", func() bool { return content(filepath.Join(dst, "a", "b", "new.txt")) == "n" })

	// Drive → PC: a file downloaded into the mirror.
	write(t, dst, "from-drive.txt", "d")
	eventually(t, "pull", func() bool { return content(filepath.Join(src, "from-drive.txt")) == "d" })

	// Drive replaces a file (new version downloaded).
	tmp := filepath.Join(dst, "keep.txt.dl")
	os.WriteFile(tmp, []byte("v2 from drive"), 0o644)
	os.Rename(tmp, filepath.Join(dst, "keep.txt"))
	eventually(t, "remote edit", func() bool { return content(filepath.Join(src, "keep.txt")) == "v2 from drive" })

	// Hand deletion in the mirror: the runner must timestamp it, wait for
	// evidence, and restore it; the source is never touched.
	os.Remove(filepath.Join(dst, "from-drive.txt"))
	eventually(t, "restored", func() bool { return exists(filepath.Join(dst, "from-drive.txt")) })
	if !exists(filepath.Join(src, "from-drive.txt")) {
		t.Fatal("source lost the file")
	}
	cls.mu.Lock()
	sawTime := len(cls.calls) > 0 && !cls.calls[len(cls.calls)-1].Gone.IsZero()
	cls.mu.Unlock()
	if !sawTime {
		t.Fatal("classifier did not receive a vanish time")
	}

	// Deleting in the source deletes from the mirror.
	os.Remove(filepath.Join(src, "a", "b", "new.txt"))
	eventually(t, "source delete", func() bool { return !exists(filepath.Join(dst, "a", "b", "new.txt")) })

	// Paused: nothing moves until resumed.
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
	src, dst := filepath.Join(root, "src"), filepath.Join(root, "mirror")
	write(t, src, "a.txt", "a")
	write(t, dst, "existing.txt", "e")
	eng, err := twoway.New(twoway.Options{Source: src, Target: dst, StatePath: filepath.Join(root, "s.json"),
		Recycle: func(string) error { return nil }})
	if err != nil {
		t.Fatal(err)
	}
	r := Start(eng, src, dst, Options{})
	defer r.Stop()
	eventually(t, "needs-adopt", func() bool { return r.Status().State == StateNeedsAdopt })
	if exists(filepath.Join(dst, "a.txt")) || exists(filepath.Join(src, "existing.txt")) {
		t.Fatal("synced without confirmation")
	}
}
