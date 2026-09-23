package ignore

// Template is a named set of common patterns offered as quick-adds.
type Template struct {
	Name     string
	Patterns []string
}

// Templates are shown in the rules editor.
var Templates = []Template{
	{"OS junk", []string{"Thumbs.db", "desktop.ini", ".DS_Store", "~$*", "*.tmp", "*.temp"}},
	{"Node", []string{"node_modules/", ".npm/", ".next/", ".nuxt/", "dist/", ".turbo/", ".parcel-cache/"}},
	{"Python", []string{"__pycache__/", "*.pyc", ".venv/", "venv/", ".mypy_cache/", ".pytest_cache/", ".ruff_cache/", ".tox/"}},
	{".NET / Visual Studio", []string{"bin/", "obj/", ".vs/", "*.user", "packages/"}},
	{"Java / Gradle", []string{"target/", "build/", ".gradle/", "*.class"}},
	{"Rust / Go", []string{"target/", "vendor/"}},
	{"Git", []string{".git/"}},
	{"IDE", []string{".idea/", ".vscode/", "*.swp"}},
}
