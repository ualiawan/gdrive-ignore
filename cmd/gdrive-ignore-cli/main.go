// Command gdrive-ignore-cli is installed as gdrive-ignore.com next to
// gdrive-ignore.exe. The main exe is a GUI program (so it can start silently
// in the tray), which shells like PowerShell neither wait for nor capture.
// Windows resolves "gdrive-ignore" to the .com first; this console launcher
// runs the exe with the same console and waits for it.
package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

func main() {
	self, err := os.Executable()
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
	exe := filepath.Join(filepath.Dir(self), "gdrive-ignore.exe")
	cmd := exec.Command(exe, os.Args[1:]...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	err = cmd.Run()
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		os.Exit(exit.ExitCode())
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}
