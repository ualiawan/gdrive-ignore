//go:build !windows

package drivefs

type syncTarget struct {
	account string
	mount   string
}

func syncTargets() ([]syncTarget, error) { return nil, nil }

func processRunning(string) bool { return false }
