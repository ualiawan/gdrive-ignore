//go:build !windows

package ui

import (
	"errors"

	"gdrive-ignore/internal/winapi"
)

// ErrNoWebView means no embedded view is available on this platform yet.
var ErrNoWebView = errors.New("embedded window not available on this platform yet")

// Run is not implemented yet outside Windows; callers fall back to a browser.
func Run(url string, reconnect func() (string, error)) error { return ErrNoWebView }

// OpenInBrowser opens url in the default browser.
func OpenInBrowser(url string) error { return winapi.Open(url) }

// CloseExisting is a no-op outside Windows.
func CloseExisting() {}
