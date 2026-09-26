//go:build !windows

package twoway

import (
	"os"
	"path/filepath"
	"syscall"
)

func fileID(path string) (uint64, error) {
	fi, err := os.Lstat(path)
	if err != nil {
		return 0, err
	}
	return fi.Sys().(*syscall.Stat_t).Ino, nil
}

func entryInfo(path string) (node, error) {
	fi, err := os.Lstat(path)
	if err != nil {
		return node{}, err
	}
	n := node{Dir: fi.IsDir(), MTime: fi.ModTime().UnixNano()}
	if !n.Dir {
		n.Size = fi.Size()
	}
	return n, nil
}

func hardlinkCapable(src, dst string) bool {
	a, err1 := os.Stat(src)
	b, err2 := os.Stat(nearestExisting(dst))
	if err1 != nil || err2 != nil {
		return false
	}
	return a.Sys().(*syscall.Stat_t).Dev == b.Sys().(*syscall.Stat_t).Dev
}

func nearestExisting(p string) string {
	for {
		if _, err := os.Stat(p); err == nil {
			return p
		}
		parent := filepath.Dir(p)
		if parent == p {
			return p
		}
		p = parent
	}
}

func removeFile(p string) error         { return os.Remove(p) }
func replaceFile(tmp, dst string) error { return os.Rename(tmp, dst) }
