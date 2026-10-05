// Package model renders defocused star images (donuts) by geometric ray
// mapping from a uniformly sampled annular pupil. It is a Go port of the
// model from an earlier Python prototype.
package model

import (
	"math"
	"sync"
)

// Shared holds the physical parameters common to both sides of focus.
type Shared struct {
	Cx, Cy float64 // sagittal coma radius (px); +c flares toward +x/+y
	Sx, Sy float64 // secondary shadow offset (pupil units)
	Gx, Gy float64 // illumination gradient across the pupil
	Sigma  float64 // seeing blur (px)
	Eps    float64 // central obstruction ratio
	SA     float64 // transverse spherical aberration at the pupil edge (px)
	A1, A2 float64 // astigmatism (px at the pupil edge)

	// Spider: four vanes through the shadow centre, VaneW wide (pupil units)
	// at VaneAng and VaneAng+90° (radians). VaneW = 0 disables them.
	VaneW, VaneAng float64
}

// Side holds the per-side parameters.
type Side struct {
	K      float64 // defocus radius (px), sign gives the side of focus
	X0, Y0 float64 // centre offset (px)
	Amp    float64 // total flux
	Bg     float64 // constant background
}

// Pupil is a set of samples uniform in area over the unit disc.
type Pupil struct {
	X, Y, Rho2 []float64
}

// NewPupil samples nr radial rings (ρ = √u) by nt azimuths.
func NewPupil(nr, nt int) *Pupil {
	p := &Pupil{
		X:    make([]float64, 0, nr*nt),
		Y:    make([]float64, 0, nr*nt),
		Rho2: make([]float64, 0, nr*nt),
	}
	for i := range nr {
		u := (float64(i) + 0.5) / float64(nr)
		rho := math.Sqrt(u)
		// Stagger alternate rings to avoid radial spokes.
		off := 0.5 * float64(i%2)
		for j := range nt {
			th := 2 * math.Pi * (float64(j) + off) / float64(nt)
			p.X = append(p.X, rho*math.Cos(th))
			p.Y = append(p.Y, rho*math.Sin(th))
			p.Rho2 = append(p.Rho2, u)
		}
	}
	return p
}

// PupilFor picks a sampling density suited to a defocus radius of k px.
func PupilFor(k float64) *Pupil {
	nr := int(math.Max(100, math.Min(240, 3.7*math.Abs(k))))
	return NewPupil(nr, int(float64(nr)*2.45))
}

// edgeWidth softens the obstruction edge (pupil units) so the fit sees a
// smooth cost surface in Eps and the shadow offset.
const edgeWidth = 0.015

var scratchPool = sync.Pool{New: func() any { return new(scratch) }}

type scratch struct{ a, b []float64 }

func (s *scratch) size(n int) {
	if cap(s.a) < n {
		s.a = make([]float64, n)
		s.b = make([]float64, n)
	}
	s.a, s.b = s.a[:n], s.b[:n]
}

// Render draws the model into dst, an n×n image whose centre pixel (n-1)/2
// corresponds to the star centroid. dst is overwritten.
func (p *Pupil) Render(dst []float64, n int, sh Shared, sd Side) {
	for i := range dst {
		dst[i] = 0
	}
	c := float64(n-1) / 2
	k := sd.K
	eps := sh.Eps
	vs, vc := math.Sincos(sh.VaneAng)
	for i := range p.X {
		x, y := p.X[i], p.Y[i]
		// Obstruction with a soft edge.
		d := math.Hypot(x-sh.Sx, y-sh.Sy)
		var keep float64
		switch t := (d-eps)/edgeWidth + 0.5; {
		case t <= 0:
			continue
		case t >= 1:
			keep = 1
		default:
			keep = t * t * (3 - 2*t)
		}
		if sh.VaneW > 0 {
			qx, qy := x-sh.Sx, y-sh.Sy
			if math.Abs(-qx*vs+qy*vc) < sh.VaneW/2 || math.Abs(qx*vc+qy*vs) < sh.VaneW/2 {
				continue
			}
		}
		wgt := (1 + sh.Gx*x + sh.Gy*y) * keep
		if wgt <= 0 {
			continue
		}
		r2 := p.Rho2[i]
		ex := sh.Cx*(3*x*x+y*y) + sh.Cy*2*x*y + sh.A1*x + sh.A2*y + sh.SA*r2*x
		ey := sh.Cx*2*x*y + sh.Cy*(x*x+3*y*y) + sh.A2*x - sh.A1*y + sh.SA*r2*y
		px := k*x + ex + sd.X0 + c
		py := k*y + ey + sd.Y0 + c
		splat(dst, n, px, py, wgt)
	}
	sc := scratchPool.Get().(*scratch)
	sc.size(n * n)
	blur(dst, sc.a, n, math.Max(sh.Sigma, 0.3))
	scratchPool.Put(sc)
	var sum float64
	for _, v := range dst {
		sum += v
	}
	if sum <= 0 {
		for i := range dst {
			dst[i] = sd.Bg
		}
		return
	}
	f := sd.Amp / sum
	for i := range dst {
		dst[i] = dst[i]*f + sd.Bg
	}
}

func splat(dst []float64, n int, px, py, w float64) {
	x0 := int(math.Floor(px))
	y0 := int(math.Floor(py))
	if x0 < 0 || y0 < 0 || x0 >= n-1 || y0 >= n-1 {
		return
	}
	tx, ty := px-float64(x0), py-float64(y0)
	i := y0*n + x0
	dst[i] += w * (1 - tx) * (1 - ty)
	dst[i+1] += w * tx * (1 - ty)
	dst[i+n] += w * (1 - tx) * ty
	dst[i+n+1] += w * tx * ty
}

// blur applies a separable Gaussian in place using tmp as scratch.
func blur(img, tmp []float64, n int, sigma float64) {
	r := max(1, int(math.Ceil(3*sigma)))
	k := make([]float64, 2*r+1)
	var s float64
	for i := -r; i <= r; i++ {
		k[i+r] = math.Exp(-0.5 * float64(i*i) / (sigma * sigma))
		s += k[i+r]
	}
	for i := range k {
		k[i] /= s
	}
	for y := range n {
		row := img[y*n : (y+1)*n]
		for x := range n {
			var v float64
			lo, hi := max(0, x-r), min(n-1, x+r)
			for xx := lo; xx <= hi; xx++ {
				v += k[xx-x+r] * row[xx]
			}
			tmp[y*n+x] = v
		}
	}
	for x := range n {
		for y := range n {
			var v float64
			lo, hi := max(0, y-r), min(n-1, y+r)
			for yy := lo; yy <= hi; yy++ {
				v += k[yy-y+r] * tmp[yy*n+x]
			}
			img[y*n+x] = v
		}
	}
}

// Centroid returns the flux centroid of an n×n image relative to its centre.
func Centroid(img []float64, n int) (float64, float64) {
	c := float64(n-1) / 2
	var s, sx, sy float64
	for y := range n {
		for x := range n {
			v := img[y*n+x]
			s += v
			sx += v * (float64(x) - c)
			sy += v * (float64(y) - c)
		}
	}
	if s == 0 {
		return 0, 0
	}
	return sx / s, sy / s
}
