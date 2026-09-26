// Command gdrive-ignore lets you exclude files and folders from Google Drive
// for Desktop sync with .gitignore-style rules.
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"gdrive-ignore/internal/agent"
	"gdrive-ignore/internal/config"
	"gdrive-ignore/internal/drivefs"
	"gdrive-ignore/internal/install"
	"gdrive-ignore/internal/tray"
	"gdrive-ignore/internal/ui"
	"gdrive-ignore/internal/web"
	"gdrive-ignore/internal/winapi"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

func init() {
	// The tray and the window need the main OS thread.
	runtime.LockOSThread()
}

const usage = `gdrive-ignore: exclude files and folders from Google Drive for Desktop sync.

Usage:
  gdrive-ignore                    start (tray icon) and open the window
  gdrive-ignore ui                 open the window
  gdrive-ignore status             show folders and their state
  gdrive-ignore sync               rescan all folders now
  gdrive-ignore check <path>       explain whether a path is ignored, and by which rule
  gdrive-ignore preview <folder> [--rules file] [--no-global] [--gitignore]
                                   dry run: list what would be ignored
  gdrive-ignore roots              list the folders Google Drive syncs
  gdrive-ignore install            install for this user (Start menu, autostart)
  gdrive-ignore uninstall [--purge]
                                   remove the app (--purge also removes settings)
  gdrive-ignore version
`

func main() {
	args := os.Args[1:]
	cmd := ""
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		cmd, args = args[0], args[1:]
	}
	if cmd != "" && cmd != "ui" {
		attachConsole()
	}
	var err error
	switch cmd {
	case "":
		fs := flag.NewFlagSet("gdrive-ignore", flag.ExitOnError)
		background := fs.Bool("background", false, "start without opening the window")
		wait := fs.Bool("wait", false, "wait for a previous instance to exit")
		_ = fs.Parse(args)
		err = runAgent(*background, *wait)
	case "ui":
		err = runUI()
	case "status":
		err = cmdStatus()
	case "sync":
		err = cmdSync()
	case "check":
		err = cmdCheck(args)
	case "preview":
		err = cmdPreview(args)
	case "roots":
		err = cmdRoots()
	case "install":
		err = cmdInstall()
	case "uninstall":
		fs := flag.NewFlagSet("uninstall", flag.ExitOnError)
		purge := fs.Bool("purge", false, "also remove settings and state")
		_ = fs.Parse(args)
		err = cmdUninstall(*purge)
	case "version", "--version", "-v":
		fmt.Println("gdrive-ignore", version)
	case "help", "--help", "-h":
		fmt.Print(usage)
	default:
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		if cmd == "" || cmd == "ui" {
			winapi.MessageBox("gdrive-ignore", err.Error())
		}
		os.Exit(1)
	}
}

// ---------- agent ----------

func runAgent(background, wait bool) error {
	release, ok := singleInstance("agent", wait)
	if !ok {
		// Already running: just show the window.
		if !background {
			return spawn("ui")
		}
		return nil
	}
	defer release()

	logger, closeLog := openLog()
	defer closeLog()
	logger.Info("starting", "version", version, "exe", exePath())
	// Left behind when an install replaced the running exe.
	_ = os.Remove(exePath() + ".old")

	a, err := agent.New(version, logger, tray.Refresh)
	if err != nil {
		return err
	}
	a.Start()
	_, err = a.Serve(web.Assets(), agent.Hooks{
		Quit: tray.Quit,
		Install: func() error {
			if err := install.Install(version); err != nil {
				return err
			}
			// Hand over to the installed copy.
			if err := exec.Command(install.ExePath(), "--wait").Start(); err != nil {
				return err
			}
			go func() { time.Sleep(300 * time.Millisecond); tray.Quit() }()
			return nil
		},
	})
	if err != nil {
		a.Stop()
		return err
	}
	if !background {
		go func() { _ = spawn("ui") }()
	}
	tray.Run(tray.Actions{
		Open:      func() { _ = spawn("ui") },
		SyncAll:   func() { a.SyncNow("") },
		SetPaused: func(p bool) { _ = a.SetPaused("", p) },
		AllPaused: a.AllPaused,
		OpenLogs:  func() { _ = winapi.Open(config.LogDir()) },
		Quit:      tray.Quit,
		Summary:   a.Summary,
	}, func() {
		a.Stop()
		agent.RemoveRuntime()
		logger.Info("stopped")
	})
	return nil
}

func openLog() (*slog.Logger, func()) {
	dir := config.LogDir()
	_ = os.MkdirAll(dir, 0o755)
	p := filepath.Join(dir, "agent.log")
	if fi, err := os.Stat(p); err == nil && fi.Size() > 5<<20 {
		_ = os.Rename(p, p+".1")
	}
	f, err := os.OpenFile(p, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return slog.New(slog.NewTextHandler(io.Discard, nil)), func() {}
	}
	return slog.New(slog.NewTextHandler(f, &slog.HandlerOptions{Level: slog.LevelInfo})), func() { f.Close() }
}

