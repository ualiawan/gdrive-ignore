package agent

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"testing/fstest"
	"time"

	"gdrive-ignore/internal/config"
)

type client struct {
	t    *testing.T
	base string
	tok  string
}

func (c *client) do(method, path string, body any) (int, map[string]any) {
	c.t.Helper()
	var r io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		r = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, c.base+path, r)
	if c.tok != "" {
		req.Header.Set("X-Token", c.tok)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	defer res.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(res.Body).Decode(&out)
	return res.StatusCode, out
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestAgentAPI(t *testing.T) {
	home := t.TempDir()
	t.Setenv(config.HomeEnv, home)
	src := filepath.Join(home, "project")
	dst := filepath.Join(home, "drive", "project")
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(os.MkdirAll(filepath.Join(src, "node_modules", "x"), 0o755))
	must(os.WriteFile(filepath.Join(src, "main.go"), []byte("package main"), 0o644))
	must(os.WriteFile(filepath.Join(src, "node_modules", "x", "i.js"), []byte("js"), 0o644))
	must(os.WriteFile(filepath.Join(src, "notes.log"), []byte("log"), 0o644))

	a, err := New("test", slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
	must(err)
	a.Start()
	defer a.Stop()
	rt, err := a.Serve(fstest.MapFS{"index.html": {Data: []byte("ui")}}, Hooks{Quit: func() {}})
	must(err)
	base := "http://127.0.0.1:" + strconv.Itoa(rt.Port)

	// Guard: no token, wrong host.
	anon := &client{t: t, base: base}
	if code, _ := anon.do("GET", "/api/state", nil); code != http.StatusUnauthorized {
		t.Fatalf("no token: %d", code)
	}
	req, _ := http.NewRequest("GET", base+"/api/state", nil)
	req.Host = "evil.example:" + strconv.Itoa(rt.Port)
	req.Header.Set("X-Token", rt.Token)
	if res, err := http.DefaultClient.Do(req); err != nil || res.StatusCode != http.StatusForbidden {
		t.Fatalf("wrong host allowed: %v %v", res.StatusCode, err)
	}
	if got, _ := ReadRuntime(); got.Port != rt.Port {
		t.Fatal("runtime file not written")
	}

	c := &client{t: t, base: base, tok: rt.Token}
	// Global rules default to ignoring node_modules.
	code, out := c.do("POST", "/api/pairs", config.Pair{Source: src, Target: dst, Rules: "*.log", UseGlobal: true})
	if code != 200 {
		t.Fatalf("add pair: %d %v", code, out)
	}
	id := out["id"].(string)
	waitFor(t, "mirror", func() bool {
		_, err := os.Stat(filepath.Join(dst, "main.go"))
		return err == nil
	})
	for _, p := range []string{"node_modules", "notes.log"} {
		if _, err := os.Stat(filepath.Join(dst, p)); err == nil {
			t.Errorf("%s should be ignored", p)
		}
	}

	// Overlapping target is rejected.
	if code, out := c.do("POST", "/api/pairs", config.Pair{Source: filepath.Join(home, "other"), Target: filepath.Join(dst, "sub")}); code != 400 {
		t.Errorf("overlap accepted: %d %v", code, out)
	}

	// Preview reports the ignored entries with their rules.
	code, out = c.do("POST", "/api/preview", config.Pair{Source: src, Rules: "*.log", UseGlobal: true})
	if code != 200 {
		t.Fatalf("preview: %d %v", code, out)
	}
	if ig := out["ignored"].([]any); len(ig) != 2 {
		t.Errorf("preview ignored: %v", ig)
	}

	// Check explains a path.
	code, out = c.do("POST", "/api/check", map[string]string{"path": filepath.Join(src, "node_modules", "x", "i.js")})
	if code != 200 || out["ignored"] != true || out["by"] != "node_modules" || out["source"] != "global rules" {
		t.Errorf("check: %d %v", code, out)
	}

	// Removing with deleteMirror cleans the target.
	if code, out := c.do("DELETE", "/api/pairs/"+id+"?deleteMirror=1", nil); code != 200 {
		t.Fatalf("delete: %d %v", code, out)
	}
	if _, err := os.Stat(filepath.Join(dst, "main.go")); err == nil {
		t.Error("mirror not deleted")
	}
	if _, err := os.Stat(filepath.Join(src, "main.go")); err != nil {
		t.Error("source damaged")
	}
}
