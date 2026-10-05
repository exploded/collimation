package web

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/exploded/collimation/internal/analysis"
	"github.com/exploded/collimation/internal/collim"
	"github.com/exploded/collimation/internal/db"
	"github.com/exploded/collimation/internal/station"
)

// ---- Collimate ----

func (s *Server) collimate(w http.ResponseWriter, r *http.Request) {
	s.renderStation(w, r, "")
}

func (s *Server) stationFragment(w http.ResponseWriter, r *http.Request) {
	s.renderStation(w, r, "")
}

func (s *Server) renderStation(w http.ResponseWriter, r *http.Request, msg string) {
	v, err := s.stationView(r.Context(), msg)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.render(w, r, "collimate", "station", Page{Title: "Collimate", Nav: "collimate", Data: v})
}

// act runs a station action and re-renders the station panel, showing any
// error in the panel.
func (s *Server) act(w http.ResponseWriter, r *http.Request, fn func() error) {
	msg := ""
	if err := fn(); err != nil {
		msg = err.Error()
	}
	s.renderStation(w, r, msg)
}

func (s *Server) measure(w http.ResponseWriter, r *http.Request) {
	s.act(w, r, s.st.Measure)
}

// measurePlain measures without counting it as the result of a suggestion.
func (s *Server) measurePlain(w http.ResponseWriter, r *http.Request) {
	s.act(w, r, func() error {
		if j := s.st.Job(); j.Running() {
			return station.ErrBusy
		}
		if err := s.st.SkipPending(r.Context()); err != nil {
			return err
		}
		return s.st.Measure()
	})
}

func (s *Server) skipAdjust(w http.ResponseWriter, r *http.Request) {
	s.act(w, r, func() error { return s.st.SkipPending(r.Context()) })
}

func (s *Server) liveStart(w http.ResponseWriter, r *http.Request) { s.act(w, r, s.st.StartLive) }
func (s *Server) field(w http.ResponseWriter, r *http.Request)     { s.act(w, r, s.st.GoToField) }
func (s *Server) recentre(w http.ResponseWriter, r *http.Request)  { s.act(w, r, s.st.Recentre) }
func (s *Server) park(w http.ResponseWriter, r *http.Request)      { s.act(w, r, s.st.Park) }
func (s *Server) unpark(w http.ResponseWriter, r *http.Request)    { s.act(w, r, s.st.Unpark) }
func (s *Server) home(w http.ResponseWriter, r *http.Request)      { s.act(w, r, s.st.Home) }

// ninaStatus renders the header status light.
func (s *Server) ninaStatus(w http.ResponseWriter, r *http.Request) {
	st := s.st.Status(r.Context())
	var buf bytes.Buffer
	if err := s.pages["collimate"].ExecuteTemplate(&buf, "ninalight", st); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	buf.WriteTo(w)
}

