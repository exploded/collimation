package web

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/exploded/collimation/internal/analysis"
	"github.com/exploded/collimation/internal/collim"
	"github.com/exploded/collimation/internal/db"
	"github.com/exploded/collimation/internal/station"
)

// Page is the data envelope for every template.
type Page struct {
	Title string
	Nav   string // active tab
	Msg   string // error to show
	Data  any
}

// Pt is a point in bullseye SVG coordinates.
type Pt struct {
	X, Y   float64
	Label  string
	Faded  float64 // 0..1 opacity for trail points
	Inside bool    // within tolerance
}

// Ring is a labelled distance ring.
type Ring struct {
	R     float64 // SVG units
	LX    float64 // label x
	Label string
	Tol   bool
}

// Bullseye is the target plot: where the primary's axis lands on the sensor.
type Bullseye struct {
	Extent    float64 // mm from centre to edge
	Rings     []Ring
	Trail     []Pt
	TrailPath string
	Current   *Pt
	Predicted *Pt
	Live      bool
}

const (
	bullC = 200.0 // SVG centre
	bullR = 180.0 // SVG radius of the plot
)

func newBullseye(tol float64, pts []collim.Vec, current, predicted *collim.Vec) Bullseye {
	ext := 4.0
	all := append([]collim.Vec(nil), pts...)
	if current != nil {
		all = append(all, *current)
	}
	if predicted != nil {
		all = append(all, *predicted)
	}
	for _, p := range all {
		ext = math.Max(ext, math.Ceil(p.Len()+0.5))
	}
	ext = math.Min(ext, 12)
	scale := bullR / ext
	b := Bullseye{Extent: ext}
	b.Rings = append(b.Rings, Ring{R: tol * scale, Label: fmt.Sprintf("%g mm", tol), Tol: true})
	step := 1.0
	if ext > 6 {
		step = 2
	}
	for r := step; r <= ext+1e-9; r += step {
		if math.Abs(r-tol) < 1e-9 {
			continue
		}
		b.Rings = append(b.Rings, Ring{R: r * scale, LX: bullC + r*scale + 4, Label: fmt.Sprintf("%g", r)})
	}
	toPt := func(v collim.Vec) Pt {
		// Clamp off-scale points to the edge.
		if l := v.Len(); l > ext {
			v = v.Scale(ext / l)
		}
		return Pt{X: bullC + v.X*scale, Y: bullC + v.Y*scale, Inside: v.Len() <= tol}
	}
	n := len(pts)
	for i, p := range pts {
		q := toPt(p)
		q.Faded = 0.25 + 0.6*float64(i+1)/float64(n)
		b.Trail = append(b.Trail, q)
		if i == 0 {
			b.TrailPath = fmt.Sprintf("M%.1f %.1f", q.X, q.Y)
		} else {
			b.TrailPath += fmt.Sprintf(" L%.1f %.1f", q.X, q.Y)
		}
	}
	if current != nil {
		q := toPt(*current)
		b.Current = &q
		if b.TrailPath != "" {
			b.TrailPath += fmt.Sprintf(" L%.1f %.1f", q.X, q.Y)
		}
	}
	if predicted != nil {
		q := toPt(*predicted)
		b.Predicted = &q
	}
	return b
}

// StationView feeds the main Collimate screen.
type StationView struct {
	Job      *station.Job
	Busy     bool
	Live     station.Live
	LiveOn   bool
	Latest   *db.Measurement
	Prev     *db.Measurement
	Pending  *db.Adjustment
	Steps    []collim.Step
	LastAdj  *db.Adjustment // most recently completed
	HasCal   bool
	Set      station.Settings
	Done     bool // latest within tolerance
	Bull     Bullseye
	Poll     string // htmx trigger interval
	Msg      string
	Field    bool // a collimation field is stored
	Accuracy float64
	Parked   bool // from the last status check
	CanHome  bool
}

func (s *Server) stationView(ctx context.Context, msg string) (StationView, error) {
	v := StationView{Msg: msg}
	set, err := station.LoadSettings(ctx, s.st.Q)
	if err != nil {
		return v, err
	}
	v.Set = set
	v.Field = set.TargetRA != 0 || set.TargetDec != 0
	v.Job = s.st.Job()
	v.Busy = v.Job.Running()
	if ns, ok := s.st.CachedStatus(); ok {
		v.Parked, v.CanHome = ns.AtPark, ns.CanHome
	}
	v.Live = s.st.Live()
	v.LiveOn = v.Busy && v.Job.Kind == station.KindLive
	_, v.HasCal, err = s.st.Calibration(ctx)
	if err != nil {
		return v, err
	}

	// Trail: the last 12 hours of measurements.
	since := time.Now().Add(-12 * time.Hour).Format(time.RFC3339)
	ms, err := s.st.Q.ListMeasurementsSince(ctx, since)
	if err != nil {
		return v, err
	}
	var trail []collim.Vec
	for i := range ms {
		trail = append(trail, collim.Vec{X: ms[i].AxisXMm, Y: ms[i].AxisYMm})
	}
	if n := len(ms); n > 0 {
		v.Latest = &ms[n-1]
		if n > 1 {
			v.Prev = &ms[n-2]
		}
		trail = trail[:n-1]
		v.Done = v.Latest.DecentreMm <= set.ToleranceMM
		v.Accuracy = axisErr(v.Latest.ComaErr, s.st.Cfg)
	}
	if p, err := s.st.Q.GetPendingAdjustment(ctx); err == nil {
		v.Pending = &p
		v.Steps = collim.Turns{p.TurnsA, p.TurnsB, p.TurnsC}.Steps()
	} else if !errors.Is(err, sql.ErrNoRows) {
		return v, err
	}
	if adj, err := s.st.Q.ListRecentAdjustments(ctx, 3); err == nil {
		for i := range adj {
			if adj[i].Status == "done" && v.Latest != nil && adj[i].AfterID == v.Latest.ID {
				v.LastAdj = &adj[i]
				break
			}
		}
	}

	switch {
	case v.LiveOn || (v.Live.HasReading && !v.Busy && v.Latest == nil):
		var cur *collim.Vec
		if v.Live.HasReading {
			c := collim.Vec{X: v.Live.AxisX, Y: v.Live.AxisY}
			cur = &c
		}
		lt := v.Live.Trail
		if len(lt) > 0 && cur != nil {
			lt = lt[:len(lt)-1]
		}
		v.Bull = newBullseye(set.ToleranceMM, lt, cur, nil)
		v.Bull.Live = true
	default:
		var cur, pred *collim.Vec
		if v.Latest != nil {
			c := collim.Vec{X: v.Latest.AxisXMm, Y: v.Latest.AxisYMm}
			cur = &c
			if v.Pending != nil && v.Pending.BeforeID == v.Latest.ID {
				dx, dy := analysis.ComaToAxisMM(v.Pending.PredDcx, v.Pending.PredDcy, s.st.Cfg)
				p := collim.Vec{X: c.X + dx, Y: c.Y + dy}
				pred = &p
			}
		}
		v.Bull = newBullseye(set.ToleranceMM, trail, cur, pred)
	}
	v.Poll = "every 4s"
	if v.Busy {
		v.Poll = "every 1s"
	}
	return v, nil
}

// axisErr converts a coma uncertainty (px) to mm at the sensor.
func axisErr(comaErr float64, cfg analysis.Config) float64 {
	x, _ := analysis.ComaToAxisMM(comaErr, 0, cfg)
	return math.Abs(x)
}
