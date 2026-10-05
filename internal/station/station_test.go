package station

import (
	"context"
	"encoding/json"
	"math"
	"math/rand/v2"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/exploded/collimation/internal/analysis"
	"github.com/exploded/collimation/internal/collim"
	"github.com/exploded/collimation/internal/db"
	"github.com/exploded/collimation/internal/nina"
)

func newTestStation(t *testing.T) (*Station, *nina.Stub) {
	dir := filepath.Join("..", "..", "images", "2026-09-28_Snapshot", "SNAPSHOT")
	if _, err := os.Stat(dir); err != nil || testing.Short() {
		t.Skip("28 Sep frames not present")
	}
	stub, err := nina.NewStub(dir)
	if err != nil {
		t.Fatal(err)
	}
	stub.ExposureScale = 0
	srv := httptest.NewServer(stub)
	t.Cleanup(srv.Close)
	conn, err := db.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	st := New(conn, analysis.DefaultConfig())
	st.NewClient = func(Settings) *nina.Client {
		c := nina.New("x", 1)
		c.Base = srv.URL + "/v2/api"
		c.Poll = 20 * time.Millisecond
		return c
	}
	set := DefaultSettings()
	set.FocusPos = 3870
	set.DefocusSteps = 505 // → 3365 and 4375, the second 28 Sep set
	set.FramesPerSide = 2
	if err := set.Save(context.Background(), st.Q); err != nil {
		t.Fatal(err)
	}
	return st, stub
}

