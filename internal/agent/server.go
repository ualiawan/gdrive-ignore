package agent

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io/fs"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"gdrive-ignore/internal/config"
	"gdrive-ignore/internal/ignore"
	"gdrive-ignore/internal/install"
	"gdrive-ignore/internal/winapi"
)

// Runtime describes a running agent so the UI and CLI can reach it.
type Runtime struct {
	Port  int    `json:"port"`
	Token string `json:"token"`
	PID   int    `json:"pid"`
}

// URL is the UI address, including the one-time login token.
func (r Runtime) URL() string {
	return "http://127.0.0.1:" + strconv.Itoa(r.Port) + "/?t=" + r.Token
}

// RuntimePath is where the running agent publishes its Runtime.
func RuntimePath() string { return filepath.Join(config.LocalDir(), "agent.json") }

// ReadRuntime returns the running agent's details.
func ReadRuntime() (Runtime, error) {
	var rt Runtime
	b, err := os.ReadFile(RuntimePath())
	if err != nil {
		return rt, err
	}
	err = json.Unmarshal(b, &rt)
	return rt, err
}

const cookieName = "gdi"

// Hooks are app-level actions the UI can trigger.
type Hooks struct {
	Quit    func()       // exit the app
	Install func() error // install for this user and restart from there
}

// Serve starts the API and UI server on a random loopback port.
func (a *Agent) Serve(assets fs.FS, hooks Hooks) (Runtime, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return Runtime{}, err
	}
	tok := make([]byte, 24)
	_, _ = rand.Read(tok)
	rt := Runtime{Port: ln.Addr().(*net.TCPAddr).Port, Token: hex.EncodeToString(tok), PID: os.Getpid()}

	mux := http.NewServeMux()
	a.routes(mux, hooks)
	static := http.FileServerFS(assets)
	mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		if t := r.URL.Query().Get("t"); t != "" {
			if !equal(t, rt.Token) {
				http.Error(w, "forbidden", http.StatusForbidden)
				return
			}
			http.SetCookie(w, &http.Cookie{Name: cookieName, Value: rt.Token, Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode})
			http.Redirect(w, r, "/", http.StatusFound)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		static.ServeHTTP(w, r)
	})

	srv := &http.Server{
		Handler:           guard(rt, mux),
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() { _ = srv.Serve(ln) }()

	b, _ := json.Marshal(rt)
	if err := os.MkdirAll(filepath.Dir(RuntimePath()), 0o755); err != nil {
		return rt, err
	}
	if err := os.WriteFile(RuntimePath(), b, 0o600); err != nil {
		return rt, err
	}
	return rt, nil
}

// RemoveRuntime deletes the runtime file if it belongs to this process.
func RemoveRuntime() {
	if rt, err := ReadRuntime(); err == nil && rt.PID == os.Getpid() {
		_ = os.Remove(RuntimePath())
	}
}

func equal(a, b string) bool { return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1 }

