// Package station runs the collimation workflow: it drives N.I.N.A., analyses
// the frames, stores measurements and turns them into screw instructions.
// One job runs at a time; the web UI polls its progress.
package station

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/exploded/collimation/internal/analysis"
	"github.com/exploded/collimation/internal/collim"
	"github.com/exploded/collimation/internal/db"
	"github.com/exploded/collimation/internal/model"
	"github.com/exploded/collimation/internal/nina"
)

// Job kinds.
const (
	KindMeasure   = "measure"
	KindCalibrate = "calibrate"
	KindLive      = "live"
	KindSlew      = "slew"
	KindAnalyse   = "analyse"
)

// JobStep is one line of progress.
type JobStep struct {
	Text  string
	State string // run | done | err
}

// Job is the current or most recent task.
type Job struct {
	ID       int
	Kind     string
	Title    string
	Started  time.Time
	Finished time.Time
	Steps    []JobStep
	Err      string
	Done     bool
	ResultID int64
	cancel   context.CancelFunc
}

// Running reports whether the job is still going.
func (j *Job) Running() bool { return j != nil && !j.Done }

// Station holds the workflow state.
type Station struct {
	Q   *db.Queries
	Cfg analysis.Config
	// NewClient builds the N.I.N.A. client; replaced in tests.
	NewClient func(Settings) *nina.Client

	mu      sync.Mutex
	job     *Job
	jobSeq  int
	results map[int64]*analysis.Result
	order   []int64
	live    *Live
	wiz     *Wizard
	slewed  bool
	shared  *model.Shared // shape parameters from the latest full measurement
	status  *NinaStatus   // cached equipment status for the header light
}

// New returns a station using the database conn.
func New(conn *sql.DB, cfg analysis.Config) *Station {
	return &Station{
		Q:   db.New(conn),
		Cfg: cfg,
		NewClient: func(s Settings) *nina.Client {
			return nina.New(s.NinaHost, s.NinaPort)
		},
		results: map[int64]*analysis.Result{},
	}
}

// ErrBusy is returned when a job is already running.
var ErrBusy = errors.New("busy: wait for the current task to finish, or press Stop")

// Job returns a copy of the current job (nil if none has run).
func (s *Station) Job() *Job {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.job == nil {
		return nil
	}
	j := *s.job
	j.Steps = append([]JobStep(nil), s.job.Steps...)
	return &j
}

// Result returns a cached analysis result by measurement id.
func (s *Station) Result(id int64) *analysis.Result {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.results[id]
}

func (s *Station) cache(id int64, r *analysis.Result) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.results[id] = r
	s.order = append(s.order, id)
	for len(s.order) > 20 {
		delete(s.results, s.order[0])
		s.order = s.order[1:]
	}
}

// Stop cancels the running job.
func (s *Station) Stop() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.job != nil && s.job.cancel != nil && !s.job.Done {
		s.job.cancel()
	}
}

// progress lets a job report what it is doing.
type progress struct {
	s *Station
	j *Job
}

// Step finishes the current step and starts a new one.
func (p progress) Step(format string, args ...any) {
	p.s.mu.Lock()
	defer p.s.mu.Unlock()
	if n := len(p.j.Steps); n > 0 && p.j.Steps[n-1].State == "run" {
		p.j.Steps[n-1].State = "done"
	}
	p.j.Steps = append(p.j.Steps, JobStep{Text: fmt.Sprintf(format, args...), State: "run"})
}

// Note replaces the text of the current step.
func (p progress) Note(format string, args ...any) {
	p.s.mu.Lock()
	defer p.s.mu.Unlock()
	if n := len(p.j.Steps); n > 0 {
		p.j.Steps[n-1].Text = fmt.Sprintf(format, args...)
	}
}

func (p progress) setResult(id int64) {
	p.s.mu.Lock()
	defer p.s.mu.Unlock()
	p.j.ResultID = id
}

