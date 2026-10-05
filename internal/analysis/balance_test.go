package analysis

import (
	"math"
	"math/rand/v2"
	"testing"

	"github.com/exploded/collimation/internal/model"
)

// Best focus a + εx·x + εy·y + c·r² sampled on a 3×3 grid comes back.
func TestDeriveTilt(t *testing.T) {
	cfg := DefaultConfig()
	res := &Result{NRatio: 3.79, Sides: make([]SideSummary, 2), Infos: []FrameInfo{{W: 6248, H: 4176}}}
	ex, ey, curv := 2.0, -1.2, 0.3 // mrad, mrad, µm/mm²
	umPerPx := res.NRatio * cfg.PixelUM
	rng := rand.New(rand.NewPCG(5, 6))
	for _, x := range []float64{-7.8, 0, 7.8} {
		for _, y := range []float64{-5.2, 0, 5.2} {
			focus := 4 + ex*x + ey*y + curv*(x*x+y*y) // µm toward higher positions
			// Higher position side shrinks, lower grows: |K_lo| − |K_hi| = focus/umPerPx.
			d := focus/umPerPx + rng.NormFloat64()*0.02
			res.Regions = append(res.Regions, RegionResult{OK: true, Xmm: x, Ymm: y, K: []float64{-(55 + d/2), 55 - d/2}})
		}
	}
	deriveTilt(res, cfg)
	tl := res.Tilt
	if !tl.OK || math.Abs(tl.X-ex) > 0.1 || math.Abs(tl.Y-ey) > 0.1 || math.Abs(tl.Curv-curv) > 0.02 {
		t.Errorf("tilt (%.3f, %.3f) curv %.3f ok %v, want (%.1f, %.1f) %.1f", tl.X, tl.Y, tl.Curv, tl.OK, ex, ey, curv)
	}
	// ε = 2φ(1 − d/f): f = 1220, d = 275.
	if want := tl.Mag() / (2 * (1 - 275.0/1220)); math.Abs(tl.SecondaryMrad-want) > 1e-9 {
		t.Errorf("secondary %.3f mrad, want %.3f", tl.SecondaryMrad, want)
	}
}

