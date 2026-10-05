// Package web serves the collimation station's HTMX user interface.
package web

import (
	"bytes"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"fmt"
	"html/template"
	"io/fs"
	"log"
	"math"
	"net/http"
	"net/url"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/exploded/collimation/internal/collim"
	"github.com/exploded/collimation/internal/station"
)

//go:embed ui
var uiFS embed.FS

// Server is the web front end.
type Server struct {
	st    *station.Station
	pages map[string]*template.Template
	mux   *http.ServeMux
}

// New builds the server and parses all templates.
func New(st *station.Station) (*Server, error) {
	s := &Server{st: st, mux: http.NewServeMux()}
	if err := s.loadTemplates(); err != nil {
		return nil, err
	}
	s.routes()
	return s, nil
}

var funcs = template.FuncMap{
	"f0":   func(v float64) string { return fmt.Sprintf("%.0f", v) },
	"f1":   func(v float64) string { return fmt.Sprintf("%.1f", v) },
	"f2":   func(v float64) string { return fmt.Sprintf("%.2f", v) },
	"s2":   func(v float64) string { return fmt.Sprintf("%+.2f", v) },
	"turn": collim.FormatTurn,
	"pct":  func(v float64) string { return fmt.Sprintf("%.0f%%", v*100) },
	"ago": func(ts string) string {
		t, err := time.Parse(time.RFC3339, ts)
		if err != nil {
			return ts
		}
		d := time.Since(t)
		switch {
		case d < time.Minute:
			return "just now"
		case d < time.Hour:
			return fmt.Sprintf("%d min ago", int(d.Minutes()))
		case d < 24*time.Hour:
			return t.Format("15:04")
		}
		return t.Format("2 Jan 15:04")
	},
	"clock": func(ts string) string {
		t, err := time.Parse(time.RFC3339, ts)
		if err != nil {
			return ts
		}
		return t.Format("2 Jan 15:04")
	},
	"query": url.QueryEscape,
	"abs":   math.Abs,
	"inc":   func(i int) int { return i + 1 },
	"base":  filepath.Base,
	"lower": strings.ToLower,
	"list3": func() []string { return []string{"A", "B", "C"} },
	"list4": func() []string { return []string{"Baseline", "Screw A", "Screw B", "Screw C"} },
	// scale8 turns coma px into a region-arrow length, clamped to the cell.
	"scale8": func(v float64) float64 { return math.Max(-44, math.Min(44, v*12)) },
	// turnS writes a signed turn compactly for tables.
	"turnS": func(e float64) string {
		if e == 0 {
			return "–"
		}
		d := "cw"
		if e < 0 {
			d = "acw"
		}
		return strings.TrimSuffix(strings.TrimSuffix(collim.FormatTurn(e), " turns"), " turn") + " " + d
	},
}

// assetURL returns /static/<name>?v=<content hash>, so browsers fetch a new
// copy whenever the file changes but cache it otherwise.
func assetURL(name string) string {
	b, err := uiFS.ReadFile("ui/static/" + name)
	if err != nil {
		return "/static/" + name
	}
	h := sha256.Sum256(b)
	return "/static/" + name + "?v=" + hex.EncodeToString(h[:4])
}

// loadTemplates clones layouts + partials per page so each page's "content"
// block is isolated.
func (s *Server) loadTemplates() error {
	root, err := fs.Sub(uiFS, "ui/templates")
	if err != nil {
		return err
	}
	assets := map[string]string{}
	for _, n := range []string{"app.css", "htmx.min.js", "icon.svg"} {
		assets[n] = assetURL(n)
	}
	fm := template.FuncMap{"asset": func(n string) string { return assets[n] }}
	base, err := template.New("").Funcs(funcs).Funcs(fm).ParseFS(root, "layouts/*.html", "partials/*.html")
	if err != nil {
		return err
	}
	pages, err := fs.Glob(root, "pages/*.html")
	if err != nil {
		return err
	}
	s.pages = map[string]*template.Template{}
	for _, p := range pages {
		name := strings.TrimSuffix(path.Base(p), ".html")
		t, err := template.Must(base.Clone()).ParseFS(root, p)
		if err != nil {
			return fmt.Errorf("%s: %w", p, err)
		}
		s.pages[name] = t
	}
	return nil
}