func (s *Server) stop(w http.ResponseWriter, r *http.Request) {
	s.st.Stop()
	// Give the job a moment to wind down so the panel shows it stopped.
	for range 20 {
		if j := s.st.Job(); !j.Running() {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if r.FormValue("from") == "calibrate" {
		s.calibratePanel(w, r)
		return
	}
	s.renderStation(w, r, "")
}

// ---- Calibrate ----

// CalView feeds the calibration wizard.
type CalView struct {
	Wiz    station.Wizard
	Job    *station.Job
	Busy   bool
	Set    station.Settings
	Turn   string
	HasCal bool
	Cal    collim.Calibration
	Poll   string
	Msg    string
}

func (s *Server) calView(ctx context.Context, msg string) (CalView, error) {
	set, err := station.LoadSettings(ctx, s.st.Q)
	if err != nil {
		return CalView{}, err
	}
	cal, has, err := s.st.Calibration(ctx)
	if err != nil {
		return CalView{}, err
	}
	v := CalView{Wiz: s.st.Wizard(), Job: s.st.Job(), Set: set, HasCal: has, Msg: msg}
	if has {
		cal.DeriveC()
		v.Cal = cal
	}
	v.Busy = v.Job.Running()
	v.Turn = collim.FormatTurn(set.CalTurn)
	v.Poll = "every 4s"
	if v.Busy {
		v.Poll = "every 1s"
	}
	return v, nil
}

func (s *Server) calibrate(w http.ResponseWriter, r *http.Request) {
	s.renderCal(w, r, "")
}

func (s *Server) calibratePanel(w http.ResponseWriter, r *http.Request) {
	s.renderCal(w, r, "")
}

func (s *Server) renderCal(w http.ResponseWriter, r *http.Request, msg string) {
	v, err := s.calView(r.Context(), msg)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.render(w, r, "calibrate", "calpanel", Page{Title: "Calibrate", Nav: "calibrate", Data: v})
}

func (s *Server) calAct(w http.ResponseWriter, r *http.Request, fn func() error) {
	msg := ""
	if err := fn(); err != nil {
		msg = err.Error()
	}
	s.renderCal(w, r, msg)
}

func (s *Server) calibrateStart(w http.ResponseWriter, r *http.Request) {
	s.calAct(w, r, func() error { return s.st.WizardStart(r.Context()) })
}
func (s *Server) calibrateMeasure(w http.ResponseWriter, r *http.Request) {
	s.calAct(w, r, s.st.WizardMeasure)
}
func (s *Server) calibrateFinish(w http.ResponseWriter, r *http.Request) {
	s.calAct(w, r, func() error { s.st.WizardFinish(); return nil })
}
func (s *Server) calibrateCancel(w http.ResponseWriter, r *http.Request) {
	s.calAct(w, r, func() error { s.st.WizardCancel(); return nil })
}

// ---- Analyse saved images ----

// AnalyseView lists runs in a folder.
type AnalyseView struct {
	Dir  string
	Runs []analysis.Run
	Job  *station.Job
	Err  string
}

func (s *Server) analyse(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	set, err := station.LoadSettings(ctx, s.st.Q)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	dir := r.FormValue("dir")
	if dir == "" {
		dir = set.AnalyseDir
	}
	if dir == "" {
		dir = defaultImageDir()
	}
	v := AnalyseView{Dir: dir}
	if j := s.st.Job(); j != nil && j.Kind == station.KindAnalyse {
		v.Job = j
	}
	if dir != "" {
		runs, err := s.st.Runs(dir)
		if err != nil {
			v.Err = err.Error()
		} else {
			v.Runs = runs
			if r.FormValue("dir") != "" && dir != set.AnalyseDir {
				set.AnalyseDir = dir
				_ = set.Save(ctx, s.st.Q)
			}
		}
	}
	s.render(w, r, "analyse", "runs", Page{Title: "Analyse saved images", Nav: "analyse", Data: v})
}

func defaultImageDir() string {
	cands := []string{filepath.Join("images", "2026-09-28_Snapshot", "SNAPSHOT")}
	if home, err := os.UserHomeDir(); err == nil {
		cands = append(cands, filepath.Join(home, "Documents", "N.I.N.A"))
	}
	for _, c := range cands {
		if st, err := os.Stat(c); err == nil && st.IsDir() {
			abs, _ := filepath.Abs(c)
			return abs
		}
	}
	return ""
}

func (s *Server) analyseRun(w http.ResponseWriter, r *http.Request) {
	idx, _ := strconv.Atoi(r.FormValue("run"))
	err := s.st.AnalyseRun(r.FormValue("dir"), idx)
	v := AnalyseView{Dir: r.FormValue("dir"), Job: s.st.Job()}
	if err != nil {
		v.Err = err.Error()
	}
	s.render(w, r, "analyse", "analysejob", Page{Data: v})
}

func (s *Server) analyseJob(w http.ResponseWriter, r *http.Request) {
	v := AnalyseView{Job: s.st.Job()}
	s.render(w, r, "analyse", "analysejob", Page{Data: v})
}

// ---- Results ----

// ResultView shows one measurement in detail.
type ResultView struct {
	M       db.Measurement
	Res     *analysis.Result
	Regions [][]analysis.RegionResult
	Err     float64
	Balance *BalanceView
}

func (s *Server) result(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	m, err := s.st.Q.GetMeasurement(r.Context(), id)
	if errors.Is(err, sql.ErrNoRows) {
		http.NotFound(w, r)
		return
	} else if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	v := ResultView{M: m, Res: s.st.Result(id), Err: axisErr(m.ComaErr, s.st.Cfg)}
	v.Balance = balanceView(m, v.Res, s.st.Cfg)
	if v.Res != nil && len(v.Res.Regions) > 0 {
		n := s.st.Cfg.Regions
		v.Regions = make([][]analysis.RegionResult, n)
		for _, g := range v.Res.Regions {
			v.Regions[g.Row] = append(v.Regions[g.Row], g)
		}
	}
	s.render(w, r, "result", "", Page{Title: fmt.Sprintf("Measurement %d", id), Nav: "history", Data: v})
}

// sideImage renders stack | model | residual for one side of focus.
func (s *Server) sideImage(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	i, _ := strconv.Atoi(r.PathValue("i"))
	res := s.st.Result(id)
	if res == nil || i < 0 || i >= len(res.Sides) {
		http.NotFound(w, r)
		return
	}
	sd := res.Sides[i]
	n := sd.N
	img := image.NewRGBA(image.Rect(0, 0, 3*n+8, n))
	var peak float64
	for _, v := range sd.Stack {
		peak = math.Max(peak, v)
	}
	// Amber on black to match the UI; the residual is diverging around grey.
	amber := func(t float64) color.RGBA {
		t = math.Max(0, math.Min(1, t))
		return color.RGBA{uint8(255 * math.Pow(t, 0.8)), uint8(190 * math.Pow(t, 1.2)), uint8(80 * t * t), 255}
	}
	for y := range n {
		for x := range n {
			img.Set(x, y, amber(sd.Stack[y*n+x]/peak))
			img.Set(n+4+x, y, amber(sd.Model[y*n+x]/peak))
			d := (sd.Stack[y*n+x] - sd.Model[y*n+x]) / peak * 4
			c := color.RGBA{30, 30, 30, 255}
			if d > 0 {
				c = color.RGBA{uint8(30 + 225*math.Min(d, 1)), 30, 30, 255}
			} else {
				c = color.RGBA{30, uint8(30 + 150*math.Min(-d, 1)), uint8(30 + 225*math.Min(-d, 1)), 255}
			}
			img.Set(2*n+8+x, y, c)
		}
	}
	var buf bytes.Buffer
	png.Encode(&buf, img)
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "public, max-age=86400")
	buf.WriteTo(w)
}

// ---- History ----

// HistoryView lists recent measurements and adjustments.
type HistoryView struct {
	Ms  []db.Measurement
	Adj []db.Adjustment
}

func (s *Server) history(w http.ResponseWriter, r *http.Request) {
	ms, err := s.st.Q.ListRecentMeasurements(r.Context(), 100)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	adj, _ := s.st.Q.ListRecentAdjustments(r.Context(), 30)
	s.render(w, r, "history", "", Page{Title: "History", Nav: "history", Data: HistoryView{Ms: ms, Adj: adj}})
}

// ---- Setup ----

// SetupView is the settings form.
type SetupView struct {
	Set   station.Settings
	Saved bool
	Test  []TestLine
}

// TestLine is one line of the connection test.
type TestLine struct {
	OK   bool
	Text string
}

func (s *Server) setup(w http.ResponseWriter, r *http.Request) {
	set, err := station.LoadSettings(r.Context(), s.st.Q)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.render(w, r, "setup", "", Page{Title: "Setup", Nav: "setup", Data: SetupView{Set: set}})
}

func (s *Server) setupSave(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	set, err := station.LoadSettings(ctx, s.st.Q)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	r.ParseForm()
	err = set.Apply(func(k string) (string, bool) {
		v, ok := r.PostForm[k]
		if !ok || len(v) == 0 {
			return "", false
		}
		return v[0], true
	})
	p := Page{Title: "Setup", Nav: "setup"}
	if err != nil {
		p.Msg = err.Error()
	} else if err := set.Save(ctx, s.st.Q); err != nil {
		p.Msg = err.Error()
	}
	p.Data = SetupView{Set: set, Saved: p.Msg == ""}
	s.render(w, r, "setup", "setupform", p)
}

// setupTest checks N.I.N.A. read-only: plugin version, focuser, mount, guider.
func (s *Server) setupTest(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	set, _ := station.LoadSettings(ctx, s.st.Q)
	if h := r.FormValue("nina_host"); h != "" {
		set.NinaHost = h
	}
	if p, err := strconv.Atoi(r.FormValue("nina_port")); err == nil {
		set.NinaPort = p
	}
	c := s.st.NewClient(set)
	var lines []TestLine
	add := func(ok bool, f string, a ...any) { lines = append(lines, TestLine{ok, fmt.Sprintf(f, a...)}) }
	if v, err := c.Version(ctx); err != nil {
		add(false, "N.I.N.A. Advanced API not reachable at %s:%d: %v", set.NinaHost, set.NinaPort, err)
	} else {
		add(true, "Advanced API plugin %s", v)
		if f, err := c.Focuser(ctx); err != nil {
			add(false, "Focuser: %v", err)
		} else if !f.Connected {
			add(false, "Focuser not connected in N.I.N.A.")
		} else {
			add(true, "Focuser %s at %d", f.Name, f.Position)
		}
		if fw, err := c.FilterWheel(ctx); err == nil && fw.Connected {
			name := "?"
			if fw.SelectedFilter != nil {
				name = fw.SelectedFilter.Name
			}
			add(true, "Filter wheel on %s", name)
		}
		if m, err := c.Mount(ctx); err != nil {
			add(false, "Mount: %v", err)
		} else if !m.Connected {
			add(false, "Mount not connected in N.I.N.A.")
		} else {
			add(true, "Mount %s: alt %.0f°, az %.0f°, %s", m.Name, m.Altitude, m.Azimuth, m.SideOfPier)
		}
		if g, err := c.Guider(ctx); err == nil && g.Connected {
			add(true, "Guider %s: %s (stopped automatically before measuring)", g.Name, g.State)
		}
	}
	s.render(w, r, "setup", "testresult", Page{Data: SetupView{Set: set, Test: lines}})
}

// setupFocus stores the focuser's current position as best focus.
func (s *Server) setupFocus(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	set, _ := station.LoadSettings(ctx, s.st.Q)
	p := Page{Title: "Setup", Nav: "setup"}
	f, err := s.st.NewClient(set).Focuser(ctx)
	switch {
	case err != nil:
		p.Msg = err.Error()
	case !f.Connected:
		p.Msg = "Focuser not connected in N.I.N.A."
	default:
		set.FocusPos = f.Position
		if err := set.Save(ctx, s.st.Q); err != nil {
			p.Msg = err.Error()
		}
	}
	p.Data = SetupView{Set: set, Saved: p.Msg == ""}
	s.render(w, r, "setup", "setupform", p)
}
