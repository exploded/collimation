package model

import (
	"math"
	"testing"
)

// Sanity check from the Python prototype: with c = (2, 0), K = 16 and a 0.33 obstruction,
// the flux centroid moves by about +1.1·c in x.
func TestComaCentroidShift(t *testing.T) {
	p := NewPupil(160, 400)
	n := 61
	img := make([]float64, n*n)
	sh := Shared{Cx: 2, Sigma: 1.5, Eps: 0.33}
	p.Render(img, n, sh, Side{K: 16, Amp: 1})
	cx, cy := Centroid(img, n)
	want := 2 * (1 + 0.33*0.33) // c·(1+ε²) for an annular pupil
	if math.Abs(cx-want) > 0.05 || math.Abs(cy) > 0.02 {
		t.Errorf("centroid = (%.3f, %.3f), want (%.3f, 0)", cx, cy, want)
	}
	// Coma does not depend on the side of focus.
	p.Render(img, n, sh, Side{K: -16, Amp: 1})
	cx2, _ := Centroid(img, n)
	if math.Abs(cx2-cx) > 0.02 {
		t.Errorf("centroid with K<0 = %.3f, want %.3f", cx2, cx)
	}
}

func TestRenderNormalised(t *testing.T) {
	p := NewPupil(120, 300)
	n := 51
	img := make([]float64, n*n)
	p.Render(img, n, Shared{Sigma: 2, Eps: 0.4}, Side{K: 15, Amp: 3, Bg: 0})
	var s float64
	for _, v := range img {
		s += v
	}
	if math.Abs(s-3) > 1e-9 {
		t.Errorf("sum = %v, want 3", s)
	}
	// Centre is dark (obstructed), the ring is bright.
	c := (n - 1) / 2
	if img[c*n+c] > 0.2*img[c*n+c+12] {
		t.Errorf("centre %.4g not dark relative to ring %.4g", img[c*n+c], img[c*n+c+12])
	}
}