// render writes a full page, or just the named fragment for HTMX requests.
func (s *Server) render(w http.ResponseWriter, r *http.Request, page, fragment string, data any) {
	t, ok := s.pages[page]
	if !ok {
		http.Error(w, "no such page", http.StatusInternalServerError)
		return
	}
	name := "base"
	if fragment != "" && r.Header.Get("HX-Request") == "true" {
		name = fragment
	}
	var buf bytes.Buffer
	if err := t.ExecuteTemplate(&buf, name, data); err != nil {
		log.Printf("template %s/%s: %v", page, name, err)
		http.Error(w, "template error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	buf.WriteTo(w)
}

// ServeHTTP applies a same-origin check to state-changing requests: the app
// has no login, so refuse POSTs that a web page elsewhere could forge.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost {
		if o := r.Header.Get("Origin"); o != "" {
			u, err := url.Parse(o)
			if err != nil || u.Host != r.Host {
				http.Error(w, "cross-origin request refused", http.StatusForbidden)
				return
			}
		}
	}
	s.mux.ServeHTTP(w, r)
}

func (s *Server) routes() {
	static, _ := fs.Sub(uiFS, "ui/static")
	fileServer := http.FileServerFS(static)
	s.mux.Handle("GET /static/", http.StripPrefix("/static/", cacheStatic(fileServer)))

	s.mux.HandleFunc("GET /{$}", s.collimate)
	s.mux.HandleFunc("GET /station", s.stationFragment)
	s.mux.HandleFunc("POST /measure", s.measure)
	s.mux.HandleFunc("POST /measure/plain", s.measurePlain)
	s.mux.HandleFunc("POST /adjust/skip", s.skipAdjust)
	s.mux.HandleFunc("POST /live", s.liveStart)
	s.mux.HandleFunc("POST /stop", s.stop)
	s.mux.HandleFunc("POST /field", s.field)
	s.mux.HandleFunc("POST /recentre", s.recentre)
	s.mux.HandleFunc("POST /park", s.park)
	s.mux.HandleFunc("POST /unpark", s.unpark)
	s.mux.HandleFunc("POST /home", s.home)
	s.mux.HandleFunc("GET /nina/status", s.ninaStatus)

	s.mux.HandleFunc("GET /calibrate", s.calibrate)
	s.mux.HandleFunc("GET /calibrate/panel", s.calibratePanel)
	s.mux.HandleFunc("POST /calibrate/start", s.calibrateStart)
	s.mux.HandleFunc("POST /calibrate/measure", s.calibrateMeasure)
	s.mux.HandleFunc("POST /calibrate/finish", s.calibrateFinish)
	s.mux.HandleFunc("POST /calibrate/cancel", s.calibrateCancel)

	s.mux.HandleFunc("GET /analyse", s.analyse)
	s.mux.HandleFunc("POST /analyse/run", s.analyseRun)
	s.mux.HandleFunc("GET /analyse/job", s.analyseJob)

	s.mux.HandleFunc("GET /result/{id}", s.result)
	s.mux.HandleFunc("GET /result/{id}/side/{i}", s.sideImage)
	s.mux.HandleFunc("GET /history", s.history)
	s.mux.HandleFunc("GET /setup", s.setup)
	s.mux.HandleFunc("POST /setup", s.setupSave)
	s.mux.HandleFunc("POST /setup/test", s.setupTest)
	s.mux.HandleFunc("POST /setup/focus", s.setupFocus)
}

func cacheStatic(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("v") != "" {
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		} else {
			w.Header().Set("Cache-Control", "no-cache")
		}
		h.ServeHTTP(w, r)
	})
}
