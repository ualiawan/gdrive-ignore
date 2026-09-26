package twoway

import (
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"gdrive-ignore/internal/ignore"
)

// IgnoredEntry describes a source path excluded by a rule.
type IgnoredEntry struct {
	Path   string `json:"path"`
	Dir    bool   `json:"dir"`
	Rule   string `json:"rule"`
	Source string `json:"source"`
	Line   int    `json:"line"`
	Size   int64  `json:"size"`  // bytes, including everything below a folder
	Count  int    `json:"count"` // files, including everything below a folder
}

// PreviewResult is what a dry run of the source finds.
type PreviewResult struct {
	Files       int            `json:"files"`
	Bytes       int64          `json:"bytes"`
	Ignored     []IgnoredEntry `json:"ignored"`
	IgnoreFiles []string       `json:"ignoreFiles"`
	Duration    time.Duration  `json:"duration"`
}

// Preview scans a source folder with rules and reports what would sync and
// what is ignored, without touching anything. With measure, ignored folders
// are walked to total their size.
func Preview(source string, ignoreFiles []string, rules *ignore.Set, measure bool) (PreviewResult, error) {
	start := time.Now()
	if rules == nil {
		rules = ignore.NewSet()
	}
	if len(ignoreFiles) == 0 {
		ignoreFiles = []string{ignore.DefaultFileName}
	}
	l := &ignore.Loader{Root: source, FileNames: ignoreFiles, Base: rules}
	var res PreviewResult
	set, err := l.ForDir(rules, "")
	if err != nil {
		return res, err
	}
	if _, err := os.Stat(source); err != nil {
		return res, err
	}
	var walk func(rel string, set *ignore.Set)
	walk = func(rel string, set *ignore.Set) {
		entries, err := os.ReadDir(filepath.Join(source, filepath.FromSlash(rel)))
		if err != nil {
			return
		}
		for _, de := range entries {
			name := de.Name()
			if skipName(name) {
				continue
			}
			p := join(rel, name)
			isDir := de.IsDir()
			if !isDir && !de.Type().IsRegular() {
				continue
			}
			if ign, r := set.Match(p, isDir); ign {
				ie := IgnoredEntry{Path: p, Dir: isDir}
				if r != nil {
					ie.Rule, ie.Source, ie.Line = r.Text, r.Source, r.Line
				}
				if !isDir {
					if info, err := de.Info(); err == nil {
						ie.Size, ie.Count = info.Size(), 1
					}
				} else if measure {
					_ = filepath.WalkDir(filepath.Join(source, filepath.FromSlash(p)), func(_ string, d fs.DirEntry, err error) error {
						if err == nil && d.Type().IsRegular() {
							if info, err := d.Info(); err == nil {
								ie.Size += info.Size()
								ie.Count++
							}
						}
						return nil
					})
				}
				res.Ignored = append(res.Ignored, ie)
				continue
			}
			if isDir {
				child, err := l.ForDir(set, p)
				if err == nil {
					walk(p, child)
				}
				continue
			}
			res.Files++
			if info, err := de.Info(); err == nil {
				res.Bytes += info.Size()
			}
			if l.IsIgnoreFile(name) {
				res.IgnoreFiles = append(res.IgnoreFiles, p)
			}
		}
	}
	walk("", set)
	res.Duration = time.Since(start)
	return res, nil
}
