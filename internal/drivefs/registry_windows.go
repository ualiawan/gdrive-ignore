package drivefs

import (
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

type syncTarget struct {
	account string
	mount   string
}

// syncTargets decodes HKCU\Software\Google\DriveFS\Share\SyncTargets, a
// protobuf listing each account's virtual drive letter.
func syncTargets() ([]syncTarget, error) {
	k, err := registry.OpenKey(registry.CURRENT_USER, `Software\Google\DriveFS\Share`, registry.QUERY_VALUE)
	if err != nil {
		if err == registry.ErrNotExist {
			return nil, nil
		}
		return nil, err
	}
	defer k.Close()
	b, _, err := k.GetBinaryValue("SyncTargets")
	if err != nil {
		if err == registry.ErrNotExist {
			return nil, nil
		}
		return nil, err
	}
	return parseSyncTargets(b), nil
}

func processRunning(exe string) bool {
	snap, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return false
	}
	defer windows.CloseHandle(snap)
	var pe windows.ProcessEntry32
	pe.Size = uint32(unsafe.Sizeof(pe))
	for err = windows.Process32First(snap, &pe); err == nil; err = windows.Process32Next(snap, &pe) {
		if strings.EqualFold(windows.UTF16ToString(pe.ExeFile[:]), exe) {
			return true
		}
	}
	return false
}
