// Package donut cuts out defocused stars, rejects contaminated ones and
// stacks them.
package donut

import (
	"math"
	"slices"
)

// Cutout is a star image centred on its flux centroid.
type Cutout struct {
	X, Y float64   // flux centroid in frame pixels
	Flux float64   // background-subtracted flux inside the donut
	N    int       // side length (2R+1)
	Pix  []float32 // N×N, local background removed, not normalised
}

// Options control extraction.
type Options struct {
	K     float64 // donut radius estimate (px)
	R     int     // cutout half-size (px)
	Noise float64 // per-pixel noise of the background-subtracted frame
	Sat   float64 // pixels at or above this level mark a saturated star
}

// HalfSize picks a cutout half-size for donut radius k.
func HalfSize(k float64) int { return int(math.Ceil(1.5*math.Abs(k) + 12)) }

// Reject reasons.
const (
	RejEdge         = "edge"
	RejSaturated    = "saturated"
	RejFaint        = "faint"
	RejContaminated = "contaminated"
)

// Extract cuts out the star near (x, y) from a background-subtracted frame.
// It returns nil and a reason if the star is unusable.
func Extract(pix []float32, w, h int, x, y float64, o Options) (*Cutout, string) {
	k := math.Abs(o.K)
	rc := 1.25*k + 6
	// Iterated flux centroid.
	for range 4 {
		var s, sx, sy float64
		x0, x1 := int(math.Floor(x-rc)), int(math.Ceil(x+rc))
		y0, y1 := int(math.Floor(y-rc)), int(math.Ceil(y+rc))
		if x0 < 0 || y0 < 0 || x1 >= w || y1 >= h {
			return nil, RejEdge
		}
		for yy := y0; yy <= y1; yy++ {
			for xx := x0; xx <= x1; xx++ {
				dx, dy := float64(xx)-x, float64(yy)-y
				if dx*dx+dy*dy > rc*rc {
					continue
				}
				v := float64(pix[yy*w+xx])
				s += v
				sx += v * float64(xx)
				sy += v * float64(yy)
			}
		}
		if s <= 0 {
			return nil, RejFaint
		}
		x, y = sx/s, sy/s
	}
	R := o.R
	n := 2*R + 1
	ix, iy := int(math.Floor(x)), int(math.Floor(y))
	fx, fy := x-float64(ix), y-float64(iy)
	if ix-R-2 < 0 || iy-R-2 < 0 || ix+R+3 >= w || iy+R+3 >= h {
		return nil, RejEdge
	}
	wx, wy := cubicWeights(fx), cubicWeights(fy)
	out := make([]float32, n*n)
	var peak float32
	for j := range n {
		sy := iy + j - R
		for i := range n {
			sx := ix + i - R
			var v float32
			for b := range 4 {
				row := pix[(sy+b-1)*w:]
				var rv float32
				for a := range 4 {
					rv += wx[a] * row[sx+a-1]
				}
				v += wy[b] * rv
			}
			out[j*n+i] = v
			// Saturation is judged on the original pixels.
			if p := pix[sy*w+sx]; p > peak {
				peak = p
			}
		}
	}
	if o.Sat > 0 && float64(peak) >= o.Sat {
		return nil, RejSaturated
	}

	// Local background from the corners outside the donut.
	rOut := 1.3*k + 6
	var bg []float32
	for j := range n {
		for i := range n {
			dx, dy := float64(i-R), float64(j-R)
			if dx*dx+dy*dy > rOut*rOut {
				bg = append(bg, out[j*n+i])
			}
		}
	}
	if len(bg) > 30 {
		slices.Sort(bg)
		b := bg[len(bg)/2]
		for i := range out {
			out[i] -= b
		}
	}

	// Ring level and flux.
	var ring, nRing, flux float64
	rIn := 1.25*k + 6
	for j := range n {
		for i := range n {
			dx, dy := float64(i-R), float64(j-R)
			r := math.Hypot(dx, dy)
			v := float64(out[j*n+i])
			if r > 0.65*k && r < 0.9*k {
				ring += v
				nRing++
			}
			if r < rIn {
				flux += v
			}
		}
	}
	if nRing == 0 || flux <= 0 {
		return nil, RejFaint
	}
	ring /= nRing
	// Large donuts spread the light thinly, so judge brightness on the whole
	// ring rather than per pixel: the ring's summed signal-to-noise.
	if o.Noise > 0 && (ring <= 0 || ring*math.Sqrt(nRing)/o.Noise < 40) {
		return nil, RejFaint
	}

	// Contamination: box-averaged light well outside the donut. The box grows
	// with the donut so that noise alone cannot trip the test.
	hb := max(1, int(k/16))
	box := float64((2*hb + 1) * (2*hb + 1))
	limit := 0.5*ring + 4*o.Noise/math.Sqrt(box)
	rCont := 1.25*k + 8
	for j := hb; j < n-hb; j++ {
		for i := hb; i < n-hb; i++ {
			dx, dy := float64(i-R), float64(j-R)
			if dx*dx+dy*dy <= rCont*rCont {
				continue
			}
			var s float32
			for b := -hb; b <= hb; b++ {
				for a := -hb; a <= hb; a++ {
					s += out[(j+b)*n+i+a]
				}
			}
			if float64(s)/box > limit {
				return nil, RejContaminated
			}
		}
	}
	return &Cutout{X: x, Y: y, Flux: flux, N: n, Pix: out}, ""
}

