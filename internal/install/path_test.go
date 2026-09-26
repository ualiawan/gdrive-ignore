package install

import "testing"

func TestPathRoundTrip(t *testing.T) {
	dir := `C:\Users\me\AppData\Local\Programs\gdrive-ignore`
	for _, orig := range []string{
		"",
		`C:\a`,
		`C:\a;C:\b`,
		`C:\a;%USERPROFILE%\x;C:\b;`, // trailing separator (as on the developer's machine)
		`C:\a;;C:\b`,                 // empty entry in the middle
		`;C:\a`,
	} {
		with, changed := pathWith(orig, dir)
		if !changed {
			t.Fatalf("%q: add reported no change", orig)
		}
		if again, changed := pathWith(with, dir+`\`); changed || again != with {
			t.Fatalf("%q: adding twice changed it: %q", orig, again)
		}
		back, changed := pathWithout(with, dir)
		if !changed || back != orig {
			t.Errorf("round trip %q -> %q -> %q", orig, with, back)
		}
	}
	if _, changed := pathWithout(`C:\a;C:\b`, dir); changed {
		t.Error("removing an absent entry must not change PATH")
	}
}