func exePath() string {
	p, _ := os.Executable()
	return p
}

// spawn starts this exe with args, detached.
func spawn(args ...string) error {
	return exec.Command(exePath(), args...).Start()
}

// ---------- window ----------

func runUI() error {
	rt, err := ensureAgent()
	if err != nil {
		return err
	}
	if err := ui.Run(rt.URL()); errors.Is(err, ui.ErrNoWebView) {
		return ui.OpenInBrowser(rt.URL())
	} else if err != nil {
		return err
	}
	return nil
}

// ensureAgent returns the running agent, starting one if needed.
func ensureAgent() (agent.Runtime, error) {
	if rt, err := liveAgent(); err == nil {
		return rt, nil
	}
	if err := spawn("--background"); err != nil {
		return agent.Runtime{}, err
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if rt, err := liveAgent(); err == nil {
			return rt, nil
		}
		time.Sleep(150 * time.Millisecond)
	}
	return agent.Runtime{}, errors.New("gdrive-ignore did not start; see the log folder " + config.LogDir())
}

// liveAgent returns the running agent if it answers.
func liveAgent() (agent.Runtime, error) {
	rt, err := agent.ReadRuntime()
	if err != nil {
		return rt, err
	}
	_, err = call(rt, "GET", "state", nil, nil)
	return rt, err
}

func call(rt agent.Runtime, method, path string, body, out any) (int, error) {
	var r io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		r = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, "http://127.0.0.1:"+strconv.Itoa(rt.Port)+"/api/"+path, r)
	if err != nil {
		return 0, err
	}
	req.Header.Set("X-Token", rt.Token)
	c := &http.Client{Timeout: 5 * time.Minute}
	res, err := c.Do(req)
	if err != nil {
		return 0, err
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	if res.StatusCode >= 300 {
		var e struct{ Error string }
		if json.Unmarshal(b, &e) == nil && e.Error != "" {
			return res.StatusCode, errors.New(e.Error)
		}
		return res.StatusCode, fmt.Errorf("%s: %s", res.Status, strings.TrimSpace(string(b)))
	}
	if out != nil {
		return res.StatusCode, json.Unmarshal(b, out)
	}
	return res.StatusCode, nil
}

// ---------- CLI ----------

