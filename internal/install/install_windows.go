// Package install performs a per-user install without admin rights: copy
// the exe, add a Start menu shortcut, autostart and an uninstall entry.
package install

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"

	"gdrive-ignore/internal/assets"
	"gdrive-ignore/internal/config"
	"gdrive-ignore/internal/winapi"
)

const (
	displayName = "gdrive-ignore"
	runKey      = `Software\Microsoft\Windows\CurrentVersion\Run`
	uninstKey   = `Software\Microsoft\Windows\CurrentVersion\Uninstall\` + config.AppName
	exeName     = config.AppName + ".exe"
)

// Dir is the install folder.
func Dir() string {
	return filepath.Join(os.Getenv("LOCALAPPDATA"), "Programs", config.AppName)
}

// ExePath is the installed exe.
func ExePath() string { return filepath.Join(Dir(), exeName) }

func shortcutPath() string {
	return filepath.Join(os.Getenv("APPDATA"), `Microsoft\Windows\Start Menu\Programs`, displayName+".lnk")
}

// IsInstalled reports whether the app is installed for this user.
func IsInstalled() bool {
	_, err := os.Stat(ExePath())
	return err == nil
}

// RunningInstalled reports whether the current process is the installed exe.
func RunningInstalled() bool {
	self, err := os.Executable()
	return err == nil && strings.EqualFold(filepath.Clean(self), filepath.Clean(ExePath()))
}

// Install copies the running exe into place and registers it.
func Install(version string) error {
	self, err := os.Executable()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(Dir(), 0o755); err != nil {
		return err
	}
	if !RunningInstalled() {
		if err := copyExe(self, ExePath()); err != nil {
			return fmt.Errorf("copying program: %w (is it running? quit it from the tray first)", err)
		}
	}
	exe := ExePath()
	if launcher := assets.CLILauncher(); launcher != nil {
		// Replacing a running launcher fails; it is tiny, so rename it away.
		com := filepath.Join(Dir(), config.AppName+".com")
		_ = os.Remove(com + ".old")
		_ = os.Rename(com, com+".old")
		if err := os.WriteFile(com, launcher, 0o755); err != nil {
			return fmt.Errorf("writing command-line launcher: %w", err)
		}
		if err := addToPath(Dir()); err != nil {
			return fmt.Errorf("adding to PATH: %w", err)
		}
	}
	if err := winapi.CreateShortcut(winapi.Shortcut{
		Path: shortcutPath(), Target: exe, WorkDir: Dir(),
		Description: "Choose what Google Drive syncs", Icon: exe,
	}); err != nil {
		return fmt.Errorf("creating Start menu shortcut: %w", err)
	}
	if err := SetAutostart(true); err != nil {
		return err
	}
	k, _, err := registry.CreateKey(registry.CURRENT_USER, uninstKey, registry.SET_VALUE)
	if err != nil {
		return err
	}
	defer k.Close()
	size := uint32(0)
	if fi, err := os.Stat(exe); err == nil {
		size = uint32(fi.Size() / 1024)
	}
	for name, v := range map[string]string{
		"DisplayName":     displayName,
		"DisplayVersion":  version,
		"Publisher":       displayName,
		"InstallLocation": Dir(),
		"DisplayIcon":     exe,
		"UninstallString": `"` + exe + `" uninstall`,
	} {
		if err := k.SetStringValue(name, v); err != nil {
			return err
		}
	}
	_ = k.SetDWordValue("NoModify", 1)
	_ = k.SetDWordValue("NoRepair", 1)
	_ = k.SetDWordValue("EstimatedSize", size)
	return nil
}

func copyExe(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	tmp := dst + ".new"
	out, err := os.Create(tmp)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		os.Remove(tmp)
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	// A running exe can be renamed but not overwritten.
	if _, err := os.Stat(dst); err == nil {
		old := dst + ".old"
		os.Remove(old)
		if err := os.Rename(dst, old); err != nil {
			os.Remove(tmp)
			return err
		}
	}
	return os.Rename(tmp, dst)
}

// Uninstall removes the registration and, after this process exits, the
// program folder. Settings are kept unless purge is set. Mirrored files in
// Drive are never touched.
func Uninstall(purge bool) error {
	var errs []error
	_ = SetAutostart(false)
	if err := removeFromPath(Dir()); err != nil {
		errs = append(errs, err)
	}
	if err := os.Remove(shortcutPath()); err != nil && !errors.Is(err, os.ErrNotExist) {
		errs = append(errs, err)
	}
	if err := registry.DeleteKey(registry.CURRENT_USER, uninstKey); err != nil && !errors.Is(err, registry.ErrNotExist) {
		errs = append(errs, err)
	}
	dirs := []string{Dir()}
	if purge {
		dirs = append(dirs, config.Dir(), config.LocalDir())
	}
	// Delete folders once we have exited (the exe may be inside one).
	var cmd strings.Builder
	cmd.WriteString("ping -n 3 127.0.0.1 >nul")
	for _, d := range dirs {
		cmd.WriteString(` & rmdir /s /q "` + d + `"`)
	}
	c := exec.Command("cmd.exe", "/c", cmd.String())
	c.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000} // CREATE_NO_WINDOW
	if err := c.Start(); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// AutostartEnabled reports whether the app starts with Windows.
func AutostartEnabled() bool {
	k, err := registry.OpenKey(registry.CURRENT_USER, runKey, registry.QUERY_VALUE)
	if err != nil {
		return false
	}
	defer k.Close()
	_, _, err = k.GetStringValue(config.AppName)
	return err == nil
}

// SetAutostart adds or removes the app from the user's startup programs.
// It points at the installed exe when installed, else the running one.
func SetAutostart(on bool) error {
	k, _, err := registry.CreateKey(registry.CURRENT_USER, runKey, registry.SET_VALUE)
	if err != nil {
		return err
	}
	defer k.Close()
	if !on {
		if err := k.DeleteValue(config.AppName); err != nil && !errors.Is(err, registry.ErrNotExist) {
			return err
		}
		return nil
	}
	exe := ExePath()
	if !IsInstalled() {
		if exe, err = os.Executable(); err != nil {
			return err
		}
	}
	return k.SetStringValue(config.AppName, `"`+exe+`" --background`)
}

const envKey = `Environment`

// addToPath appends dir to the user's PATH if missing.
func addToPath(dir string) error {
	return editPath(func(parts []string) []string {
		for _, p := range parts {
			if strings.EqualFold(strings.TrimRight(p, `\`), strings.TrimRight(dir, `\`)) {
				return nil
			}
		}
		return append(parts, dir)
	})
}

// removeFromPath removes dir from the user's PATH.
func removeFromPath(dir string) error {
	return editPath(func(parts []string) []string {
		out := parts[:0:0]
		found := false
		for _, p := range parts {
			if strings.EqualFold(strings.TrimRight(p, `\`), strings.TrimRight(dir, `\`)) {
				found = true
				continue
			}
			out = append(out, p)
		}
		if !found {
			return nil
		}
		return out
	})
}

// editPath rewrites HKCU\Environment\Path. edit returns nil for no change.
// The value type (usually REG_EXPAND_SZ) is preserved.
func editPath(edit func([]string) []string) error {
	k, err := registry.OpenKey(registry.CURRENT_USER, envKey, registry.QUERY_VALUE|registry.SET_VALUE)
	if err != nil {
		return err
	}
	defer k.Close()
	cur, typ, err := k.GetStringValue("Path")
	if err != nil && !errors.Is(err, registry.ErrNotExist) {
		return err
	}
	var parts []string
	for _, p := range strings.Split(cur, ";") {
		if p != "" {
			parts = append(parts, p)
		}
	}
	next := edit(parts)
	if next == nil {
		return nil
	}
	val := strings.Join(next, ";")
	if typ == registry.SZ {
		err = k.SetStringValue("Path", val)
	} else {
		err = k.SetExpandStringValue("Path", val)
	}
	if err != nil {
		return err
	}
	// Tell Explorer so newly opened terminals see the change.
	env, _ := windows.UTF16PtrFromString("Environment")
	var result uintptr
	windows.NewLazySystemDLL("user32.dll").NewProc("SendMessageTimeoutW").Call(
		0xFFFF, 0x001A, 0, uintptr(unsafe.Pointer(env)), 0x0002, 2000, uintptr(unsafe.Pointer(&result)))
	return nil
}