// cubicWeights returns Keys (a = -0.5) cubic convolution weights for taps at
// offsets -1, 0, 1, 2 from the integer position, for fractional shift t.
func cubicWeights(t float64) [4]float32 {
	const a = -0.5
	f := func(x float64) float64 {
		x = math.Abs(x)
		switch {
		case x <= 1:
			return (a+2)*x*x*x - (a+3)*x*x + 1
		case x < 2:
			return a*x*x*x - 5*a*x*x + 8*a*x - 4*a
		}
		return 0
	}
	return [4]float32{float32(f(1 + t)), float32(f(t)), float32(f(1 - t)), float32(f(2 - t))}
}

// Stack returns the flux-weighted mean of cutouts (Σ pix / Σ flux), so the
// stack integrates to about 1. All cutouts must share the same size.
func Stack(cs []*Cutout) []float64 {
	if len(cs) == 0 {
		return nil
	}
	n := cs[0].N
	out := make([]float64, n*n)
	var f float64
	for _, c := range cs {
		for i, v := range c.Pix {
			out[i] += float64(v)
		}
		f += c.Flux
	}
	for i := range out {
		out[i] /= f
	}
	return out
}

// RadialProfile returns the azimuthal mean of an n×n image about its centre
// in 1-px bins.
func RadialProfile(img []float64, n int) []float64 {
	c := float64(n-1) / 2
	nb := n / 2
	sum := make([]float64, nb+1)
	cnt := make([]float64, nb+1)
	for y := range n {
		for x := range n {
			r := int(math.Round(math.Hypot(float64(x)-c, float64(y)-c)))
			if r <= nb {
				sum[r] += img[y*n+x]
				cnt[r]++
			}
		}
	}
	for i := range sum {
		if cnt[i] > 0 {
			sum[i] /= cnt[i]
		}
	}
	return sum
}

// EdgeRadius returns the radius beyond the profile's maximum where it first
// falls to half the maximum, interpolated. It returns 0 if not found.
func EdgeRadius(prof []float64) float64 {
	im := 0
	for i, v := range prof {
		if v > prof[im] {
			im = i
		}
	}
	half := prof[im] / 2
	for i := im + 1; i < len(prof); i++ {
		if prof[i] <= half {
			a, b := prof[i-1], prof[i]
			return float64(i-1) + (a-half)/(a-b)
		}
	}
	return 0
}

// Ellipticity returns (λ1−λ2)/(λ1+λ2) of the second moments of an n×n image
// about its centroid, and the major-axis angle in radians. Trailed (bumped)
// frames show up as high ellipticity.
func Ellipticity(img []float64, n int) (float64, float64) {
	var s, sx, sy float64
	for y := range n {
		for x := range n {
			v := math.Max(img[y*n+x], 0)
			s += v
			sx += v * float64(x)
			sy += v * float64(y)
		}
	}
	if s == 0 {
		return 0, 0
	}
	mx, my := sx/s, sy/s
	var xx, yy, xy float64
	for y := range n {
		for x := range n {
			v := math.Max(img[y*n+x], 0)
			dx, dy := float64(x)-mx, float64(y)-my
			xx += v * dx * dx
			yy += v * dy * dy
			xy += v * dx * dy
		}
	}
	xx, yy, xy = xx/s, yy/s, xy/s
	tr := xx + yy
	d := math.Sqrt((xx-yy)*(xx-yy) + 4*xy*xy)
	if tr == 0 {
		return 0, 0
	}
	return d / tr, 0.5 * math.Atan2(2*xy, xx-yy)
}