// start runs fn as the single active job.
func (s *Station) start(kind, title string, fn func(ctx context.Context, p progress) error) error {
	s.mu.Lock()
	if s.job.Running() {
		s.mu.Unlock()
		return ErrBusy
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.jobSeq++
	j := &Job{ID: s.jobSeq, Kind: kind, Title: title, Started: time.Now(), cancel: cancel}
	s.job = j
	s.mu.Unlock()

	go func() {
		defer cancel()
		p := progress{s, j}
		err := func() (err error) {
			defer func() {
				if r := recover(); r != nil {
					err = fmt.Errorf("internal error: %v", r)
				}
			}()
			return fn(ctx, p)
		}()
		s.mu.Lock()
		defer s.mu.Unlock()
		j.Done = true
		j.Finished = time.Now()
		n := len(j.Steps)
		switch {
		case errors.Is(err, context.Canceled):
			j.Err = "Stopped."
			if n > 0 {
				j.Steps[n-1].State = "err"
			}
		case err != nil:
			j.Err = err.Error()
			if n > 0 {
				j.Steps[n-1].State = "err"
			}
			log.Printf("%s: %v", kind, err)
		default:
			if n > 0 {
				j.Steps[n-1].State = "done"
			}
		}
	}()
	return nil
}

func (s *Station) settings(ctx context.Context) (Settings, error) {
	return LoadSettings(ctx, s.Q)
}

// connect checks N.I.N.A. is reachable.
func (s *Station) connect(ctx context.Context, p progress, set Settings) (*nina.Client, error) {
	p.Step("Connecting to N.I.N.A. at %s:%d", set.NinaHost, set.NinaPort)
	c := s.NewClient(set)
	cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if _, err := c.Version(cctx); err != nil {
		return nil, fmt.Errorf("cannot reach N.I.N.A. (is the Advanced API plugin running?): %w", err)
	}
	return c, nil
}

// prepare stops guiding, selects the filter and works out the focus position.
func (s *Station) prepare(ctx context.Context, p progress, c *nina.Client, set Settings) (int, error) {
	p.Step("Stopping guiding")
	was, err := c.StopGuiding(ctx)
	if err != nil {
		return 0, err
	}
	if was {
		p.Note("Guiding stopped (it would chase the star shifts)")
	} else {
		p.Note("Guiding is off")
	}
	if set.Filter != "" {
		p.Step("Selecting filter %s", set.Filter)
		if err := c.SetFilter(ctx, set.Filter); err != nil {
			return 0, err
		}
	}
	focus := set.FocusPos
	if focus == 0 {
		f, err := c.Focuser(ctx)
		if err != nil {
			return 0, err
		}
		if !f.Connected {
			return 0, errors.New("the focuser is not connected in N.I.N.A.")
		}
		focus = f.Position
		p.Step("No focus position set: using the focuser's current position %d", focus)
	}
	return focus, nil
}

// moveFocuser always approaches the target from below, so backlash is taken
// up the same way every time.
func moveFocuser(ctx context.Context, c *nina.Client, target int) error {
	f, err := c.Focuser(ctx)
	if err != nil {
		return err
	}
	if f.Position > target {
		if err := c.MoveFocuser(ctx, target-150); err != nil {
			return err
		}
	}
	if f.Position == target {
		return nil
	}
	return c.MoveFocuser(ctx, target)
}

// capture takes n frames at the focuser position pos.
func (s *Station) capture(ctx context.Context, p progress, c *nina.Client, set Settings, pos, n int, exp float64) ([]string, error) {
	p.Step("Moving focuser to %d", pos)
	if err := moveFocuser(ctx, c, pos); err != nil {
		return nil, err
	}
	var paths []string
	var dir string
	for i := range n {
		p.Step("Exposure %d of %d at %d (%.0f s)", i+1, n, pos, exp)
		f, err := c.Capture(ctx, exp, set.Gain)
		if err != nil {
			return nil, err
		}
		f, err = savedImage(ctx, c, set, f, &dir)
		if err != nil {
			return nil, err
		}
		paths = append(paths, f)
	}
	return paths, nil
}

// savedImage returns the local path of the image N.I.N.A. just saved as name,
// once the file is complete. *dir remembers its folder for the next frame.
func savedImage(ctx context.Context, c *nina.Client, set Settings, name string, dir *string) (string, error) {
	path := set.MapPath(name)
	if !filepath.IsAbs(name) {
		root, err := c.ImageDir(ctx)
		if err != nil {
			return "", err
		}
		path, err = nina.FindFile(ctx, set.MapPath(root), *dir, name, 30*time.Second)
		if err != nil {
			return "", err
		}
		*dir = filepath.Dir(path)
	}
	if err := nina.WaitForFile(ctx, path, 30*time.Second); err != nil {
		return "", err
	}
	return path, nil
}

// measureBoth captures both sides of focus, analyses and stores the result.
func (s *Station) measureBoth(ctx context.Context, p progress, kind string) (int64, *analysis.Result, error) {
	set, err := s.settings(ctx)
	if err != nil {
		return 0, nil, err
	}
	c, err := s.connect(ctx, p, set)
	if err != nil {
		return 0, nil, err
	}
	focus, err := s.prepare(ctx, p, c, set)
	if err != nil {
		return 0, nil, err
	}
	var paths []string
	for _, pos := range []int{focus - set.DefocusSteps, focus + set.DefocusSteps} {
		ps, err := s.capture(ctx, p, c, set, pos, set.FramesPerSide, set.ExposureS)
		if err != nil {
			return 0, nil, err
		}
		paths = append(paths, ps...)
	}
	p.Step("Returning focuser to focus (%d)", focus)
	if err := moveFocuser(ctx, c, focus); err != nil {
		return 0, nil, err
	}
	p.Step("Analysing %d frames", len(paths))
	cfg := s.Cfg
	cfg.FocusPos = focus
	res, err := analysis.Analyze(ctx, paths, cfg, func(m string) { p.Note("Analysing: %s", m) })
	if err != nil {
		return 0, nil, err
	}
	id, err := s.save(ctx, kind, res)
	if err != nil {
		return 0, nil, err
	}
	s.mu.Lock()
	sh := res.Fit.Shared
	s.shared = &sh
	s.mu.Unlock()
	return id, res, nil
}

// save stores a result and caches it for the diagnostic images.
func (s *Station) save(ctx context.Context, kind string, res *analysis.Result) (int64, error) {
	var files []string
	for _, f := range res.Infos {
		files = append(files, f.Path)
	}
	var pos []analysis.Point
	ref := 0
	if n := len(res.Sides); n > 0 {
		pos = res.Sides[n-1].Positions
		ref = res.Sides[n-1].Pos
	}
	pj, _ := json.Marshal(pos)
	stars := 0
	for _, sd := range res.Sides {
		stars += sd.Stars
	}
	s.mu.Lock()
	slewed := s.slewed
	s.slewed = false
	s.mu.Unlock()
	fr := res.Fit.Shared
	err := s.Q.CreateMeasurement(ctx, db.CreateMeasurementParams{
		CreatedAt:     time.Now().Format(time.RFC3339),
		Kind:          kind,
		ComaX:         res.ComaX,
		ComaY:         res.ComaY,
		ComaErr:       res.ComaErr,
		AxisXMm:       res.AxisXmm,
		AxisYMm:       res.AxisYmm,
		DecentreMm:    res.DecentreMM,
		SeeingPx:      fr.Sigma,
		Obstruction:   fr.Eps,
		SaPx:          fr.SA,
		StepUm:        res.StepUM,
		ParaxialFocus: res.ParaxialFocus,
		Alt:           finite(res.Alt),
		Az:            finite(res.Az),
		FocTemp:       finite(res.FocTemp),
		AmbTemp:       finite(res.AmbTemp),
		Stars:         int64(stars),
		Frames:        int64(len(res.Infos)),
		Files:         strings.Join(files, "\n"),
		RefFocpos:     int64(ref),
		Positions:     string(pj),
		AfterSlew:     boolInt(slewed),
		TiltX:         res.Tilt.X,
		TiltY:         res.Tilt.Y,
		TiltErr:       res.Tilt.Err,
		ShadowXMm:     res.Pupil.ShadowX,
		ShadowYMm:     res.Pupil.ShadowY,
		HubXMm:        res.Pupil.HubX,
		HubYMm:        res.Pupil.HubY,
		PupilErrMm:    res.Pupil.ErrMM,
		IntraHigh:     int64(res.Pupil.IntraHigh),
		TiltRough:     boolInt(res.Tilt.Rough),
		PupilRough:    boolInt(res.Pupil.Rough),
	})
	if err != nil {
		return 0, err
	}
	id, err := s.Q.GetLastMeasurementID(ctx)
	if err != nil {
		return 0, err
	}
	s.cache(id, res)
	return id, nil
}

func finite(v float64) float64 {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return 0
	}
	return v
}