// guard rejects requests that are not from our UI: wrong Host (DNS
// rebinding) or, for the API, a missing token.
func guard(rt Runtime, next http.Handler) http.Handler {
	port := strconv.Itoa(rt.Port)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Host != "127.0.0.1:"+port && r.Host != "localhost:"+port {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		authed := equal(r.Header.Get("X-Token"), rt.Token)
		if c, err := r.Cookie(cookieName); err == nil && equal(c.Value, rt.Token) {
			authed = true
		}
		if strings.HasPrefix(r.URL.Path, "/api/") && !authed {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if r.URL.Path == "/" && !authed && r.URL.Query().Get("t") == "" {
			http.Error(w, "Open gdrive-ignore from the tray icon or Start menu.", http.StatusUnauthorized)
			return
		}
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; style-src 'self' 'unsafe-inline'")
		next.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, err error, code int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
}

func readJSON(r *http.Request, v any) error {
	return json.NewDecoder(http.MaxBytesReader(nil, r.Body, 4<<20)).Decode(v)
}

func (a *Agent) routes(mux *http.ServeMux, hooks Hooks) {
	mux.HandleFunc("GET /api/state", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{
			"version":          a.Version,
			"pairs":            a.Pairs(),
			"drive":            a.Drive(r.URL.Query().Has("refresh")),
			"settings":         a.Settings(),
			"autostart":        install.AutostartEnabled(),
			"installed":        install.IsInstalled(),
			"runningInstalled": install.RunningInstalled(),
			"suggested":        SuggestedRoot(),
			"globalPath":       config.GlobalRulesPath(),
		})
	})
	mux.HandleFunc("GET /api/global", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"text": a.Global(), "path": config.GlobalRulesPath()})
	})
	mux.HandleFunc("PUT /api/global", func(w http.ResponseWriter, r *http.Request) {
		var body struct{ Text string }
		if err := readJSON(r, &body); err != nil {
			writeErr(w, err, http.StatusBadRequest)
			return
		}
		if err := a.SetGlobal(body.Text); err != nil {
			writeErr(w, err, http.StatusInternalServerError)
			return
		}
		writeJSON(w, map[string]bool{"ok": true})
	})
	mux.HandleFunc("GET /api/templates", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, ignore.Templates)
	})
	mux.HandleFunc("POST /api/pairs", func(w http.ResponseWriter, r *http.Request) {
		var p config.Pair
		if err := readJSON(r, &p); err != nil {
			writeErr(w, err, http.StatusBadRequest)
			return
		}
		p, err := a.AddPair(p)
		if err != nil {
			writeErr(w, err, http.StatusBadRequest)
			return
		}
		writeJSON(w, p)
	})
	mux.HandleFunc("PUT /api/pairs/{id}", func(w http.ResponseWriter, r *http.Request) {
		var p config.Pair
		if err := readJSON(r, &p); err != nil {
			writeErr(w, err, http.StatusBadRequest)
			return
		}
		p.ID = r.PathValue("id")
		if err := a.UpdatePair(p); err != nil {
			writeErr(w, err, http.StatusBadRequest)
			return
		}
		writeJSON(w, map[string]bool{"ok": true})
	})
	mux.HandleFunc("DELETE /api/pairs/{id}", func(w http.ResponseWriter, r *http.Request) {
		if err := a.RemovePair(r.PathValue("id")); err != nil {
			writeErr(w, err, http.StatusBadRequest)
			return
		}
		writeJSON(w, map[string]bool{"ok": true})
	})
	mux.HandleFunc("POST /api/pairs/{id}/{action}", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if id == "all" {
			id = ""
		}
		var err error
		switch r.PathValue("action") {
		case "sync":
			a.SyncNow(id)
		case "pause":
			err = a.SetPaused(id, true)
		case "resume":
			err = a.SetPaused(id, false)
		case "adopt":
			err = a.Adopt(id)
		default:
			err = errors.New("unknown action")
		}
		if err != nil {
			writeErr(w, err, http.StatusBadRequest)
			return
		}
		writeJSON(w, map[string]bool{"ok": true})
	})
	mux.HandleFunc("POST /api/pairs/{id}/decide", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Path     string `json:"path"` // "" = all pending for this pair
			Decision string `json:"decision"`
		}
		if err := readJSON(r, &body); err != nil {
			writeErr(w, err, http.StatusBadRequest)
			return
		}
		if err := a.Decide(r.PathValue("id"), body.Path, body.Decision); err != nil {
			writeErr(w, err, http.StatusBadRequest)
			return
		}
		writeJSON(w, map[string]bool{"ok": true})
	})
	mux.HandleFunc("POST /api/preview", func(w http.ResponseWriter, r *http.Request) {
		var p config.Pair
		if err := readJSON(r, &p); err != nil {
			writeErr(w, err, http.StatusBadRequest)
			return
		}
		res, err := a.Preview(p)
		if err != nil {
			writeErr(w, err, http.StatusBadRequest)
			return
		}
		writeJSON(w, res)
	})
	mux.HandleFunc("POST /api/check", func(w http.ResponseWriter, r *http.Request) {
		var body struct{ Path string }
		if err := readJSON(r, &body); err != nil {
			writeErr(w, err, http.StatusBadRequest)
			return
		}
		pair, res, err := a.Check(body.Path)
		if err != nil {
			writeErr(w, err, http.StatusBadRequest)
			return
		}
		out := map[string]any{"pair": pair, "ignored": res.Ignored, "by": res.By}
		if res.Rule != nil {
			out["rule"], out["source"], out["line"] = res.Rule.Text, res.Rule.Source, res.Rule.Line
		}
		writeJSON(w, out)
	})
	mux.HandleFunc("PUT /api/settings", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Settings
			Autostart *bool `json:"autostart"`
		}
		if err := readJSON(r, &body); err != nil {
			writeErr(w, err, http.StatusBadRequest)
			return
		}
		if err := a.SetSettings(body.Settings); err != nil {
			writeErr(w, err, http.StatusBadRequest)
			return
		}
		if body.Autostart != nil {
			if err := install.SetAutostart(*body.Autostart); err != nil {
				writeErr(w, err, http.StatusInternalServerError)
				return
			}
		}
		writeJSON(w, map[string]bool{"ok": true})
	})
	mux.HandleFunc("POST /api/pick-folder", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Title string `json:"title"`
			Start string `json:"start"`
			Owner uint64 `json:"owner"`
		}
		_ = readJSON(r, &body)
		p, err := winapi.PickFolder(uintptr(body.Owner), body.Title, body.Start)
		if errors.Is(err, winapi.ErrCancelled) {
			writeJSON(w, map[string]string{"path": ""})
			return
		}
		if err != nil {
			writeErr(w, err, http.StatusInternalServerError)
			return
		}
		writeJSON(w, map[string]string{"path": p})
	})
	mux.HandleFunc("POST /api/open", func(w http.ResponseWriter, r *http.Request) {
		var body struct{ Path string }
		if err := readJSON(r, &body); err != nil {
			writeErr(w, err, http.StatusBadRequest)
			return
		}
		if body.Path == "logs" {
			body.Path = config.LogDir()
		}
		if fi, err := os.Stat(body.Path); err != nil || !fi.IsDir() {
			writeErr(w, errors.New("folder not found"), http.StatusBadRequest)
			return
		}
		if err := winapi.Open(body.Path); err != nil {
			writeErr(w, err, http.StatusInternalServerError)
			return
		}
		writeJSON(w, map[string]bool{"ok": true})
	})
	mux.HandleFunc("POST /api/install", func(w http.ResponseWriter, r *http.Request) {
		if hooks.Install == nil {
			writeErr(w, errors.New("install is not available"), http.StatusBadRequest)
			return
		}
		if err := hooks.Install(); err != nil {
			writeErr(w, err, http.StatusInternalServerError)
			return
		}
		writeJSON(w, map[string]bool{"ok": true})
	})
	mux.HandleFunc("POST /api/quit", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]bool{"ok": true})
		go func() {
			time.Sleep(100 * time.Millisecond)
			hooks.Quit()
		}()
	})
}
