package install

import "strings"

// pathWith returns cur with dir appended as a PATH entry. Everything else,
// including empty entries and a trailing separator, is kept exactly.
func pathWith(cur, dir string) (string, bool) {
	for _, p := range strings.Split(cur, ";") {
		if sameEntry(p, dir) {
			return cur, false
		}
	}
	switch {
	case cur == "":
		return dir, true
	case strings.HasSuffix(cur, ";"):
		return cur + dir + ";", true
	default:
		return cur + ";" + dir, true
	}
}

// pathWithout removes every entry equal to dir, keeping everything else
// exactly, so that pathWithout(pathWith(x, d), d) == x.
func pathWithout(cur, dir string) (string, bool) {
	parts := strings.Split(cur, ";")
	out := parts[:0:0]
	found := false
	for _, p := range parts {
		if sameEntry(p, dir) {
			found = true
			continue
		}
		out = append(out, p)
	}
	if !found {
		return cur, false
	}
	return strings.Join(out, ";"), true
}

func sameEntry(a, b string) bool {
	return a != "" && strings.EqualFold(strings.TrimRight(a, `\`), strings.TrimRight(b, `\`))
}
