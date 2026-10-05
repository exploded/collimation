package fit

import (
	"math"
	"testing"
)

func TestSolveExponential(t *testing.T) {
	// y = a·exp(-b·x) + c in two blocks; c only affects block 1.
	xs := []float64{0, 0.5, 1, 1.5, 2, 2.5, 3, 3.5, 4, 4.5}
	a, b, c := 2.5, 0.7, 0.3
	y := func(x float64, blk int) float64 {
		v := a * math.Exp(-b*x)
		if blk == 1 {
			v += c
		}
		return v
	}
	pr := Problem{
		Blocks: []int{len(xs), len(xs)},
		Eval: func(p []float64, blk int, r []float64) {
			for i, x := range xs {
				m := p[0] * math.Exp(-p[1]*x)
				if blk == 1 {
					m += p[2]
				}
				r[i] = m - y(x, blk)
			}
		},
		Affects: [][]int{nil, nil, {1}},
		Lower:   []float64{0, 0, -1},
		Upper:   []float64{10, 5, 1},
		Step:    []float64{1e-6, 1e-6, 1e-6},
	}
	res, err := Solve(pr, []float64{1, 0.2, 0}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	want := []float64{a, b, c}
	for i := range want {
		if math.Abs(res.P[i]-want[i]) > 1e-5 {
			t.Errorf("p[%d] = %v, want %v (status %s)", i, res.P[i], want[i], res.Status)
		}
	}
}

func TestSolveRespectsBounds(t *testing.T) {
	pr := Problem{
		Blocks: []int{1},
		Eval:   func(p []float64, _ int, r []float64) { r[0] = p[0] - 5 },
		Lower:  []float64{0},
		Upper:  []float64{2},
		Step:   []float64{1e-6},
	}
	res, _ := Solve(pr, []float64{1}, Options{})
	if res.P[0] != 2 {
		t.Errorf("p = %v, want 2 (upper bound)", res.P[0])
	}
}
