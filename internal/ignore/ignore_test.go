package ignore

import (
	"os"
	"path/filepath"
	"testing"
)

func TestMatchSegment(t *testing.T) {
	cases := []struct {
		pat, name string
		want      bool
	}{
		{"*.txt", "a.txt", true},
		{"*.txt", "a.txt.bak", false},
		{"a*b*c", "aXXbYYc", true},
		{"a*b*c", "aXXbYY", false},
		{"?.go", "a.go", true},
		{"?.go", "ab.go", false},
		{"[abc].md", "b.md", true},
		{"[!abc].md", "b.md", false},
		{"[^abc].md", "d.md", true},
		{"[a-c]x", "bx", true},
		{"[a-c]x", "dx", false},
		{"[]]x", "]x", true},
		{"[x", "[x", true},
		{`\*`, "*", true},
		{`\*`, "a", false},
		{"**", "anything", true},
		{"", "", true},
		{"*", "", true},
		{"ü*", "über", true},
	}
	for _, c := range cases {
		if got := matchSegment(c.pat, c.name); got != c.want {
			t.Errorf("matchSegment(%q, %q) = %v, want %v", c.pat, c.name, got, c.want)
		}
	}
}

type pathCase struct {
	path  string
	isDir bool
	want  bool
}

func checkSet(t *testing.T, rules string, cases []pathCase) {
	t.Helper()
	s := NewSet(Parse(rules, "test", ""))
	for _, c := range cases {
		if got, _ := s.Match(c.path, c.isDir); got != c.want {
			t.Errorf("rules %q: Match(%q, dir=%v) = %v, want %v", rules, c.path, c.isDir, got, c.want)
		}
	}
}

func TestSetSemantics(t *testing.T) {
	checkSet(t, "*.log", []pathCase{
		{"a.log", false, true},
		{"x/y/a.log", false, true},
		{"a.logs", false, false},
	})
	checkSet(t, "node_modules/", []pathCase{
		{"node_modules", true, true},
		{"a/b/node_modules", true, true},
		{"node_modules", false, false}, // a file named node_modules
	})
	checkSet(t, "/build", []pathCase{
		{"build", true, true},
		{"src/build", true, false},
	})
	checkSet(t, "doc/*.txt", []pathCase{
		{"doc/a.txt", false, true},
		{"doc/sub/a.txt", false, false},
		{"x/doc/a.txt", false, false},
	})
	checkSet(t, "**/logs", []pathCase{
		{"logs", true, true},
		{"a/b/logs", true, true},
	})
	checkSet(t, "a/**/b", []pathCase{
		{"a/b", true, true},
		{"a/x/b", true, true},
		{"a/x/y/b", false, true},
		{"b", true, false},
	})
	checkSet(t, "abc/**", []pathCase{
		{"abc", true, false},
		{"abc/x", false, true},
		{"abc/x/y", false, true},
	})
	checkSet(t, "*.log\n!keep.log", []pathCase{
		{"a.log", false, true},
		{"keep.log", false, false},
		{"sub/keep.log", false, false},
	})
	checkSet(t, "# comment\n\n   \n\\#hash\n\\!bang", []pathCase{
		{"#hash", false, true},
		{"!bang", false, true},
		{"comment", false, false},
	})
	checkSet(t, "trail   \nesc\\ ", []pathCase{
		{"trail", false, true},
		{"esc ", false, true},
	})
	// Windows-style separators are accepted.
	checkSet(t, `build\output\`, []pathCase{
		{"build/output", true, true},
		{"x/build/output", true, false},
	})
}

func TestCaseFolding(t *testing.T) {
	old := FoldCase
	defer func() { FoldCase = old }()

	FoldCase = true
	checkSet(t, "*.JPG\nNode_Modules/", []pathCase{
		{"a.jpg", false, true},
		{"x/node_modules", true, true},
	})
	FoldCase = false
	checkSet(t, "*.JPG", []pathCase{
		{"a.jpg", false, false},
		{"a.JPG", false, true},
	})
}

func TestPrecedenceAndBase(t *testing.T) {
	global := Parse("*.tmp\n*.log", "global", "")
	pair := Parse("!important.log", "pair", "")
	nested := Parse("*.md\n!keep.tmp", "sub/.driveignore", "sub")
	s := NewSet(global, pair).Extend(nested)

	cases := []struct {
		path   string
		want   bool
		source string
	}{
		{"a.log", true, "global"},
		{"important.log", false, "pair"},
		{"x.md", false, ""},
		{"sub/x.md", true, "sub/.driveignore"},
		{"sub/deeper/x.md", true, "sub/.driveignore"},
		{"sub/keep.tmp", false, "sub/.driveignore"},
		{"keep.tmp", true, "global"},
	}
	for _, c := range cases {
		got, r := s.Match(c.path, false)
		src := ""
		if r != nil {
			src = r.Source
		}
		if got != c.want || src != c.source {
			t.Errorf("Match(%q) = %v by %q, want %v by %q", c.path, got, src, c.want, c.source)
		}
	}
}

func TestLoaderCheck(t *testing.T) {
	root := t.TempDir()
	write := func(rel, content string) {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(".driveignore", "build/\n*.log\n")
	write("app/.driveignore", "!debug.log\nsecret.txt\n")
	write("app/.gitignore", "cache/\n")

	l := &Loader{Root: root, FileNames: []string{DefaultFileName}, Base: NewSet(Parse("*.bak", "global", ""))}
	cases := []struct {
		path  string
		isDir bool
		want  bool
		by    string
	}{
		{"x.log", false, true, "x.log"},
		{"app/debug.log", false, false, "app/debug.log"},
		{"app/secret.txt", false, true, "app/secret.txt"},
		{"secret.txt", false, false, "secret.txt"},
		{"build/out/app.exe", false, true, "build"},
		{"app/cache/x", false, false, "app/cache/x"},
		{"a.bak", false, true, "a.bak"},
	}
	for _, c := range cases {
		res, err := l.Check(c.path, c.isDir)
		if err != nil {
			t.Fatal(err)
		}
		if res.Ignored != c.want || res.By != c.by {
			t.Errorf("Check(%q) = %v by %q, want %v by %q", c.path, res.Ignored, res.By, c.want, c.by)
		}
	}

	// Honoring .gitignore as well.
	l.FileNames = []string{DefaultFileName, ".gitignore"}
	if res, _ := l.Check("app/cache/x", false); !res.Ignored || res.By != "app/cache" {
		t.Errorf("with .gitignore: got %+v", res)
	}
	if !l.IsIgnoreFile(`app\.GitIgnore`) {
		t.Error("IsIgnoreFile should be case-insensitive and accept paths")
	}
}
