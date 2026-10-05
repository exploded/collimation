package analysis

import (
	"math"
	"math/rand/v2"
	"testing"

	"github.com/exploded/collimation/internal/model"
)

// synthSide renders a noisy stack from known parameters.
func synthSide(sh model.Shared, sd model.Side, n int, noise float64, rng *rand.Rand) []float64 {
	p := model.NewPupil(240, 590)
	img := make([]float64, n*n)
	p.Render(img, n, sh, sd)
	var peak float64
	for _, v := range img {
		peak = math.Max(peak, v)
	}
	for i := range img {
		img[i] += rng.NormFloat64() * noise * peak
	}
	return img
}

// Round trip: render set-2-like donuts on both sides of focus, fit them, and
// recover the coma.
func TestFitRoundTrip(t *testing.T) {
	if testing.Short() {
		t.Skip("slow")
	}
	rng := rand.New(rand.NewPCG(1, 2))
	truth := model.Shared{Cx: -3.65, Cy: 3.25, Sx: 0.07, Sy: -0.02, Gx: 0.1, Gy: -0.05,
		Sigma: 3.5, Eps: 0.45, SA: 3.8, A1: 0.5, A2: -0.3}
	n := 187
	lo := model.Side{K: -55, X0: 0.4, Y0: -0.3, Amp: 1}
	hi := model.Side{K: 60, X0: -0.2, Y0: 0.5, Amp: 1}
	sides := []SideData{
		{Pos: 3366, Sign: -1, N: n, Stack: synthSide(truth, lo, n, 0.01, rng)},
		{Pos: 4375, Sign: 1, N: n, Stack: synthSide(truth, hi, n, 0.01, rng)},
	}
	f := FitDonuts(sides, nil, FitAll, DefaultStarts(sides))
	got := f.Shared
	check := func(name string, g, w, tol float64) {
		if math.Abs(g-w) > tol {
			t.Errorf("%s = %.3f, want %.3f ± %.3f", name, g, w, tol)
		}
	}
	check("Cx", got.Cx, truth.Cx, 0.1)
	check("Cy", got.Cy, truth.Cy, 0.1)
	check("Eps", got.Eps, truth.Eps, 0.01)
	check("Sigma", got.Sigma, truth.Sigma, 0.15)
	check("SA", got.SA, truth.SA, 0.3)
	check("K lo", f.Sides[0].K, lo.K, 0.5)
	check("K hi", f.Sides[1].K, hi.K, 0.5)
	t.Logf("fit: c=(%.3f, %.3f) eps %.3f σ %.2f SA %.2f, %d iterations", got.Cx, got.Cy, got.Eps, got.Sigma, got.SA, f.Iter)
}

// Spider vanes are not in the fitted model. Check they do not bias coma.
func TestFitIgnoresVanes(t *testing.T) {
	if testing.Short() {
		t.Skip("slow")
	}
	rng := rand.New(rand.NewPCG(3, 4))
	truth := model.Shared{Cx: -2.8, Cy: 2.7, Sx: 0.09, Sy: -0.03, Gx: 0.12, Gy: -0.05,
		Sigma: 3.3, Eps: 0.37, SA: -3, VaneW: 0.02, VaneAng: 0.03}
	n := 187
	lo := model.Side{K: -55, Amp: 1}
	hi := model.Side{K: 60, Amp: 1}
	sides := []SideData{
		{Pos: 3366, Sign: -1, N: n, Stack: synthSide(truth, lo, n, 0.01, rng)},
		{Pos: 4375, Sign: 1, N: n, Stack: synthSide(truth, hi, n, 0.01, rng)},
	}
	f := FitDonuts(sides, nil, FitAll, DefaultStarts(sides))
	if d := math.Hypot(f.Shared.Cx-truth.Cx, f.Shared.Cy-truth.Cy); d > 0.15 {
		t.Errorf("coma (%.3f, %.3f) is %.2f px from truth (%.2f, %.2f)", f.Shared.Cx, f.Shared.Cy, d, truth.Cx, truth.Cy)
	}
	t.Logf("with vanes: c=(%.3f, %.3f) eps %.3f", f.Shared.Cx, f.Shared.Cy, f.Shared.Eps)
}
