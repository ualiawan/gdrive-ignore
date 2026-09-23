//go:build !windows

package mirror

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
)

func hardlinkCapable(src, dst string) bool {
	a, err1 := os.Stat(src)
	b, err2 := os.Stat(nearestExisting(dst))
	if err1 != nil || err2 != nil {
		return false
	}
	sa, ok1 := a.Sys().(*syscall.Stat_t)
	sb, ok2 := b.Sys().(*syscall.Stat_t)
	return ok1 && ok2 && sa.Dev == sb.Dev
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

func isCrossDevice(err error) bool {
	return errors.Is(err, syscall.EXDEV) || errors.Is(err, syscall.ENOTSUP) || errors.Is(err, syscall.EPERM)
}

func removeFile(p string) error { return os.Remove(p) }

func replaceFile(tmp, dst string) error { return os.Rename(tmp, dst) }
