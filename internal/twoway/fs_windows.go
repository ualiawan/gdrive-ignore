package twoway

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// fileID returns the NTFS file ID (sequence + MFT index). Hardlinks share it.
func fileID(path string) (uint64, error) {
	p, err := windows.UTF16PtrFromString(fixLongPath(path))
	if err != nil {
		return 0, err
	}
	h, err := windows.CreateFile(p, windows.FILE_READ_ATTRIBUTES,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil, windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err != nil {
		return 0, &os.PathError{Op: "open", Path: path, Err: err}
	}
	defer windows.CloseHandle(h)
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(h, &info); err != nil {
		return 0, &os.PathError{Op: "fileinfo", Path: path, Err: err}
	}
	return uint64(info.FileIndexHigh)<<32 | uint64(info.FileIndexLow), nil
}

// entryInfo returns what the directory listing shows for path: size and
// mtime as recorded in the parent's directory entry. For hardlinks this can
// differ per name (NTFS updates the entry of the name used to write), which
// is exactly what the engine compares against its baseline.
func entryInfo(path string) (node, error) {
	p, err := windows.UTF16PtrFromString(fixLongPath(path))
	if err != nil {
		return node{}, err
	}
	var fd windows.Win32finddata
	h, err := windows.FindFirstFile(p, &fd)
	if err != nil {
		return node{}, &os.PathError{Op: "find", Path: path, Err: err}
	}
	windows.FindClose(h)
	n := node{Dir: fd.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY != 0}
	if !n.Dir {
		n.Size = int64(fd.FileSizeHigh)<<32 | int64(fd.FileSizeLow)
	}
	n.MTime = time.Unix(0, fd.LastWriteTime.Nanoseconds()).UnixNano()
	return n, nil
}

// hardlinkCapable reports whether src and dst are on the same NTFS/ReFS volume.
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

// removeFile deletes one name of a file, even if read-only, without clearing
// the read-only attribute (which is shared by all hardlinks, so clearing it
// would change the other side too). os.Remove is not used because it clears
// the attribute and retries.
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

// fixLongPath adds the \\?\ prefix to long absolute paths for raw API calls.
func fixLongPath(p string) string {
	if len(p) < 248 || strings.HasPrefix(p, `\\?\`) || !filepath.IsAbs(p) {
		return p
	}
	if strings.HasPrefix(p, `\\`) {
		return `\\?\UNC\` + p[2:]
	}
	return `\\?\` + p
}

// removeDir removes an empty folder in the mirror. Drive for Desktop marks
// folders in computer folders read-only, which makes RemoveDirectory fail;
// folders are never hardlinked, so clearing the attribute on the mirror's
// folder cannot affect the source.
func removeDir(p string) error {
	err := os.Remove(p)
	if err == nil || !errors.Is(err, windows.ERROR_ACCESS_DENIED) {
		return err
	}
	ptr, perr := windows.UTF16PtrFromString(fixLongPath(p))
	if perr != nil {
		return err
	}
	attrs, aerr := windows.GetFileAttributes(ptr)
	if aerr != nil || attrs&windows.FILE_ATTRIBUTE_DIRECTORY == 0 || attrs&windows.FILE_ATTRIBUTE_READONLY == 0 {
		return err
	}
	if serr := windows.SetFileAttributes(ptr, attrs&^windows.FILE_ATTRIBUTE_READONLY); serr != nil {
		return err
	}
	if rerr := os.Remove(p); rerr != nil {
		_ = windows.SetFileAttributes(ptr, attrs) // not empty after all: put it back
		return rerr
	}
	return nil
}