// Render regions whose shadow and spider shift with field position, run the
// pupil fits, and recover the shadow, hub and the side of focus.
func TestPupilRoundTrip(t *testing.T) {
	if testing.Short() {
		t.Skip("slow")
	}
	cfg := DefaultConfig()
	nRatio := 3.79
	F, R := nRatio*cfg.ApertureMM, cfg.ApertureMM/2
	kGeo := cfg.SecondaryHeightMM / (F * R)
	// The higher focuser position is outside focus, so in the fit frame the
	// shadow moves against the field position.
	kFit := -kGeo
	s0 := [2]float64{0.06, -0.02}
	v0 := [2]float64{0.02, 0.01}
	base := model.Shared{Cx: -0.5, Cy: 0.4, Gx: 0.08, Gy: -0.04, Sigma: 3.5, Eps: 0.36, SA: 3,
		VaneW: 0.012, VaneAng: 0.05}
	n := 187
	rng := rand.New(rand.NewPCG(7, 8))
	lo := model.Side{K: -55, Amp: 1}
	hi := model.Side{K: 60, Amp: 1}

	type region struct {
		x, y  float64
		sides []SideData
	}
	var regions []region
	glob := []SideData{{Pos: 3366, Sign: -1, N: n, Stack: make([]float64, n*n)}, {Pos: 4375, Sign: 1, N: n, Stack: make([]float64, n*n)}}
	for _, x := range []float64{-7.8, 0, 7.8} {
		for _, y := range []float64{-5.2, 0, 5.2} {
			sh := base
			sh.Sx, sh.Sy = s0[0]+kFit*x, s0[1]+kFit*y
			sh.Vx, sh.Vy = v0[0]+kFit*x, v0[1]+kFit*y
			r := region{x: x, y: y}
			for i, sd := range []model.Side{lo, hi} {
				st := synthSide(sh, sd, n, 0.01, rng)
				r.sides = append(r.sides, SideData{Pos: glob[i].Pos, Sign: glob[i].Sign, N: n, Stack: st})
				for j, v := range st {
					glob[i].Stack[j] += v / 9
				}
			}
			regions = append(regions, r)
		}
	}
	global := FitDonuts(glob, nil, FitAll, DefaultStarts(glob))
	res := &Result{NRatio: nRatio, Fit: global, Sides: make([]SideSummary, 2)}
	res.Pupil.Fit = fitPupil(glob, global)
	res.Pupil.OK = true
	ps := res.Pupil.Fit.Shared
	t.Logf("global pupil: s (%.3f, %.3f) vane w %.4f at %.1f°, hub (%.3f, %.3f)", ps.Sx, ps.Sy, ps.VaneW, ps.VaneAng*180/math.Pi, ps.Vx, ps.Vy)
	for _, r := range regions {
		c := FitDonuts(r.sides, &global.Shared, FitComaOnly, nil)
		st := startPupilRegion(ps, c.Shared)
		p := FitDonuts(r.sides, &st, FitPupilShift, nil)
		res.Regions = append(res.Regions, RegionResult{OK: true, PupilOK: true, Xmm: r.x, Ymm: r.y,
			Sx: p.Shared.Sx, Sy: p.Shared.Sy, Vx: p.Shared.Vx, Vy: p.Shared.Vy})
	}
	derivePupil(res, cfg)
	g := res.Pupil
	t.Logf("slope %.5f ± %.5f (truth %.5f), shadow (%.2f, %.2f) mm, hub (%.2f, %.2f) mm ± %.2f", g.Slope, g.SlopeErr, kFit, g.ShadowX, g.ShadowY, g.HubX, g.HubY, g.ErrMM)
	if g.IntraHigh != -1 {
		t.Fatalf("IntraHigh = %d, want -1", g.IntraHigh)
	}
	if !g.Hub {
		t.Fatalf("spider not found")
	}
	// Physical frame is the fit frame flipped; the axis is at the sensor centre.
	check := func(name string, got, want float64) {
		if math.Abs(got-want) > 1.0 {
			t.Errorf("%s = %.2f mm, want %.2f", name, got, want)
		}
	}
	check("shadow x", g.ShadowX, -s0[0]*R)
	check("shadow y", g.ShadowY, -s0[1]*R)
	check("hub x", g.HubX, -v0[0]*R)
	check("hub y", g.HubY, -v0[1]*R)
}

// A region's coma-only fit resolves the half-pixel change in defocus that
// 1 mrad of tilt gives 8 mm from the centre.
func TestRegionKResolvesTilt(t *testing.T) {
	if testing.Short() {
		t.Skip("slow")
	}
	rng := rand.New(rand.NewPCG(9, 10))
	sh := model.Shared{Cx: -2.8, Cy: 2.4, Sx: 0.08, Gx: 0.1, Sigma: 3.8, Eps: 0.36, SA: 3.5, VaneW: 0.012}
	n := 187
	for _, d := range []float64{0, 0.5} { // |K_lo| − |K_hi| (px)
		sides := []SideData{
			{Pos: 3366, Sign: -1, N: n, Stack: synthSide(sh, model.Side{K: -(57.5 + d/2), Amp: 1}, n, 0.02, rng)},
			{Pos: 4375, Sign: 1, N: n, Stack: synthSide(sh, model.Side{K: 57.5 - d/2, Amp: 1}, n, 0.02, rng)},
		}
		start := sh
		start.VaneW = 0
		f := FitDonuts(sides, &start, FitComaOnly, nil)
		got := math.Abs(f.Sides[0].K) - math.Abs(f.Sides[1].K)
		if math.Abs(got-d) > 0.1 {
			t.Errorf("|K_lo| − |K_hi| = %.3f, want %.2f", got, d)
		}
	}
}
