package analysis

import (
	"fmt"
	"math"

	"github.com/exploded/collimation/internal/model"
)

// Mirror balance: whether the two mirrors are fighting each other.
//
// Nulling coma with the primary alone leaves any secondary tilt error φ as a
// focal-plane tilt ε = 2φ(1 − d/f), with the primary pulled 2φ·d/f away from
// its ideal angle. So the tilt says how far the mirrors are compensating.
//
// The donut is an image of the pupil, so it also shows where the secondary's
// silhouette and the spider hub sit relative to the primary's axis. Both
// shift across the field by u·h/F (u the secondary's height above the
// primary, h the star's position on the sensor). That known slope tells
// which side of focus is inside and separates the shadow from the
// illumination gradient.

// Tilt is the focal-plane tilt from per-region defocus on both sides.
type Tilt struct {
	OK      bool
	Rough   bool // donuts too small for a reliable tilt
	Regions int
	// X, Y: best focus moves toward higher focuser positions by this many µm
	// per mm toward +x/+y on the sensor (mrad).
	X, Y     float64
	Err      float64 // 1σ on each component (mrad)
	Curv     float64 // best-focus change per mm² from the centre (µm/mm²)
	CornerUM float64 // focus difference, centre to the worst corner, from tilt alone
	// What the tilt implies if it comes from the mirrors: the secondary's
	// error and the angle the primary has been pulled to compensate (mrad).
	SecondaryMrad float64
	PrimaryMrad   float64
}

// Mag is the tilt magnitude (mrad).
func (t Tilt) Mag() float64 { return math.Hypot(t.X, t.Y) }

// PupilGeometry is where the secondary and spider sit in the beam.
type PupilGeometry struct {
	OK      bool
	Regions int
	Fit     FitResult // global pupil fit (spider free)
	// Slope is the fitted shadow shift per mm of field position (pupil
	// units), SlopeGeo the value from the tube geometry. The sign of Slope
	// gives the side of focus: + means the higher focuser position is inside
	// focus.
	Slope, SlopeErr, SlopeGeo float64
	IntraHigh                 int     // +1 higher position inside focus, −1 lower, 0 not determined
	HeightMM                  float64 // secondary height implied by Slope
	// Positions at the secondary relative to the primary's axis (mm, image
	// axes), for a star on that axis. Signs are only meaningful when
	// IntraHigh is known.
	ShadowX, ShadowY float64 // secondary silhouette centre
	HubX, HubY       float64 // spider hub
	Hub              bool    // spider found
	ErrMM            float64 // 1σ on each component
	TubeMrad         float64 // angle between the primary's axis and the hub (tube axis)
	// Rough: too noisy to trust (thin cloud, few stars). Why says which test
	// failed.
	Rough bool
	Why   string
}

// Limits for a trustworthy pupil fit: the position error, and how far the
// height implied by the shadow drift may stray from the assumed height.
const (
	maxPupilErrMM = 2.0
	minHeightFrac = 0.6
	maxHeightFrac = 1.25 // the front ring is only about 1070 mm up
)

// minPupilK is the smallest defocus radius (px) worth a pupil fit: smaller
// donuts don't resolve the spider.
const minPupilK = 30

// fitPupil frees the shadow, gradient, obstruction and spider on top of the
// global fit. The vane angle starts where the residual is darkest.
func fitPupil(sides []SideData, global FitResult) FitResult {
	sh := global.Shared
	sh.VaneW = 0.015
	sh.VaneAng = vaneAngle(sides, global)
	sh.Vx, sh.Vy = sh.Sx, sh.Sy
	return FitDonuts(sides, &sh, FitPupil, nil)
}

// vaneAngle estimates the spider angle (radians, 0 to π/2) as the darkest
// direction, folded every 90°, in the residual between the stacks and the
// vane-free model.
func vaneAngle(sides []SideData, fr FitResult) float64 {
	const nb = 90
	var sum [nb]float64
	var cnt [nb]float64
	for i, d := range sides {
		m := RenderModel(fr, d, i)
		sd := fr.Sides[i]
		k := math.Abs(sd.K)
		c := float64(d.N-1) / 2
		r0, r1 := (fr.Shared.Eps+0.12)*k, 0.88*k
		for y := range d.N {
			for x := range d.N {
				dx, dy := float64(x)-c-sd.X0, float64(y)-c-sd.Y0
				r := math.Hypot(dx, dy)
				if r < r0 || r > r1 {
					continue
				}
				a := math.Mod(math.Atan2(dy, dx)+2*math.Pi, math.Pi/2)
				b := int(a/(math.Pi/2)*nb) % nb
				sum[b] += d.Stack[y*d.N+x] - m[y*d.N+x]
				cnt[b]++
			}
		}
	}
	best, bestV := 0, math.Inf(1)
	for b := range nb {
		var s, n float64
		for o := -2; o <= 2; o++ {
			j := (b + o + nb) % nb
			s += sum[j]
			n += cnt[j]
		}
		if n > 0 && s/n < bestV {
			best, bestV = b, s/n
		}
	}
	return (float64(best) + 0.5) / nb * math.Pi / 2
}