func boolInt(b bool) int64 {
	if b {
		return 1
	}
	return 0
}

// Measure captures both sides of focus. If a suggested adjustment is pending,
// the new measurement is taken as its result: the calibration is refined and
// a new suggestion is made.
func (s *Station) Measure() error {
	return s.start(KindMeasure, "Measuring", func(ctx context.Context, p progress) error {
		id, _, err := s.measureBoth(ctx, p, KindMeasure)
		if err != nil {
			return err
		}
		p.setResult(id)
		p.Step("Working out the next adjustment")
		return s.afterMeasure(ctx, id)
	})
}

// SkipPending discards a suggested adjustment (it was not made).
func (s *Station) SkipPending(ctx context.Context) error {
	return s.Q.SkipPendingAdjustments(ctx)
}

// afterMeasure closes a pending adjustment and suggests the next one.
func (s *Station) afterMeasure(ctx context.Context, id int64) error {
	cur, err := s.Q.GetMeasurement(ctx, id)
	if err != nil {
		return err
	}
	cal, haveCal, err := s.Calibration(ctx)
	if err != nil {
		return err
	}
	if pend, err := s.Q.GetPendingAdjustment(ctx); err == nil {
		before, err := s.Q.GetMeasurement(ctx, pend.BeforeID)
		if err != nil {
			return err
		}
		obs := collim.Vec{X: cur.ComaX - before.ComaX, Y: cur.ComaY - before.ComaY}
		pred := collim.Vec{X: pend.PredDcx, Y: pend.PredDcy}
		applied := collim.AppliedFraction(obs, pred)
		turns := collim.Turns{pend.TurnsA, pend.TurnsB, pend.TurnsC}
		// The star shift is a cleaner measure of how far the mirror moved.
		sh, hasShift := Shift(before, cur)
		if hasShift && haveCal {
			if ps := cal.ShiftEffect(turns); ps.Len() > 20 {
				applied = collim.AppliedFraction(sh, ps)
			}
		}
		if err := s.Q.CompleteAdjustment(ctx, db.CompleteAdjustmentParams{
			AfterID: id, ObsDcx: obs.X, ObsDcy: obs.Y, Applied: finite(applied), ID: pend.ID,
		}); err != nil {
			return err
		}
		if haveCal && applied > 0.2 && applied < 3 {
			if err := s.learn(ctx, &cal, turns, obs, sh, hasShift, "update"); err != nil {
				return err
			}
		}
	} else if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	return s.suggest(ctx, cur, cal, haveCal)
}

