package mirror

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
)

// hardlinkCapable reports whether src and dst live on the same volume and
// that volume's file system supports hardlinks.
func hardlinkCapable(src, dst string) bool {
	vs, err := volumePath(src)
	if err != nil {
		return false
	}
	vd, err := volumePath(nearestExisting(dst))
	if err != nil || !strings.EqualFold(vs, vd) {
		return false
	}
	var fsName [windows.MAX_PATH + 1]uint16
	root, err := windows.UTF16PtrFromString(vs)
	if err != nil {
		return false
	}
	if err := windows.GetVolumeInformation(root, nil, 0, nil, nil, nil, &fsName[0], uint32(len(fsName))); err != nil {
		return false
	}
	switch strings.ToUpper(windows.UTF16ToString(fsName[:])) {
	case "NTFS", "REFS":
		return true
	}
	return false
}

func volumePath(p string) (string, error) {
	ptr, err := windows.UTF16PtrFromString(p)
	if err != nil {
		return "", err
	}
	buf := make([]uint16, windows.MAX_PATH+1)
	if err := windows.GetVolumePathName(ptr, &buf[0], uint32(len(buf))); err != nil {
		return "", err
	}
	return windows.UTF16ToString(buf), nil
}

// nearestExisting returns p or its closest existing ancestor.
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
	return errors.Is(err, windows.ERROR_NOT_SAME_DEVICE) ||
		errors.Is(err, windows.ERROR_INVALID_FUNCTION) ||
		errors.Is(err, windows.ERROR_NOT_SUPPORTED)
}

// removeFile deletes a file even if it is read-only, without clearing the
// attribute (with hardlinks that would change the source file too).
// os.Remove is not used because it clears the read-only attribute and retries.
func removeFile(p string) error {
	ptr, perr := windows.UTF16PtrFromString(fixLongPath(p))
	if perr != nil {
		return perr
	}
	err := windows.DeleteFile(ptr)
	if err == nil {
		return nil
	}
	if errors.Is(err, windows.ERROR_FILE_NOT_FOUND) || errors.Is(err, windows.ERROR_PATH_NOT_FOUND) {
		return &os.PathError{Op: "remove", Path: p, Err: os.ErrNotExist}
	}
	if !errors.Is(err, windows.ERROR_ACCESS_DENIED) {
		return &os.PathError{Op: "remove", Path: p, Err: err}
	}
	h, herr := windows.CreateFile(ptr, windows.DELETE, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil, windows.OPEN_EXISTING, windows.FILE_FLAG_OPEN_REPARSE_POINT|windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
	if herr != nil {
		return &os.PathError{Op: "remove", Path: p, Err: err}
	}
	defer windows.CloseHandle(h)
	const (
		fileDispositionDelete               = 0x1
		fileDispositionPosixSemantics       = 0x2
		fileDispositionIgnoreReadonlyAttrib = 0x10
	)
	info := uint32(fileDispositionDelete | fileDispositionPosixSemantics | fileDispositionIgnoreReadonlyAttrib)
	if serr := windows.SetFileInformationByHandle(h, windows.FileDispositionInfoEx, (*byte)(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info))); serr != nil {
		return &os.PathError{Op: "remove", Path: p, Err: err}
	}
	return nil
}

// fixLongPath adds the \\?\ prefix to long absolute paths for raw API calls
// (the os package does this itself).
func fixLongPath(p string) string {
	if len(p) < 248 || strings.HasPrefix(p, `\\?\`) || !filepath.IsAbs(p) {
		return p
	}
	if strings.HasPrefix(p, `\\`) {
		return `\\?\UNC\` + p[2:]
	}
	return `\\?\` + p
}

// replaceFile moves tmp over dst, handling a read-only dst.
func replaceFile(tmp, dst string) error {
	err := os.Rename(tmp, dst)
	if err == nil || !errors.Is(err, windows.ERROR_ACCESS_DENIED) {
		return err
	}
	if rerr := removeFile(dst); rerr != nil {
		return err
	}
	return os.Rename(tmp, dst)
}