// deriveTilt fits best focus a + εx·x + εy·y + c·r² over the regions, with
// best focus from the defocus on each side: (|K_lo| − |K_hi|)·N·pixel.
func deriveTilt(res *Result, cfg Config) {
	if len(res.Sides) != 2 {
		return
	}
	var rows [][]float64
	var ys []float64
	for _, r := range res.Regions {
		if !r.OK || len(r.K) != 2 {
			continue
		}
		focus := (math.Abs(r.K[0]) - math.Abs(r.K[1])) * res.NRatio * cfg.PixelUM
		rows = append(rows, []float64{1, r.Xmm, r.Ymm, r.Xmm*r.Xmm + r.Ymm*r.Ymm})
		ys = append(ys, focus)
	}
	t := Tilt{Regions: len(rows)}
	if len(rows) < 5 {
		res.Tilt = t
		return
	}
	x, sig, ok := lstsq(rows, ys)
	if !ok {
		res.Tilt = t
		return
	}
	t.OK = true
	t.X, t.Y, t.Curv = x[1], x[2], x[3]
	if minAbsK(res.Fit) < minPupilK {
		// Near focus the defocus radius trades off against spherical
		// aberration and seeing; the 28 Sep ±150-step set disagreed with the
		// ±500-step set by 2 mrad.
		t.Rough = true
		res.Warnings = append(res.Warnings, "Tilt from small donuts is rough. Use ±450–550 focuser steps for a tilt measurement.")
	}
	t.Err = math.Max(sig[1], sig[2])
	// Worst corner of the sensor from the tilt alone.
	hw, hh := float64(res.Infos[0].W)/2*cfg.PixelUM/1000, float64(res.Infos[0].H)/2*cfg.PixelUM/1000
	t.CornerUM = math.Abs(t.X)*hw + math.Abs(t.Y)*hh
	f := cfg.PrimaryFRatio * cfg.ApertureMM
	d := cfg.SecondaryToFocusMM
	t.SecondaryMrad = t.Mag() / (2 * (1 - d/f))
	t.PrimaryMrad = t.Mag() * d / (f - d)
	res.Tilt = t
}

// derivePupil regresses the per-region shadow and hub positions on field
// position: s(h) = s0 + k·h, with one slope k shared by both.
func derivePupil(res *Result, cfg Config) {
	g := &res.Pupil
	if !g.OK {
		return
	}
	g.Hub = g.Fit.Shared.VaneW > 0.004 && g.Fit.Shared.VaneW > 3*g.Fit.SharedSigma.VaneW
	// Unknowns: s0x, s0y, v0x, v0y, k.
	var rows [][]float64
	var ys []float64
	n := 0
	for _, r := range res.Regions {
		if !r.PupilOK {
			continue
		}
		n++
		rows = append(rows, []float64{1, 0, 0, 0, r.Xmm}, []float64{0, 1, 0, 0, r.Ymm})
		ys = append(ys, r.Sx, r.Sy)
		if g.Hub {
			rows = append(rows, []float64{0, 0, 1, 0, r.Xmm}, []float64{0, 0, 0, 1, r.Ymm})
			ys = append(ys, r.Vx, r.Vy)
		}
	}
	g.Regions = n
	if !g.Hub {
		// Keep the system square: pin the hub terms at zero.
		rows = append(rows, []float64{0, 0, 1, 0, 0}, []float64{0, 0, 0, 1, 0})
		ys = append(ys, 0, 0)
	}
	if n < 4 {
		g.OK = false
		return
	}
	x, sig, ok := lstsq(rows, ys)
	if !ok {
		g.OK = false
		return
	}
	F := res.NRatio * cfg.ApertureMM
	R := cfg.ApertureMM / 2
	g.Slope, g.SlopeErr = x[4], sig[4]
	g.SlopeGeo = cfg.SecondaryHeightMM / (F * R)
	g.HeightMM = math.Abs(g.Slope) * F * R
	if ratio := math.Abs(g.Slope) / g.SlopeGeo; math.Abs(g.Slope) > 3*g.SlopeErr && ratio > 0.5 && ratio < 2 {
		g.IntraHigh = 1
		if g.Slope < 0 {
			g.IntraHigh = -1
		}
	}
	// Physical frame: flip if the higher position is outside focus. Evaluate
	// at the star on the primary's axis (where coma says it lands).
	sgn := 1.0
	if g.IntraHigh < 0 {
		sgn = -1
	}
	at := func(c0 float64, h float64) float64 { return sgn * (c0 + g.Slope*h) * R }
	g.ShadowX, g.ShadowY = at(x[0], res.AxisXmm), at(x[1], res.AxisYmm)
	g.ErrMM = math.Max(sig[0], sig[1]) * R
	if g.Hub {
		g.HubX, g.HubY = at(x[2], res.AxisXmm), at(x[3], res.AxisYmm)
		g.ErrMM = math.Max(g.ErrMM, math.Max(sig[2], sig[3])*R)
		g.TubeMrad = math.Hypot(g.HubX, g.HubY) / cfg.SecondaryHeightMM * 1000
	}
	switch frac := g.HeightMM / cfg.SecondaryHeightMM; {
	case g.ErrMM > maxPupilErrMM:
		g.Rough, g.Why = true, fmt.Sprintf("positions are only good to ±%.1f mm", g.ErrMM)
	case frac < minHeightFrac || frac > maxHeightFrac:
		g.Rough, g.Why = true, fmt.Sprintf("the shadow drift puts the secondary %.0f mm above the primary, which can't be right", g.HeightMM)
	}
	if g.Rough {
		res.Warnings = append(res.Warnings, "The secondary and spider positions are too noisy to trust: "+g.Why+".")
	}
}