// minGainMM is the least improvement worth a turn once inside the tolerance:
// about the scatter between repeat measurements (0.15–0.2 px of coma).
const minGainMM = 0.2

// nextTurns returns the turns to suggest for coma (px). Outside the tolerance
// a turn is always suggested; inside it, only a fine-tune that is predicted to
// bring the axis at least minGainMM closer.
func nextTurns(cal collim.Calibration, coma collim.Vec, set Settings, cfg analysis.Config) (collim.Turns, bool) {
	t := cal.Solve(coma.Scale(-1), set.CorrectionGain)
	if t == (collim.Turns{}) {
		return t, false
	}
	mm := func(c collim.Vec) float64 {
		x, y := analysis.ComaToAxisMM(c.X, c.Y, cfg)
		return math.Hypot(x, y)
	}
	now := mm(coma)
	if now > set.ToleranceMM {
		return t, true
	}
	return t, now-mm(coma.Add(cal.Effect(t))) >= minGainMM
}

// suggest stores the next adjustment for measurement cur, if one is worth
// making (see nextTurns).
func (s *Station) suggest(ctx context.Context, cur db.Measurement, cal collim.Calibration, haveCal bool) error {
	set, err := s.settings(ctx)
	if err != nil {
		return err
	}
	if !haveCal {
		return nil
	}
	t, ok := nextTurns(cal, collim.Vec{X: cur.ComaX, Y: cur.ComaY}, set, s.Cfg)
	if !ok {
		return nil
	}
	pred := cal.Effect(t)
	return s.Q.CreateAdjustment(ctx, db.CreateAdjustmentParams{
		CreatedAt: time.Now().Format(time.RFC3339), BeforeID: cur.ID,
		TurnsA: t[0], TurnsB: t[1], TurnsC: t[2], PredDcx: pred.X, PredDcy: pred.Y,
	})
}

