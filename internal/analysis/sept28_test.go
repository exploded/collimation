package analysis

import (
	"context"
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestSept28 runs the engine on the 28 Sep 2026 frames (two sets, at ±150
// and about ±500 focuser steps) when they are present. Set 1 must match the
// Python prototype; set 2 must agree with set 1 (the prototype's larger set 2
// value did not reproduce).
func TestSept28(t *testing.T) {
	dir := filepath.Join("..", "..", "images", "2026-09-28_Snapshot", "SNAPSHOT")
	if _, err := os.Stat(dir); err != nil || testing.Short() {
		t.Skip("28 Sep frames not present")
	}
	run := func(pattern string, focus int) *Result {
		files, _ := filepath.Glob(filepath.Join(dir, pattern))
		cfg := DefaultConfig()
		cfg.FocusPos = focus
		t0 := time.Now()
		res, err := Analyze(context.Background(), files, cfg, nil)
		if err != nil {
			t.Fatal(err)
		}
		if d := time.Since(t0); d > 20*time.Second {
			t.Errorf("%s took %v, want < 20 s", pattern, d)
		}
		t.Logf("%s: c=(%+.2f, %+.2f) ± %.2f, axis (%+.2f, %+.2f) mm, eps %.3f, σ %.2f, %.2f µm/step, focus %.0f",
			pattern, res.ComaX, res.ComaY, res.ComaErr, res.AxisXmm, res.AxisYmm, res.Fit.Shared.Eps, res.Fit.Shared.Sigma, res.StepUM, res.ParaxialFocus)
		tl := res.Tilt
		t.Logf("  tilt (%+.2f, %+.2f) ± %.2f mrad over %d regions, curv %+.3f µm/mm², corner %.0f µm",
			tl.X, tl.Y, tl.Err, tl.Regions, tl.Curv, tl.CornerUM)
		for _, r := range res.Regions {
			t.Logf("  region %d,%d at (%+5.1f, %+5.1f) mm: %3d stars, K %v, c (%+.2f, %+.2f), s (%+.3f, %+.3f), hub (%+.3f, %+.3f)",
				r.Row, r.Col, r.Xmm, r.Ymm, r.Stars, r.K, r.Cx, r.Cy, r.Sx, r.Sy, r.Vx, r.Vy)
		}
		if g := res.Pupil; g.OK {
			ps := g.Fit.Shared
			t.Logf("  pupil fit: s (%+.3f, %+.3f) g (%+.3f, %+.3f) eps %.3f vane w %.4f ± %.4f at %.1f°, hub (%+.3f, %+.3f), rms %.4f vs %.4f",
				ps.Sx, ps.Sy, ps.Gx, ps.Gy, ps.Eps, ps.VaneW, g.Fit.SharedSigma.VaneW, ps.VaneAng*180/math.Pi, ps.Vx, ps.Vy, g.Fit.RMS, res.Fit.RMS)
			t.Logf("  slope %+.4f ± %.4f /mm (geometry %.4f, u ≈ %.0f mm), intra-high %+d, shadow (%+.1f, %+.1f) mm, hub (%+.1f, %+.1f) mm ± %.1f, tube %.1f mrad",
				g.Slope, g.SlopeErr, g.SlopeGeo, g.HeightMM, g.IntraHigh, g.ShadowX, g.ShadowY, g.HubX, g.HubY, g.ErrMM, g.TubeMrad)
		}
		return res
	}
	s1 := run("2026-09-28_21-*.fits", 0)
	s2 := run("2026-09-28_22-*.fits", 0)

	if d := math.Hypot(s1.ComaX+2.96, s1.ComaY-2.38); d > 0.5 {
		t.Errorf("set 1 coma (%.2f, %.2f) is %.2f px from the prototype (-2.96, +2.38)", s1.ComaX, s1.ComaY, d)
	}
	if d := math.Hypot(s2.ComaX-s1.ComaX, s2.ComaY-s1.ComaY); d > 0.6 {
		t.Errorf("set 2 coma differs from set 1 by %.2f px", d)
	}
	for _, r := range []*Result{s1, s2} {
		if r.StepUM < 3.0 || r.StepUM > 3.6 {
			t.Errorf("focuser scale %.2f µm/step, want about 3.36", r.StepUM)
		}
		// Coma tail toward −x, +y, so the axis lands toward +x, −y.
		if r.AxisXmm <= 0 || r.AxisYmm >= 0 {
			t.Errorf("axis (%.2f, %.2f) mm not in the +x, −y quadrant", r.AxisXmm, r.AxisYmm)
		}
		if r.Fit.Shared.Eps < 0.33 {
			t.Errorf("obstruction %.3f below the 4\" secondary's 0.33", r.Fit.Shared.Eps)
		}
	}
}