func wait(t *testing.T, st *Station) *Job {
	t.Helper()
	deadline := time.Now().Add(2 * time.Minute)
	for time.Now().Before(deadline) {
		if j := st.Job(); j != nil && j.Done {
			return j
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("job did not finish")
	return nil
}

func TestMeasureSuggestAdjust(t *testing.T) {
	st, _ := newTestStation(t)
	ctx := context.Background()

	if err := st.Measure(); err != nil {
		t.Fatal(err)
	}
	j := wait(t, st)
	if j.Err != "" {
		t.Fatalf("measure failed: %s (steps %+v)", j.Err, j.Steps)
	}
	m, err := st.Q.GetMeasurement(ctx, j.ResultID)
	if err != nil {
		t.Fatal(err)
	}
	if m.DecentreMm < 2 || m.Frames != 4 {
		t.Errorf("measurement: decentre %.2f mm from %d frames", m.DecentreMm, m.Frames)
	}
	if _, err := st.Q.GetPendingAdjustment(ctx); err == nil {
		t.Error("suggested an adjustment without a calibration")
	}

	// With a calibration, the next measurement gives a suggestion.
	cal := collim.Calibration{Coma: [3]collim.Vec{{X: 1, Y: 0}, {X: -0.5, Y: 0.87}}}
	if err := st.saveCalibration(ctx, cal, "test"); err != nil {
		t.Fatal(err)
	}
	if err := st.Measure(); err != nil {
		t.Fatal(err)
	}
	if j := wait(t, st); j.Err != "" {
		t.Fatal(j.Err)
	}
	pend, err := st.Q.GetPendingAdjustment(ctx)
	if err != nil {
		t.Fatal("no suggestion after a measurement with a calibration")
	}
	turns := collim.Turns{pend.TurnsA, pend.TurnsB, pend.TurnsC}
	if len(turns.Steps()) == 0 {
		t.Errorf("empty suggestion %+v", pend)
	}
	// Measuring again closes the suggestion (the stub frames do not change,
	// so nothing moved and the calibration must not be "learned" from it).
	if err := st.Measure(); err != nil {
		t.Fatal(err)
	}
	if j := wait(t, st); j.Err != "" {
		t.Fatal(j.Err)
	}
	adj, _ := st.Q.ListRecentAdjustments(ctx, 5)
	if len(adj) < 2 || adj[1].Status != "done" || adj[0].Status != "pending" {
		t.Errorf("adjustments: %+v", adj)
	}
	cur, _, _ := st.Calibration(ctx)
	if cur.Coma[0] != cal.Coma[0] {
		t.Errorf("calibration changed from a no-op adjustment: %v", cur.Coma)
	}
}

func TestLiveMode(t *testing.T) {
	st, _ := newTestStation(t)
	if err := st.StartLive(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		if l := st.Live(); l.HasReading {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	l := st.Live()
	st.Stop()
	j := wait(t, st)
	if !l.HasReading {
		t.Fatalf("no steady reading after %d frames (status %s); job %+v", l.Frames, l.Status, j)
	}
	// The stub cycles two real frames at 4375; their stars barely move.
	if l.Decentre < 2 || l.Decentre > 6 {
		t.Errorf("live decentre %.2f mm", l.Decentre)
	}
	if j.Err != "Stopped." {
		t.Errorf("job error %q, want Stopped.", j.Err)
	}
}

// The stub returns the same frames whatever is "turned", so the wizard must
// refuse to save and stay on screw B.
func TestWizardRefusesNoChange(t *testing.T) {
	st, _ := newTestStation(t)
	ctx := context.Background()
	if err := st.WizardStart(ctx); err != nil {
		t.Fatal(err)
	}
	for i := range 3 {
		if err := st.WizardMeasure(); err != nil {
			t.Fatal(err)
		}
		j := wait(t, st)
		if i < 2 && j.Err != "" {
			t.Fatalf("measure %d: %s", i, j.Err)
		}
		if i == 2 && j.Err == "" {
			t.Fatal("saved a calibration from screws that changed nothing")
		}
	}
	if w := st.Wizard(); w.Stage != 2 || w.Saved || !w.Retrying() {
		t.Errorf("wizard stage %d saved %v retrying %v; want stage 2, not saved, retrying", w.Stage, w.Saved, w.Retrying())
	}
	if _, ok, _ := st.Calibration(ctx); ok {
		t.Error("a calibration was stored")
	}
}

// TestWizardReplay replays the 5 Oct 2026 calibration: the first ¼ turn of B
// barely moved the mirror (its lock still bit), so B was turned again. B must
// be measured from the rejected reading, and the coma taken from the star
// shifts, which agree with it.
func TestWizardReplay(t *testing.T) {
	conn, err := db.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	st := New(conn, analysis.DefaultConfig())
	ctx := context.Background()
	if err := DefaultSettings().Save(ctx, st.Q); err != nil { // ¼-turn steps
		t.Fatal(err)
	}
	rng := rand.New(rand.NewPCG(5, 10))
	var stars []analysis.Point
	for range 60 {
		stars = append(stars, analysis.Point{1000 + rng.Float64()*4000, 800 + rng.Float64()*2500})
	}
	steps := []struct{ coma, shift collim.Vec }{
		{collim.Vec{X: -3.095, Y: 2.274}, collim.Vec{}},                     // baseline
		{collim.Vec{X: -3.104, Y: 1.787}, collim.Vec{X: 102.9, Y: 243.7}},   // A
		{collim.Vec{X: -3.172, Y: 2.315}, collim.Vec{X: -64.9, Y: -203.2}},  // B, lock biting
		{collim.Vec{X: -1.500, Y: 3.879}, collim.Vec{X: -683.1, Y: -902.3}}, // B again
	}
	var ids []int64
	var at collim.Vec
	for _, s := range steps {
		at = at.Add(s.shift)
		var pos []analysis.Point
		for _, p := range stars {
			pos = append(pos, analysis.Point{p[0] + at.X, p[1] + at.Y})
		}
		pj, _ := json.Marshal(pos)
		if err := st.Q.CreateMeasurement(ctx, db.CreateMeasurementParams{
			CreatedAt: time.Now().Format(time.RFC3339), Kind: KindCalibrate,
			ComaX: s.coma.X, ComaY: s.coma.Y, DecentreMm: 4, RefFocpos: 4352, Positions: string(pj),
		}); err != nil {
			t.Fatal(err)
		}
		id, _ := st.Q.GetLastMeasurementID(ctx)
		ids = append(ids, id)
	}

	w := &Wizard{Active: true, IDs: [4]int64{ids[0], ids[1], ids[2]}}
	if err := st.wizardSave(ctx, w, 3); err == nil {
		t.Fatal("accepted B's first turn, which barely moved the mirror")
	}
	w.From[1], w.IDs[2] = ids[2], ids[3] // as WizardMeasure does on a retry
	if err := st.wizardSave(ctx, w, 3); err != nil {
		t.Fatal(err)
	}
	cal, ok, err := st.Calibration(ctx)
	if err != nil || !ok {
		t.Fatalf("no valid calibration: %v", err)
	}
	k := analysis.ComaPerShift(st.Cfg)
	for i, sh := range []collim.Vec{steps[1].shift, steps[3].shift} {
		want := collim.FromShift(sh, k).Scale(0.5)
		if d := cal.Coma[i].Sub(want).Len(); d > 1e-3 {
			t.Errorf("screw %s: %v per ⅛ turn, want %v from the star shift", collim.Screws[i], cal.Coma[i], want)
		}
		if d := cal.Shift[i].Sub(sh.Scale(0.5)).Len(); d > 1 {
			t.Errorf("screw %s: shift %v per ⅛ turn, want %v", collim.Screws[i], cal.Shift[i], sh.Scale(0.5))
		}
	}
}

// Inside the tolerance, a fine-tune is suggested only if it helps by more
// than the measurement scatter. Calibration and readings from 5 Oct 2026.
func TestNextTurnsFineTune(t *testing.T) {
	cal := collim.Calibration{
		Coma:      [3]collim.Vec{{X: 0.226, Y: -0.660}, {X: 0.281, Y: 0.650}, {X: -0.478, Y: 0.027}},
		CMeasured: true,
	}
	set, cfg := DefaultSettings(), analysis.DefaultConfig()
	if tr, ok := nextTurns(cal, collim.Vec{X: 0.229, Y: 0.557}, set, cfg); !ok || tr != (collim.Turns{0, -0.5, 0}) {
		t.Errorf("0.61 mm: turns %v ok %v; want B 1/16 anticlockwise", tr, ok)
	}
	if tr, ok := nextTurns(cal, collim.Vec{X: 0.05, Y: 0.1}, set, cfg); ok {
		t.Errorf("0.11 mm: suggested %v; nothing helps by 0.2 mm", tr)
	}
	if _, ok := nextTurns(cal, collim.Vec{X: -2.88, Y: 2.30}, set, cfg); !ok {
		t.Error("3.7 mm: no suggestion")
	}
}

// A turn made in live mode is reported once the stars settle, with the
// suggestion that was showing before it.
func TestLiveSeesMove(t *testing.T) {
	cal := collim.Calibration{
		Coma:  [3]collim.Vec{{X: -0.1, Y: -0.24}, {X: 0.42, Y: 0.39}, {X: -0.55, Y: -0.04}},
		Shift: [3]collim.Vec{{X: 51, Y: 122}, {X: -214, Y: -199}, {X: 289, Y: 13}},
	}
	set := DefaultSettings()
	stars := []analysis.Point{{100, 200}, {900, 450}, {2500, 3000}, {4000, 1200}, {5100, 2600}, {3300, 700}}
	frame := func(dx, dy, cx float64) *analysis.FrameResult {
		var pos []analysis.Point
		for _, p := range stars {
			pos = append(pos, analysis.Point{p[0] + dx, p[1] + dy})
		}
		ax, ay := analysis.ComaToAxisMM(cx, 2.3, analysis.DefaultConfig())
		return &analysis.FrameResult{ComaX: cx, ComaY: 2.3, AxisXmm: ax, AxisYmm: ay, Positions: pos}
	}
	l := Live{cfg: analysis.DefaultConfig()}
	for range 2 {
		if mv := l.update(frame(0, 0, -2.9), nil, cal, true, set); mv != nil {
			t.Fatal("move reported before any adjustment")
		}
	}
	shown := l.Turns
	if !l.HasTurns {
		t.Fatal("no suggestion")
	}
	l.update(frame(-400, -300, -2.0), nil, cal, true, set) // the stars jump
	if l.Status != LiveMoving {
		t.Fatalf("status %s, want moving", l.Status)
	}
	l.update(frame(-400, -300, -2.0), nil, cal, true, set)
	mv := l.update(frame(-400, -300, -2.0), nil, cal, true, set)
	if mv == nil {
		t.Fatal("settled after a move but no move reported")
	}
	if mv.turns != shown || math.Abs(mv.shift.X+400) > 2 || math.Abs(mv.shift.Y+300) > 2 || math.Abs(mv.coma.X-0.9) > 1e-9 {
		t.Errorf("move %+v; want turns %v, shift (-400, -300), coma +0.9", mv, shown)
	}
	if mv := l.update(frame(-400, -300, -2.0), nil, cal, true, set); mv != nil {
		t.Error("move reported again on the next steady frame")
	}
}

// TestGoToField slews, finds the stub's pointing error by plate solving, and
// corrects it by offsetting the target.
func TestGoToField(t *testing.T) {
	stub, err := nina.NewStub(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(stub)
	t.Cleanup(srv.Close)
	conn, err := db.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	st := New(conn, analysis.DefaultConfig())
	st.NewClient = func(Settings) *nina.Client {
		c := nina.New("x", 1)
		c.Base = srv.URL + "/v2/api"
		c.Poll = 20 * time.Millisecond
		return c
	}
	set := DefaultSettings()
	set.FocusPos = 3870
	if err := set.Save(context.Background(), st.Q); err != nil {
		t.Fatal(err)
	}

	if err := st.GoToField(); err != nil {
		t.Fatal(err)
	}
	j := wait(t, st)
	if j.Err != "" {
		t.Fatalf("slew failed: %s (steps %+v)", j.Err, j.Steps)
	}
	last := j.Steps[len(j.Steps)-1].Text
	if !strings.HasPrefix(last, "Centred") {
		t.Errorf("last step %q, want Centred", last)
	}
	var corrected bool
	for _, s := range j.Steps {
		corrected = corrected || strings.HasPrefix(s.Text, "Correcting by")
	}
	if !corrected {
		t.Errorf("no correcting slew: %+v", j.Steps)
	}
}
