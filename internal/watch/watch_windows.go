package watch

import (
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"unsafe"

	"golang.org/x/sys/windows"
)

const bufSize = 64 * 1024

// New starts a recursive watch on root using ReadDirectoryChangesW.
func New(root string) (*Watcher, error) {
	ptr, err := windows.UTF16PtrFromString(root)
	if err != nil {
		return nil, err
	}
	h, err := windows.CreateFile(ptr, windows.FILE_LIST_DIRECTORY,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil, windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OVERLAPPED, 0)
	if err != nil {
		return nil, fmt.Errorf("watching %s: %w", root, err)
	}
	ioEvent, err := windows.CreateEvent(nil, 1, 0, nil)
	if err != nil {
		windows.CloseHandle(h)
		return nil, err
	}
	stopEvent, err := windows.CreateEvent(nil, 1, 0, nil)
	if err != nil {
		windows.CloseHandle(ioEvent)
		windows.CloseHandle(h)
		return nil, err
	}

	w := newWatcher()
	done := make(chan struct{})
	var once sync.Once
	w.stop = func() {
		once.Do(func() {
			windows.SetEvent(stopEvent)
			<-done
		})
	}
	go func() {
		defer close(done)
		defer windows.CloseHandle(h)
		defer windows.CloseHandle(ioEvent)
		defer windows.CloseHandle(stopEvent)
		if err := loop(h, ioEvent, stopEvent, w); err != nil {
			w.err <- err
		}
		close(w.c)
	}()
	return w, nil
}

func loop(h, ioEvent, stopEvent windows.Handle, w *Watcher) error {
	// DWORD-aligned buffer, as ReadDirectoryChangesW requires.
	buf := make([]uint32, bufSize/4)
	bufPtr := (*byte)(unsafe.Pointer(&buf[0]))
	const mask = windows.FILE_NOTIFY_CHANGE_FILE_NAME | windows.FILE_NOTIFY_CHANGE_DIR_NAME |
		windows.FILE_NOTIFY_CHANGE_SIZE | windows.FILE_NOTIFY_CHANGE_LAST_WRITE |
		windows.FILE_NOTIFY_CHANGE_CREATION
	for {
		ov := windows.Overlapped{HEvent: ioEvent}
		windows.ResetEvent(ioEvent)
		err := windows.ReadDirectoryChanges(h, bufPtr, bufSize, true, mask, nil, &ov, 0)
		if err != nil && err != windows.ERROR_IO_PENDING {
			return fmt.Errorf("watch: %w", err)
		}
		ev, err := windows.WaitForMultipleObjects([]windows.Handle{ioEvent, stopEvent}, false, windows.INFINITE)
		if err != nil {
			return err
		}
		if ev == windows.WAIT_OBJECT_0+1 {
			windows.CancelIoEx(h, &ov)
			var n uint32
			_ = windows.GetOverlappedResult(h, &ov, &n, true)
			return nil
		}
		var n uint32
		if err := windows.GetOverlappedResult(h, &ov, &n, false); err != nil {
			if err == windows.ERROR_NOTIFY_ENUM_DIR {
				w.overflow.Store(true)
				w.send(nil)
				continue
			}
			return fmt.Errorf("watch: %w", err)
		}
		if n == 0 {
			// The kernel buffer overflowed; changes were lost.
			w.overflow.Store(true)
			w.send(nil)
			continue
		}
		w.send(parse(unsafe.Slice(bufPtr, n)))
	}
}

func parse(b []byte) []string {
	var out []string
	seen := map[string]bool{}
	for off := 0; off+12 <= len(b); {
		info := (*windows.FileNotifyInformation)(unsafe.Pointer(&b[off]))
		nameLen := int(info.FileNameLength) / 2
		name := windows.UTF16ToString(unsafe.Slice(&info.FileName, nameLen))
		rel := strings.Trim(filepath.ToSlash(name), "/")
		if rel != "" && !seen[rel] {
			seen[rel] = true
			out = append(out, rel)
		}
		if info.NextEntryOffset == 0 {
			break
		}
		off += int(info.NextEntryOffset)
	}
	return out
}
