// Package winapi holds the few Windows shell calls the app needs (folder
// picker, shortcuts, opening folders), implemented with raw COM vtables.
package winapi

import (
	"errors"
	"fmt"
	"runtime"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	ole32   = windows.NewLazySystemDLL("ole32.dll")
	shell32 = windows.NewLazySystemDLL("shell32.dll")

	procCoInitializeEx   = ole32.NewProc("CoInitializeEx")
	procCoUninitialize   = ole32.NewProc("CoUninitialize")
	procCoCreateInstance = ole32.NewProc("CoCreateInstance")
	procCoTaskMemFree    = ole32.NewProc("CoTaskMemFree")
	procSHCreateItem     = shell32.NewProc("SHCreateItemFromParsingName")
	procShellExecute     = shell32.NewProc("ShellExecuteW")
)

var (
	clsidFileOpenDialog = windows.GUID{Data1: 0xDC1C5A9C, Data2: 0xE88A, Data3: 0x4DDE, Data4: [8]byte{0xA5, 0xA1, 0x60, 0xF8, 0x2A, 0x20, 0xAE, 0xF7}}
	iidIFileOpenDialog  = windows.GUID{Data1: 0xD57C7288, Data2: 0xD4AD, Data3: 0x4768, Data4: [8]byte{0xBE, 0x02, 0x9D, 0x96, 0x95, 0x32, 0xD9, 0x60}}
	iidIShellItem       = windows.GUID{Data1: 0x43826D1E, Data2: 0xE718, Data3: 0x42EE, Data4: [8]byte{0xBC, 0x55, 0xA1, 0xE2, 0x61, 0xC3, 0x7B, 0xFE}}
	clsidShellLink      = windows.GUID{Data1: 0x00021401, Data2: 0x0000, Data3: 0x0000, Data4: [8]byte{0xC0, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x46}}
	iidIShellLinkW      = windows.GUID{Data1: 0x000214F9, Data2: 0x0000, Data3: 0x0000, Data4: [8]byte{0xC0, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x46}}
	iidIPersistFile     = windows.GUID{Data1: 0x0000010B, Data2: 0x0000, Data3: 0x0000, Data4: [8]byte{0xC0, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x46}}
)

const (
	clsctxInproc   = 0x1
	coinitApt      = 0x2
	fosPickFolders = 0x20
	fosForceFS     = 0x40
	fosNoChangeDir = 0x8
	sigdnFSPath    = 0x80058000
	errCancelled   = 0x800704C7
)

// ErrCancelled is returned when the user closes the picker.
var ErrCancelled = errors.New("cancelled")

type comObj struct{ vtbl *[64]uintptr }

func (o *comObj) call(idx int, args ...uintptr) uintptr {
	a := append([]uintptr{uintptr(unsafe.Pointer(o))}, args...)
	r, _, _ := syscall.SyscallN(o.vtbl[idx], a...)
	return r
}

func (o *comObj) release() { o.call(2) }

func hr(r uintptr, what string) error {
	if int32(r) < 0 {
		if uint32(r) == errCancelled {
			return ErrCancelled
		}
		return fmt.Errorf("%s failed: 0x%08X", what, uint32(r))
	}
	return nil
}

// withCOM runs f on a locked OS thread with COM initialized (STA).
func withCOM(f func() error) error {
	done := make(chan error, 1)
	go func() {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		r, _, _ := procCoInitializeEx.Call(0, coinitApt)
		if int32(r) >= 0 {
			defer procCoUninitialize.Call()
		}
		done <- f()
	}()
	return <-done
}

func create(clsid, iid *windows.GUID) (*comObj, error) {
	var obj *comObj
	r, _, _ := procCoCreateInstance.Call(uintptr(unsafe.Pointer(clsid)), 0, clsctxInproc,
		uintptr(unsafe.Pointer(iid)), uintptr(unsafe.Pointer(&obj)))
	if err := hr(r, "CoCreateInstance"); err != nil {
		return nil, err
	}
	return obj, nil
}

// pinned keeps UTF-16 strings alive while their addresses are passed as
// uintptr to COM calls.
type pinned struct{ keep []*uint16 }

func (p *pinned) str(s string) uintptr {
	u, _ := windows.UTF16PtrFromString(s)
	p.keep = append(p.keep, u)
	return uintptr(unsafe.Pointer(u))
}