// learn refines the calibration from turns t that changed the coma by obs and
// (if hasShift) moved the stars by sh, and saves it. When the two agree, the
// shift stands in for the coma change, because it is far more precise.
func (s *Station) learn(ctx context.Context, cal *collim.Calibration, t collim.Turns, obs, sh collim.Vec, hasShift bool, source string) error {
	updated := false
	if hasShift {
		if k := analysis.ComaPerShift(s.Cfg); collim.Agrees(obs, sh, k) {
			obs = collim.FromShift(sh, k)
		}
		updated = cal.UpdateShift(t, sh, 0.5)
	}
	if cal.Update(t, obs, 0.5) {
		updated = true
	}
	if !updated {
		return nil
	}
	return s.saveCalibration(ctx, *cal, source)
}

// Shift returns the star shift between two measurements, if they are
// comparable (same focuser position, no slew in between).
func Shift(a, b db.Measurement) (collim.Vec, bool) {
	if a.RefFocpos != b.RefFocpos || b.AfterSlew != 0 || a.Positions == "" || b.Positions == "" {
		return collim.Vec{}, false
	}
	var pa, pb []analysis.Point
	if json.Unmarshal([]byte(a.Positions), &pa) != nil || json.Unmarshal([]byte(b.Positions), &pb) != nil {
		return collim.Vec{}, false
	}
	dx, dy, _, ok := analysis.MatchShift(pa, pb, 2500)
	return collim.Vec{X: dx, Y: dy}, ok
}

// Calibration returns the current screw calibration.
func (s *Station) Calibration(ctx context.Context) (collim.Calibration, bool, error) {
	r, err := s.Q.GetCurrentCalibration(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		return collim.Calibration{}, false, nil
	}
	if err != nil {
		return collim.Calibration{}, false, err
	}
	c := collim.Calibration{
		Coma:      [3]collim.Vec{{X: r.ACx, Y: r.ACy}, {X: r.BCx, Y: r.BCy}, {X: r.CCx, Y: r.CCy}},
		Shift:     [3]collim.Vec{{X: r.ASx, Y: r.ASy}, {X: r.BSx, Y: r.BSy}, {X: r.CSx, Y: r.CSy}},
		CMeasured: r.CMeasured != 0,
	}
	return c, c.Valid(), nil
}

