// Package drivefs discovers Google Drive for Desktop's synced locations from
// its local configuration, without credentials and without modifying it.
package drivefs

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Kind is how Drive syncs a location.
type Kind string

const (
	KindStream Kind = "stream" // virtual drive (e.g. G:\My Drive); files are uploaded, hardlinks impossible
	KindMirror Kind = "mirror" // My Drive mirrored to a local folder
	KindBackup Kind = "backup" // "Folders on my computer" synced or backed up to Drive
)

// Location is a folder that Drive uploads.
type Location struct {
	Kind    Kind   `json:"kind"`
	Path    string `json:"path"`
	Label   string `json:"label"`
	Account string `json:"account,omitempty"`
	Exists  bool   `json:"exists"`
}

// Info is the result of Discover.
type Info struct {
	Installed bool       `json:"installed"`
	Running   bool       `json:"running"`
	Locations []Location `json:"locations"`
	Warnings  []string   `json:"warnings,omitempty"`
}

// ConfigDir returns Drive for Desktop's per-user data folder.
func ConfigDir() string {
	return filepath.Join(os.Getenv("LOCALAPPDATA"), "Google", "DriveFS")
}

// Discover gathers everything that can be learned locally. It never fails;
// problems are reported as warnings.
func Discover() Info {
	var info Info
	dir := ConfigDir()
	if fi, err := os.Stat(dir); err == nil && fi.IsDir() {
		info.Installed = true
	}
	info.Running = processRunning("GoogleDriveFS.exe")

	seen := map[string]bool{}
	add := func(l Location) {
		k := strings.ToLower(filepath.Clean(l.Path))
		if seen[k] {
			return
		}
		seen[k] = true
		_, err := os.Stat(l.Path)
		l.Exists = err == nil
		info.Locations = append(info.Locations, l)
	}

	targets, err := syncTargets()
	if err != nil && info.Installed {
		info.Warnings = append(info.Warnings, "could not read Drive's virtual drive letters: "+err.Error())
	}
	for _, t := range targets {
		for _, l := range streamLocations(t.mount, t.account) {
			add(l)
		}
	}

	roots, err := readRoots(dir)
	if err != nil && info.Installed {
		info.Warnings = append(info.Warnings, "could not read Drive's folder list: "+err.Error())
	}
	for _, r := range roots {
		add(r)
	}
	sort.SliceStable(info.Locations, func(i, j int) bool {
		return kindOrder(info.Locations[i].Kind) < kindOrder(info.Locations[j].Kind)
	})
	return info
}

func kindOrder(k Kind) int {
	switch k {
	case KindBackup:
		return 0
	case KindMirror:
		return 1
	}
	return 2
}

// streamLocations lists the top-level folders of a Drive virtual drive
// ("My Drive", "Shared drives\X", localized names included).
func streamLocations(mount, account string) []Location {
	root := strings.TrimRight(mount, `\`) + `\`
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil
	}
	var out []Location
	for _, e := range entries {
		name := e.Name()
		if !e.IsDir() || strings.HasPrefix(name, "$") || strings.HasPrefix(name, ".") {
			continue
		}
		p := filepath.Join(root, name)
		// Shared drives (whatever the localized name) contain one folder per drive.
		if isSharedDrivesFolder(name) {
			subs, _ := os.ReadDir(p)
			for _, s := range subs {
				if s.IsDir() && !strings.HasPrefix(s.Name(), ".") {
					out = append(out, Location{Kind: KindStream, Path: filepath.Join(p, s.Name()), Label: name + " › " + s.Name(), Account: account})
				}
			}
			continue
		}
		out = append(out, Location{Kind: KindStream, Path: p, Label: name + " (" + strings.TrimRight(root, `\`) + ")", Account: account})
	}
	return out
}

func isSharedDrivesFolder(name string) bool {
	n := strings.ToLower(name)
	for _, s := range []string{"shared drives", "gedeelde drives", "geteilte ablagen", "drives partagés", "unidades compartidas", "drive condivisi", "other computers", "andere computers"} {
		if n == s {
			return true
		}
	}
	return false
}

// Covering returns the location that contains p, if any.
func (i Info) Covering(p string) *Location {
	p = strings.ToLower(filepath.Clean(p))
	var best *Location
	for k := range i.Locations {
		l := &i.Locations[k]
		lp := strings.ToLower(filepath.Clean(l.Path))
		if p == lp || strings.HasPrefix(p, strings.TrimRight(lp, `\`)+`\`) {
			if best == nil || len(l.Path) > len(best.Path) {
				best = l
			}
		}
	}
	return best
}

// IsLocationRoot reports whether p is exactly one of Drive's roots, which
// must not be used as a mirror target (the engine would own the whole root).
func (i Info) IsLocationRoot(p string) bool {
	p = strings.ToLower(filepath.Clean(p))
	for _, l := range i.Locations {
		if strings.ToLower(filepath.Clean(l.Path)) == p {
			return true
		}
	}
	return false
}