func quietAgent() (*agent.Agent, error) {
	return agent.New(version, slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
}

func cmdStatus() error {
	rt, err := liveAgent()
	if err != nil {
		a, err := quietAgent()
		if err != nil {
			return err
		}
		fmt.Println("gdrive-ignore is not running.")
		for _, p := range a.Pairs() {
			fmt.Printf("  %s\n    %s -> %s\n", p.Name, p.Source, p.Target)
		}
		return nil
	}
	var st struct{ Pairs []agent.PairView }
	if _, err := call(rt, "GET", "state", nil, &st); err != nil {
		return err
	}
	if len(st.Pairs) == 0 {
		fmt.Println("No folders set up. Run: gdrive-ignore ui")
	}
	for _, p := range st.Pairs {
		s := p.Status
		fmt.Printf("%s [%s, %s]\n  %s -> %s\n", p.Name, s.State, s.Mode, p.Source, p.Target)
		fmt.Printf("  %d files (%s) mirrored, %d ignored, last sync %s\n",
			s.Full.Files, human(s.Full.Bytes), s.Full.Ignored, s.LastSync.Local().Format(time.DateTime))
		if p.SetupError != "" {
			fmt.Println("  problem:", p.SetupError)
		}
		if s.Error != "" {
			fmt.Println("  problem:", s.Error)
		}
		if p.Location == nil {
			fmt.Println("  note: Google Drive does not sync this target yet")
		}
	}
	return nil
}

func cmdSync() error {
	if rt, err := liveAgent(); err == nil {
		_, err := call(rt, "POST", "pairs/all/sync", nil, nil)
		if err == nil {
			fmt.Println("Rescan started.")
		}
		return err
	}
	a, err := quietAgent()
	if err != nil {
		return err
	}
	var failed int
	for _, r := range a.SyncOnce() {
		if r.Err != nil {
			failed++
			fmt.Printf("%s: %v\n", r.Name, r.Err)
			continue
		}
		s := r.Stats
		fmt.Printf("%s: %d files, %d linked, %d copied, %d removed, %d errors (%s)\n",
			r.Name, s.Files, s.Linked, s.Copied, s.Removed, s.ErrorCount, s.Duration.Round(time.Millisecond))
		for _, e := range s.Errors {
			fmt.Println("  ", e)
		}
	}
	if failed > 0 {
		return fmt.Errorf("%d folder(s) failed", failed)
	}
	return nil
}

func cmdCheck(args []string) error {
	if len(args) != 1 {
		return errors.New("usage: gdrive-ignore check <path>")
	}
	abs, err := filepath.Abs(args[0])
	if err != nil {
		return err
	}
	a, err := quietAgent()
	if err != nil {
		return err
	}
	pair, res, err := a.Check(abs)
	if err != nil {
		return err
	}
	switch {
	case res.Ignored:
		fmt.Printf("IGNORED in %q: %s matches rule %q (%s line %d)\n", pair, res.By, res.Rule.Text, res.Rule.Source, res.Rule.Line)
	case res.Rule != nil:
		fmt.Printf("SYNCED in %q: re-included by rule %q (%s line %d)\n", pair, res.Rule.Text, res.Rule.Source, res.Rule.Line)
	default:
		fmt.Printf("SYNCED in %q: no rule matches\n", pair)
	}
	return nil
}

func cmdPreview(args []string) error {
	fs := flag.NewFlagSet("preview", flag.ExitOnError)
	rulesFile := fs.String("rules", "", "file with extra rules")
	noGlobal := fs.Bool("no-global", false, "do not apply the global rules")
	gitignore := fs.Bool("gitignore", false, "also honor .gitignore files")
	var src string
	// Accept the folder before or after flags.
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		src, args = args[0], args[1:]
	}
	_ = fs.Parse(args)
	if src == "" && fs.NArg() > 0 {
		src = fs.Arg(0)
	}
	if src == "" {
		return errors.New("usage: gdrive-ignore preview <folder> [--rules file] [--no-global] [--gitignore]")
	}
	abs, err := filepath.Abs(src)
	if err != nil {
		return err
	}
	p := config.Pair{Source: abs, UseGlobal: !*noGlobal, HonorGitignore: *gitignore}
	if *rulesFile != "" {
		b, err := os.ReadFile(*rulesFile)
		if err != nil {
			return err
		}
		p.Rules = string(b)
	}
	a, err := quietAgent()
	if err != nil {
		return err
	}
	res, err := a.Preview(p)
	if err != nil {
		return err
	}
	var ib int64
	var ic int
	for _, e := range res.Ignored {
		ib += e.Size
		ic += e.Count
	}
	fmt.Printf("%d files (%s) would sync; %d entries ignored (%d files, %s)\n\n",
		res.Stats.Files, human(res.Stats.Bytes), res.Stats.Ignored, ic, human(ib))
	for _, e := range res.Ignored {
		name := e.Path
		if e.Dir {
			name += "/"
		}
		fmt.Printf("  %-60s %10s  %-20s %s:%d\n", name, human(e.Size), e.Rule, e.Source, e.Line)
	}
	if res.Truncated > 0 {
		fmt.Printf("  ...and %d more\n", res.Truncated)
	}
	if len(res.IgnoreFiles) > 0 {
		fmt.Println("\nIgnore files found:", strings.Join(res.IgnoreFiles, ", "))
	}
	return nil
}

func cmdRoots() error {
	info := drivefs.Discover()
	fmt.Printf("Google Drive for Desktop: installed=%v running=%v\n", info.Installed, info.Running)
	for _, l := range info.Locations {
		fmt.Printf("  %-7s %-45s %s\n", l.Kind, l.Path, l.Label)
	}
	for _, w := range info.Warnings {
		fmt.Println("  warning:", w)
	}
	if len(info.Locations) == 0 {
		fmt.Println("  no synced locations found")
	}
	return nil
}

func cmdInstall() error {
	if rt, err := liveAgent(); err == nil && !install.RunningInstalled() {
		// A copy is running from elsewhere; ask it to exit so the exe can be replaced.
		_, _ = call(rt, "POST", "quit", nil, nil)
		time.Sleep(500 * time.Millisecond)
	}
	if err := install.Install(version); err != nil {
		return err
	}
	fmt.Println("Installed to", install.Dir())
	if err := exec.Command(install.ExePath(), "--wait").Start(); err != nil {
		return err
	}
	fmt.Println("gdrive-ignore is running (see the tray icon) and starts with Windows.")
	return nil
}

func cmdUninstall(purge bool) error {
	if rt, err := liveAgent(); err == nil {
		_, _ = call(rt, "POST", "quit", nil, nil)
		time.Sleep(800 * time.Millisecond)
	}
	if err := install.Uninstall(purge); err != nil {
		return err
	}
	msg := "gdrive-ignore was removed. Mirrored folders were left as they are."
	if !purge {
		msg += " Settings were kept in " + config.Dir() + "."
	}
	fmt.Println(msg)
	if os.Getenv("PROMPT") == "" { // started from Apps & features, not a console
		winapi.MessageBox("gdrive-ignore", msg)
	}
	return nil
}

func human(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGTPE"[exp])
}
