// Package config stores the user's sync pairs and global rules.
package config

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
)

// AppName is used for folders, the mutex and the autostart entry.
const AppName = "gdrive-ignore"

// Pair is one source folder mirrored to one target folder.
type Pair struct {
	ID             string `json:"id"`
	Name           string `json:"name"`
	Source         string `json:"source"`
	Target         string `json:"target"`
	Rules          string `json:"rules"`          // pair rules, one pattern per line
	UseGlobal      bool   `json:"useGlobal"`      // apply the global rules
	HonorGitignore bool   `json:"honorGitignore"` // also read .gitignore files
	Mode           string `json:"mode,omitempty"` // v1 only (auto/hardlink/copy); ignored since v2
	Paused         bool   `json:"paused"`
	Adopted        bool   `json:"adopted"` // user allowed syncing into a non-empty target
}

// Config is the persisted app configuration.
type Config struct {
	Version         int    `json:"version"`
	Pairs           []Pair `json:"pairs"`
	IntervalMinutes int    `json:"intervalMinutes"` // full rescan interval
}

// DefaultGlobalRules seeds the global rules file on first run.
const DefaultGlobalRules = `# Global rules apply to every sync pair.
# One pattern per line, .gitignore syntax. Lines starting with # are comments.
node_modules/
.venv/
__pycache__/
Thumbs.db
desktop.ini
~$*
`

// HomeEnv overrides both folders (used by tests and portable setups).
const HomeEnv = "GDRIVE_IGNORE_HOME"

// Dir is the roaming config folder (%APPDATA%\gdrive-ignore).
func Dir() string {
	if h := os.Getenv(HomeEnv); h != "" {
		return filepath.Join(h, "config")
	}
	if d, err := os.UserConfigDir(); err == nil {
		return filepath.Join(d, AppName)
	}
	return filepath.Join(os.TempDir(), AppName)
}

// LocalDir is the per-machine data folder (%LOCALAPPDATA%\gdrive-ignore) for
// manifests, logs and runtime files.
func LocalDir() string {
	if h := os.Getenv(HomeEnv); h != "" {
		return filepath.Join(h, "local")
	}
	if d := os.Getenv("LOCALAPPDATA"); d != "" {
		return filepath.Join(d, AppName)
	}
	if d, err := os.UserCacheDir(); err == nil {
		return filepath.Join(d, AppName)
	}
	return filepath.Join(os.TempDir(), AppName)
}

// Path of config.json.
func Path() string { return filepath.Join(Dir(), "config.json") }

// GlobalRulesPath is the global ignore file, editable in any text editor.
func GlobalRulesPath() string { return filepath.Join(Dir(), "global.driveignore") }

// ManifestPath is the v1 (one-way) manifest; its presence marks a pair as
// already adopted when migrating to v2.
func ManifestPath(id string) string { return filepath.Join(LocalDir(), "state", id+".json") }

// LogDir holds agent logs.
func LogDir() string { return filepath.Join(LocalDir(), "logs") }

// Load reads the config, returning defaults if none exists yet.
func Load() (*Config, error) {
	c := &Config{Version: 1, IntervalMinutes: 15}
	b, err := os.ReadFile(Path())
	if errors.Is(err, fs.ErrNotExist) {
		return c, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, c); err != nil {
		return nil, err
	}
	if c.IntervalMinutes <= 0 {
		c.IntervalMinutes = 15
	}
	return c, nil
}

// Save writes the config atomically.
func (c *Config) Save() error {
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	return writeAtomic(Path(), b)
}

// Find returns the pair with id, or nil.
func (c *Config) Find(id string) *Pair {
	for i := range c.Pairs {
		if c.Pairs[i].ID == id {
			return &c.Pairs[i]
		}
	}
	return nil
}

// GlobalRules returns the global rules text, creating the file with defaults
// on first use.
func GlobalRules() (string, error) {
	b, err := os.ReadFile(GlobalRulesPath())
	if errors.Is(err, fs.ErrNotExist) {
		if err := SetGlobalRules(DefaultGlobalRules); err != nil {
			return "", err
		}
		return DefaultGlobalRules, nil
	}
	return string(b), err
}

// SetGlobalRules replaces the global rules text.
func SetGlobalRules(text string) error {
	return writeAtomic(GlobalRulesPath(), []byte(text))
}

// NewID returns a short random identifier.
func NewID() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func writeAtomic(path string, b []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// StatePath is where a pair's two-way sync state (baseline and pending
// decisions) is kept.
func StatePath(id string) string { return filepath.Join(LocalDir(), "state", id+".v2.json") }
