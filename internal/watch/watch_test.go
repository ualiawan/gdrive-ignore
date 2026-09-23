package watch

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func TestWatchReportsNestedChanges(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("windows only for now")
	}
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "a", "b"), 0o755); err != nil {
		t.Fatal(err)
	}
	w, err := New(root)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()

	if err := os.WriteFile(filepath.Join(root, "a", "b", "new.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(root, "a", "b", "new.txt"), filepath.Join(root, "a", "moved.txt")); err != nil {
		t.Fatal(err)
	}

	want := map[string]bool{"a/b/new.txt": false, "a/moved.txt": false}
	deadline := time.After(5 * time.Second)
	for missing := len(want); missing > 0; {
		select {
		case batch := <-w.C:
			for _, p := range batch {
				if seen, ok := want[p]; ok && !seen {
					want[p] = true
					missing--
				}
			}
		case err := <-w.Err:
			t.Fatal(err)
		case <-deadline:
			t.Fatalf("timed out; got %v", want)
		}
	}

	// Close is prompt and idempotent.
	done := make(chan struct{})
	go func() { w.Close(); w.Close(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Close hung")
	}
}