// startPupilRegion is the start for a region's pupil fit: the global pupil
// geometry with the region's coma.
func startPupilRegion(pupil model.Shared, coma model.Shared) model.Shared {
	sh := pupil
	sh.Cx, sh.Cy = coma.Cx, coma.Cy
	return sh
}

// lstsq solves the linear least-squares problem rows·x ≈ ys by the normal
// equations, returning x and 1σ errors scaled by the residual scatter.
func lstsq(rows [][]float64, ys []float64) (x, sigma []float64, ok bool) {
	if len(rows) == 0 {
		return nil, nil, false
	}
	p := len(rows[0])
	if len(rows) < p {
		return nil, nil, false
	}
	a := make([][]float64, p)
	for i := range a {
		a[i] = make([]float64, p)
	}
	b := make([]float64, p)
	for r, row := range rows {
		for i := range p {
			for j := range p {
				a[i][j] += row[i] * row[j]
			}
			b[i] += row[i] * ys[r]
		}
	}
	inv, ok := invert(a)
	if !ok {
		return nil, nil, false
	}
	x = make([]float64, p)
	for i := range p {
		for j := range p {
			x[i] += inv[i][j] * b[j]
		}
	}
	var rss float64
	for r, row := range rows {
		var v float64
		for i := range p {
			v += row[i] * x[i]
		}
		rss += (ys[r] - v) * (ys[r] - v)
	}
	s2 := 0.0
	if dof := len(rows) - p; dof > 0 {
		s2 = rss / float64(dof)
	}
	sigma = make([]float64, p)
	for i := range p {
		sigma[i] = math.Sqrt(math.Max(0, inv[i][i]*s2))
	}
	return x, sigma, true
}

// invert inverts a small matrix by Gauss–Jordan elimination with partial
// pivoting.
func invert(a [][]float64) ([][]float64, bool) {
	n := len(a)
	m := make([][]float64, n)
	for i := range m {
		m[i] = make([]float64, 2*n)
		copy(m[i], a[i])
		m[i][n+i] = 1
	}
	scale := 0.0
	for i := range n {
		scale = math.Max(scale, math.Abs(a[i][i]))
	}
	for c := range n {
		piv := c
		for r := c + 1; r < n; r++ {
			if math.Abs(m[r][c]) > math.Abs(m[piv][c]) {
				piv = r
			}
		}
		if math.Abs(m[piv][c]) <= 1e-12*math.Max(scale, 1e-300) {
			return nil, false
		}
		m[c], m[piv] = m[piv], m[c]
		f := m[c][c]
		for j := range 2 * n {
			m[c][j] /= f
		}
		for r := range n {
			if r == c || m[r][c] == 0 {
				continue
			}
			g := m[r][c]
			for j := range 2 * n {
				m[r][j] -= g * m[c][j]
			}
		}
	}
	out := make([][]float64, n)
	for i := range out {
		out[i] = m[i][n:]
	}
	return out, true
}
