// Package tray shows the notification area icon and menu.
package tray

import (
	"sync"

	"fyne.io/systray"

	"gdrive-ignore/internal/assets"
)

// Actions are the callbacks behind the menu.
type Actions struct {
	Open      func()
	SyncAll   func()
	SetPaused func(paused bool)
	AllPaused func() bool
	OpenLogs  func()
	Quit      func()
	Summary   func() string
}

var (
	mu      sync.Mutex
	ready   bool
	pauseMI *systray.MenuItem
	statMI  *systray.MenuItem
	acts    Actions
)

// Run shows the tray icon and blocks until Quit. It must be called from the
// main goroutine with the OS thread locked.
func Run(a Actions, onExit func()) {
	acts = a
	systray.Run(onReady, onExit)
}

// Quit removes the icon and makes Run return.
func Quit() { systray.Quit() }

// Refresh updates the tooltip and menu state.
func Refresh() {
	mu.Lock()
	defer mu.Unlock()
	if !ready {
		return
	}
	s := acts.Summary()
	systray.SetTooltip("gdrive-ignore: " + s)
	statMI.SetTitle(s)
	if acts.AllPaused() {
		pauseMI.SetTitle("Resume syncing")
	} else {
		pauseMI.SetTitle("Pause syncing")
	}
}

func onReady() {
	systray.SetIcon(assets.Icon)
	systray.SetTitle("gdrive-ignore")
	systray.SetTooltip("gdrive-ignore")
	systray.SetOnTapped(acts.Open)

	open := systray.AddMenuItem("Open gdrive-ignore", "Show the window")
	mu.Lock()
	statMI = systray.AddMenuItem("", "")
	statMI.Disable()
	mu.Unlock()
	systray.AddSeparator()
	syncMI := systray.AddMenuItem("Sync all now", "Rescan every folder now")
	mu.Lock()
	pauseMI = systray.AddMenuItem("Pause syncing", "Pause or resume all folders")
	mu.Unlock()
	logsMI := systray.AddMenuItem("Open log folder", "")
	systray.AddSeparator()
	quitMI := systray.AddMenuItem("Quit", "Stop syncing and exit")

	mu.Lock()
	ready = true
	mu.Unlock()
	Refresh()

	go func() {
		for {
			select {
			case <-open.ClickedCh:
				acts.Open()
			case <-syncMI.ClickedCh:
				acts.SyncAll()
			case <-pauseMI.ClickedCh:
				acts.SetPaused(!acts.AllPaused())
				Refresh()
			case <-logsMI.ClickedCh:
				acts.OpenLogs()
			case <-quitMI.ClickedCh:
				acts.Quit()
				return
			}
		}
	}()
}