func (s *Station) saveCalibration(ctx context.Context, c collim.Calibration, source string) error {
	c.DeriveC()
	return s.Q.CreateCalibration(ctx, db.CreateCalibrationParams{
		CreatedAt: time.Now().Format(time.RFC3339), Source: source,
		ACx: c.Coma[0].X, ACy: c.Coma[0].Y, BCx: c.Coma[1].X, BCy: c.Coma[1].Y, CCx: c.Coma[2].X, CCy: c.Coma[2].Y,
		ASx: c.Shift[0].X, ASy: c.Shift[0].Y, BSx: c.Shift[1].X, BSy: c.Shift[1].Y, CSx: c.Shift[2].X, CSy: c.Shift[2].Y,
		CMeasured: boolInt(c.CMeasured),
	})
}

// GoToField slews to a collimation field about 75° up, just west of the
// meridian, and centres it (with the focuser at focus so plate solving works).
func (s *Station) GoToField() error {
	return s.start(KindSlew, "Slewing to the collimation field", func(ctx context.Context, p progress) error {
		set, err := s.settings(ctx)
		if err != nil {
			return err
		}
		c, err := s.connect(ctx, p, set)
		if err != nil {
			return err
		}
		p.Step("Reading the mount position")
		m, err := c.Mount(ctx)
		if err != nil {
			return err
		}
		if !m.Connected {
			return errors.New("the mount is not connected in N.I.N.A.")
		}
		ra, dec := collim.CollimationField(m.SiderealTime, m.SiteLatitude)
		set.TargetRA, set.TargetDec = ra, dec
		if err := set.Save(ctx, s.Q); err != nil {
			return err
		}
		return s.slewAndCentre(ctx, p, c, set, ra, dec)
	})
}

// Recentre slews back to the stored collimation field and centres it.
func (s *Station) Recentre() error {
	return s.start(KindSlew, "Re-centring", func(ctx context.Context, p progress) error {
		set, err := s.settings(ctx)
		if err != nil {
			return err
		}
		if set.TargetRA == 0 && set.TargetDec == 0 {
			return errors.New("no collimation field yet: use Go to field first")
		}
		c, err := s.connect(ctx, p, set)
		if err != nil {
			return err
		}
		// The field was chosen 1.3 h west of the meridian and keeps moving
		// west. Past about 2.5 h it nears the mount's limit, so pick a
		// fresh field instead.
		m, err := c.Mount(ctx)
		if err != nil {
			return err
		}
		if ha := hourAngle(m.SiderealTime, set.TargetRA); ha > maxFieldHA || ha < -1 {
			ra, dec := collim.CollimationField(m.SiderealTime, m.SiteLatitude)
			p.Note("The field has moved %.1f h west of the meridian, so using a fresh one", ha)
			set.TargetRA, set.TargetDec = ra, dec
			if err := set.Save(ctx, s.Q); err != nil {
				return err
			}
		}
		return s.slewAndCentre(ctx, p, c, set, set.TargetRA, set.TargetDec)
	})
}

// maxFieldHA is how far west of the meridian (hours) a stored collimation
// field may be before Re-centre picks a fresh one.
const maxFieldHA = 2.5

// hourAngle is the hour angle (hours, −12 to 12) of RA raDeg at sidereal
// time lst (hours).
func hourAngle(lst, raDeg float64) float64 {
	return wrap180((lst-raDeg/15)*15) / 15
}

// Centring tolerance and attempts. The field is about 70′ × 47′, so a few
// arcminutes is plenty.
const (
	centreTolArcmin = 3.0
	centreAttempts  = 4
)

