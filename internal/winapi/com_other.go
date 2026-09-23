//go:build !windows

package winapi

import (
	"errors"
	"os/exec"
	"runtime"
)

// ErrCancelled is returned when the user closes the picker.
var ErrCancelled = errors.New("cancelled")

var errUnsupported = errors.New("not supported on this platform yet")

// PickFolder is not implemented yet outside Windows.
func PickFolder(owner uintptr, title, start string) (string, error) { return "", errUnsupported }

// Shortcut describes a .lnk file.
type Shortcut struct {
	Path, Target, Args, WorkDir, Description, Icon string
}

// CreateShortcut is Windows only.
func CreateShortcut(Shortcut) error { return errUnsupported }

// Open opens a folder or file with its default handler.
func Open(path string) error {
	if runtime.GOOS == "darwin" {
		return exec.Command("open", path).Start()
	}
	return exec.Command("xdg-open", path).Start()
}
