// Package ui shows the app window: a WebView2 view of the agent's UI.
package ui

import (
	"errors"
	"fmt"
	"path/filepath"
	"time"
	"unsafe"

	webview2 "github.com/jchv/go-webview2"
	"golang.org/x/sys/windows"

	"gdrive-ignore/internal/config"
	"gdrive-ignore/internal/winapi"
)

const title = "gdrive-ignore"

var (
	user32                  = windows.NewLazySystemDLL("user32.dll")
	procFindWindow          = user32.NewProc("FindWindowW")
	procSetForegroundWindow = user32.NewProc("SetForegroundWindow")
	procShowWindow          = user32.NewProc("ShowWindow")
)

// ErrNoWebView means the WebView2 runtime is missing.
var ErrNoWebView = errors.New("WebView2 runtime not available")

// Run opens the window on url and blocks until it is closed. reconnect
// returns the current agent URL, used when the agent restarted (e.g. after
// an upgrade) and the page lost its connection. If a window is
// already open, it is brought to the front instead.
func Run(url string, reconnect func() (string, error)) error {
	name, _ := windows.UTF16PtrFromString(`Local\` + config.AppName + "-ui")
	m, err := windows.CreateMutex(nil, false, name)
	if errors.Is(err, windows.ERROR_ALREADY_EXISTS) {
		windows.CloseHandle(m)
		if focusExisting() {
			return nil
		}
	} else if err == nil {
		defer windows.CloseHandle(m)
	}

	w := webview2.NewWithOptions(webview2.WebViewOptions{
		AutoFocus: true,
		DataPath:  filepath.Join(config.LocalDir(), "webview"),
		WindowOptions: webview2.WindowOptions{
			Title: title, Width: 1000, Height: 780, Center: true, IconId: 1,
		},
	})
	if w == nil {
		return ErrNoWebView
	}
	defer w.Destroy()
	hwnd := uintptr(w.Window())
	w.Init(fmt.Sprintf("window.__hwnd = %d;", hwnd))
	_ = w.Bind("closeWindow", func() { w.Terminate() })
	_ = w.Bind("reconnect", reconnect)
	w.Navigate(url)
	w.Run()
	return nil
}

func focusExisting() bool {
	cls, _ := windows.UTF16PtrFromString("webview")
	t, _ := windows.UTF16PtrFromString(title)
	hwnd, _, _ := procFindWindow.Call(uintptr(unsafe.Pointer(cls)), uintptr(unsafe.Pointer(t)))
	if hwnd == 0 {
		return false
	}
	procShowWindow.Call(hwnd, 9) // SW_RESTORE
	procSetForegroundWindow.Call(hwnd)
	return true
}

// OpenInBrowser is the fallback when WebView2 is missing.
func OpenInBrowser(url string) error { return winapi.Open(url) }

var procPostMessage = user32.NewProc("PostMessageW")

// CloseExisting asks an open app window (possibly from an older version) to
// close, so an upgrade does not leave a window running the old code.
func CloseExisting() {
	cls, _ := windows.UTF16PtrFromString("webview")
	t, _ := windows.UTF16PtrFromString(title)
	for i := 0; i < 20; i++ {
		hwnd, _, _ := procFindWindow.Call(uintptr(unsafe.Pointer(cls)), uintptr(unsafe.Pointer(t)))
		if hwnd == 0 {
			return
		}
		procPostMessage.Call(hwnd, 0x0010, 0, 0) // WM_CLOSE
		time.Sleep(100 * time.Millisecond)
	}
}
