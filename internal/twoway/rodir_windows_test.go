package twoway

import "golang.org/x/sys/windows"

func setReadOnlyDir(p string) error {
	ptr, err := windows.UTF16PtrFromString(p)
	if err != nil {
		return err
	}
	return windows.SetFileAttributes(ptr, windows.FILE_ATTRIBUTE_DIRECTORY|windows.FILE_ATTRIBUTE_READONLY)
}
