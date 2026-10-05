package donut_test

import (
	"math"
	"math/rand/v2"
	"testing"

	"github.com/exploded/collimation/internal/detect"
	"github.com/exploded/collimation/internal/donut"
	"github.com/exploded/collimation/internal/model"
)

// A synthetic field: donuts at known positions on a sloped sky with noise.
// Detection, isolation and extraction must find the isolated ones and centre
// them to a fraction of a pixel; the crowded pair must be rejected.
func TestDetectAndExtract(t *testing.T) {
	const w, h, k = 1200, 900, 20.0
	rng := rand.New(rand.NewPCG(5, 6))
	pix := make([]float32, w*h)
	for y := range h {
		for x := range w {
			pix[y*w+x] = float32(500 + 0.05*float64(x) + 0.03*float64(y) + rng.NormFloat64()*8)
		}
	}
	p := model.NewPupil(120, 300)
	n := 81
	img := make([]float64, n*n)
	type star struct{ x, y, flux float64 }
	stars := []star{{200.3, 200.6, 4e5}, {600.7, 450.2, 3e5}, {950.5, 700.4, 5e5}, {300.2, 700.8, 2e5},
		// A close pair: both should be rejected as not isolated.
		{850.0, 200.0, 4e5}, {880.0, 215.0, 3e5}}
	for _, s := range stars {
		ix, iy := int(math.Round(s.x)), int(math.Round(s.y))
		// Render puts the pupil centre at the cutout centre; shift by the
		// sub-pixel remainder using the model's own offset.
		p.Render(img, n, model.Shared{Sigma: 1.5, Eps: 0.4}, model.Side{K: k, Amp: s.flux, X0: s.x - float64(ix), Y0: s.y - float64(iy)})
		for j := range n {
			for i := range n {
				pix[(iy+j-n/2)*w+ix+i-n/2] += float32(img[j*n+i])
			}
		}
	}

	detect.SubtractBackground(pix, w, h, 128)
	noise := detect.RobustSigma(pix)
	sm := detect.Smooth(pix, w, h, k/3)
	sn := detect.RobustSigma(sm)
	all := detect.FindPeaks(sm, w, h, 3*sn, int(1.6*k), 1)
	var cands []detect.Peak
	for _, pk := range all {
		if pk.Value > 10*sn {
			cands = append(cands, pk)
		}
	}
	iso := detect.Isolated(cands, all, 2.2*k, 0.04, w, h, float64(donut.HalfSize(k)+4))
	// A merged close pair may survive peak finding; extraction must then
	// reject it as contaminated.
	var got []*donut.Cutout
	for _, pk := range iso {
		c, why := donut.Extract(pix, w, h, pk.X, pk.Y, donut.Options{K: k, R: donut.HalfSize(k), Noise: noise})
		if c == nil {
			if pk.X < 800 {
				t.Errorf("isolated star near (%.0f, %.0f) rejected: %s", pk.X, pk.Y, why)
			}
			continue
		}
		got = append(got, c)
	}
	if len(got) != 4 {
		t.Fatalf("extracted %d stars, want the 4 isolated ones", len(got))
	}
	for _, c := range got {
		best := math.Inf(1)
		for _, s := range stars[:4] {
			best = math.Min(best, math.Hypot(c.X-s.x, c.Y-s.y))
		}
		if best > 0.3 {
			t.Errorf("centroid (%.2f, %.2f) is %.2f px from the nearest true star", c.X, c.Y, best)
		}
		prof := donut.RadialProfile(donut.Stack([]*donut.Cutout{c}), c.N)
		if e := donut.EdgeRadius(prof); math.Abs(e-k) > 1.5 {
			t.Errorf("edge radius %.2f, want about %.0f", e, k)
		}
	}
}
