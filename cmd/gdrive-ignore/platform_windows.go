package main

import (
	"errors"
	"os"
	"time"

	"golang.org/x/sys/windows"

	"gdrive-ignore/internal/config"
)

var procAttachConsole = windows.NewLazySystemDLL("kernel32.dll").NewProc("AttachConsole")

// attachConsole connects a GUI-subsystem exe to the parent console so CLI
// commands can print. Redirected output (pipes, files) already works.
func attachConsole() {
	if h, err := windows.GetStdHandle(windows.STD_OUTPUT_HANDLE); err == nil && h != 0 && h != windows.InvalidHandle {
		return
	}
	const attachParent = ^uintptr(0) // ATTACH_PARENT_PROCESS (-1)
	if r, _, _ := procAttachConsole.Call(attachParent); r == 0 {
		return
	}
	if f, err := os.OpenFile("CONOUT$", os.O_RDWR, 0); err == nil {
		os.Stdout, os.Stderr = f, f
	}
}

// singleInstance holds a named mutex while the returned release is not
// called. ok is false if another instance holds it (after waiting up to 10s
// when wait is set, e.g. right after an install hands over).
func singleInstance(name string, wait bool) (release func(), ok bool) {
	n, _ := windows.UTF16PtrFromString(`Local\` + config.InstanceName(name))
	deadline := time.Now().Add(10 * time.Second)
	for {
		h, err := windows.CreateMutex(nil, true, n)
		if err == nil {
			return func() { windows.ReleaseMutex(h); windows.CloseHandle(h) }, true
		}
		if h != 0 {
			windows.CloseHandle(h)
		}
		if !errors.Is(err, windows.ERROR_ALREADY_EXISTS) || !wait || time.Now().After(deadline) {
			return func() {}, false
		}
		time.Sleep(200 * time.Millisecond)
	}
}
