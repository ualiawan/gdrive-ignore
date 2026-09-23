//go:build !windows

package install

import "errors"

var errUnsupported = errors.New("installing is only supported on Windows for now")

func IsInstalled() bool       { return false }
func RunningInstalled() bool  { return false }
func Install(string) error    { return errUnsupported }
func Uninstall(bool) error    { return errUnsupported }
func AutostartEnabled() bool  { return false }
func SetAutostart(bool) error { return errUnsupported }
func Dir() string             { return "" }
func ExePath() string         { return "" }