// PickFolder shows the system folder picker. owner may be 0.
func PickFolder(owner uintptr, title, start string) (string, error) {
	var result string
	var s pinned
	defer runtime.KeepAlive(&s)
	err := withCOM(func() error {
		dlg, err := create(&clsidFileOpenDialog, &iidIFileOpenDialog)
		if err != nil {
			return err
		}
		defer dlg.release()
		var opts uint32
		dlg.call(10, uintptr(unsafe.Pointer(&opts))) // GetOptions
		if err := hr(dlg.call(9, uintptr(opts|fosPickFolders|fosForceFS|fosNoChangeDir)), "SetOptions"); err != nil {
			return err
		}
		if title != "" {
			dlg.call(17, s.str(title)) // SetTitle
		}
		if start != "" {
			var item *comObj
			r, _, _ := procSHCreateItem.Call(s.str(start), 0, uintptr(unsafe.Pointer(&iidIShellItem)), uintptr(unsafe.Pointer(&item)))
			if int32(r) >= 0 && item != nil {
				dlg.call(12, uintptr(unsafe.Pointer(item))) // SetFolder
				item.release()
			}
		}
		if err := hr(dlg.call(3, owner), "Show"); err != nil {
			return err
		}
		var item *comObj
		if err := hr(dlg.call(20, uintptr(unsafe.Pointer(&item))), "GetResult"); err != nil {
			return err
		}
		defer item.release()
		var name *uint16
		if err := hr(item.call(5, sigdnFSPath, uintptr(unsafe.Pointer(&name))), "GetDisplayName"); err != nil {
			return err
		}
		defer procCoTaskMemFree.Call(uintptr(unsafe.Pointer(name)))
		result = windows.UTF16PtrToString(name)
		return nil
	})
	return result, err
}

// Shortcut describes a .lnk file.
type Shortcut struct {
	Path, Target, Args, WorkDir, Description, Icon string
}

// CreateShortcut writes a .lnk file.
func CreateShortcut(sc Shortcut) error {
	var s pinned
	defer runtime.KeepAlive(&s)
	return withCOM(func() error {
		link, err := create(&clsidShellLink, &iidIShellLinkW)
		if err != nil {
			return err
		}
		defer link.release()
		if err := hr(link.call(20, s.str(sc.Target)), "SetPath"); err != nil {
			return err
		}
		if sc.Args != "" {
			link.call(11, s.str(sc.Args))
		}
		if sc.WorkDir != "" {
			link.call(9, s.str(sc.WorkDir))
		}
		if sc.Description != "" {
			link.call(7, s.str(sc.Description))
		}
		if sc.Icon != "" {
			link.call(17, s.str(sc.Icon), 0)
		}
		var pf *comObj
		if err := hr(link.call(0, uintptr(unsafe.Pointer(&iidIPersistFile)), uintptr(unsafe.Pointer(&pf))), "QueryInterface"); err != nil {
			return err
		}
		defer pf.release()
		return hr(pf.call(6, s.str(sc.Path), 1), "Save")
	})
}

// Open opens a folder or file with its default handler (Explorer for folders).
func Open(path string) error {
	var s pinned
	defer runtime.KeepAlive(&s)
	r, _, _ := procShellExecute.Call(0, s.str("open"), s.str(path), 0, 0, 1)
	if r <= 32 {
		return fmt.Errorf("could not open %s (code %d)", path, r)
	}
	return nil
}

var procMessageBox = windows.NewLazySystemDLL("user32.dll").NewProc("MessageBoxW")

// MessageBox shows a simple informational dialog.
func MessageBox(title, text string) {
	var s pinned
	defer runtime.KeepAlive(&s)
	procMessageBox.Call(0, s.str(text), s.str(title), 0x40) // MB_ICONINFORMATION
}

var procSHFileOperation = shell32.NewProc("SHFileOperationW")

// shFileOpStruct mirrors SHFILEOPSTRUCTW.
type shFileOpStruct struct {
	hwnd                  uintptr
	wFunc                 uint32
	pFrom                 *uint16
	pTo                   *uint16
	fFlags                uint16
	fAnyOperationsAborted int32
	hNameMappings         uintptr
	lpszProgressTitle     *uint16
}

// Recycle moves a file or folder to the Recycle Bin. If Windows cannot
// recycle it (e.g. too large for the bin), it asks the user instead of
// deleting it permanently without warning.
func Recycle(path string) error {
	const (
		foDelete           = 0x3
		fofSilent          = 0x4
		fofNoConfirmation  = 0x10
		fofAllowUndo       = 0x40
		fofNoErrorUI       = 0x400
		fofWantNukeWarning = 0x4000
	)
	// pFrom must be double-NUL terminated.
	from, err := windows.UTF16FromString(path)
	if err != nil {
		return err
	}
	from = append(from, 0)
	op := shFileOpStruct{
		wFunc:  foDelete,
		pFrom:  &from[0],
		fFlags: fofAllowUndo | fofNoConfirmation | fofSilent | fofNoErrorUI | fofWantNukeWarning,
	}
	var r uintptr
	err = withCOM(func() error {
		r, _, _ = procSHFileOperation.Call(uintptr(unsafe.Pointer(&op)))
		return nil
	})
	runtime.KeepAlive(from)
	if err != nil {
		return err
	}
	if r != 0 {
		return fmt.Errorf("moving %s to the Recycle Bin failed (code 0x%X)", path, r)
	}
	if op.fAnyOperationsAborted != 0 {
		return fmt.Errorf("moving %s to the Recycle Bin was cancelled", path)
	}
	return nil
}