// slewAndCentre slews to ra/dec (degrees), then plate-solves and re-slews by
// the measured offset until the field is centred. It corrects by offsetting
// the target rather than syncing, so TheSkyX's pointing model is untouched.
func (s *Station) slewAndCentre(ctx context.Context, p progress, c *nina.Client, set Settings, ra, dec float64) error {
	if set.FocusPos != 0 {
		p.Step("Focusing at %d so N.I.N.A. can plate solve", set.FocusPos)
		if err := moveFocuser(ctx, c, set.FocusPos); err != nil {
			return err
		}
	}
	defer func() {
		s.mu.Lock()
		s.slewed = true
		s.status = nil // refresh the header light now
		s.mu.Unlock()
	}()
	aimRA, aimDec := ra, dec
	slew := func(text string) error {
		p.Step("%s", text)
		return c.Slew(ctx, aimRA, aimDec, func(m nina.MountInfo) {
			p.Note("%s · mount at RA %.2f h, Dec %+.1f°", text, m.RightAscension, m.Declination)
		})
	}
	if err := slew(fmt.Sprintf("Slewing to RA %.2f h, Dec %+.1f°", ra/15, dec)); err != nil {
		return err
	}
	if set.FocusPos == 0 {
		p.Step("Not centred: set the best-focus position in Setup so N.I.N.A. can plate solve")
		return nil
	}
	for attempt := 1; ; attempt++ {
		p.Step("Plate solving to check the centring")
		if err := sleepCtx(ctx, 2*time.Second); err != nil { // let the mount settle
			return err
		}
		sol, err := c.Solve(ctx, set.ExposureS, set.Gain)
		if err != nil {
			return fmt.Errorf("on the field but not centred: %w. Measuring works anyway", err)
		}
		dRA := wrap180(sol.Coordinates.RADegrees - ra)
		dDec := sol.Coordinates.DECDegrees - dec
		off := 60 * math.Hypot(dRA*math.Cos(dec*math.Pi/180), dDec)
		if off <= centreTolArcmin {
			p.Note("Centred: %.1f′ from the target", off)
			return nil
		}
		p.Note("Plate solved: %.1f′ from the target", off)
		if attempt >= centreAttempts {
			return fmt.Errorf("still %.0f′ from the target after %d tries. Measuring works anyway", off, attempt)
		}
		aimRA = math.Mod(aimRA-dRA+360, 360)
		aimDec = math.Max(-90, math.Min(90, aimDec-dDec))
		if err := slew(fmt.Sprintf("Correcting by %.0f′", off)); err != nil {
			return err
		}
	}
}

// wrap180 wraps an angle in degrees to (−180, 180].
func wrap180(a float64) float64 {
	a = math.Mod(a, 360)
	switch {
	case a > 180:
		a -= 360
	case a <= -180:
		a += 360
	}
	return a
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// Runs lists the runs of FITS frames in a folder.
func (s *Station) Runs(dir string) ([]analysis.Run, error) {
	files, err := fitsFiles(dir)
	if err != nil {
		return nil, err
	}
	var infos []analysis.FrameInfo
	for _, f := range files {
		fi, err := analysis.ReadInfo(f, s.Cfg)
		if err != nil {
			continue
		}
		infos = append(infos, fi)
	}
	return analysis.SplitRuns(infos, 15*time.Minute), nil
}

// AnalyseRun analyses one run of saved frames.
func (s *Station) AnalyseRun(dir string, index int) error {
	runs, err := s.Runs(dir)
	if err != nil {
		return err
	}
	if index < 0 || index >= len(runs) {
		return fmt.Errorf("run %d not found", index+1)
	}
	run := runs[index]
	return s.start(KindAnalyse, run.Label(), func(ctx context.Context, p progress) error {
		var paths []string
		for _, f := range run.Frames {
			paths = append(paths, f.Path)
		}
		p.Step("Analysing %d frames", len(paths))
		res, err := analysis.Analyze(ctx, paths, s.Cfg, func(m string) { p.Note("Analysing: %s", m) })
		if err != nil {
			return err
		}
		id, err := s.save(ctx, KindAnalyse, res)
		if err != nil {
			return err
		}
		p.setResult(id)
		return nil
	})
}
