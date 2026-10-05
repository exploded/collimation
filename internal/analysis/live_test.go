package analysis

import (
	"math"
	"math/rand/v2"
	"testing"
)

func TestComaPerShift(t *testing.T) {
	// 5 Oct 2026 screw turns: star shift / coma change was 494 to 543.
	if r := 1 / ComaPerShift(DefaultConfig()); r < 490 || r > 545 {
		t.Errorf("star shift per px of coma %.0f, want about 511", r)
	}
}

func TestMatchShift(t *testing.T) {
	rng := rand.New(rand.NewPCG(7, 8))
	var a, b []Point
	for range 80 {
		p := Point{rng.Float64() * 6000, rng.Float64() * 4000}
		a = append(a, p)
		// Some stars leave the field, some arrive; positions jitter.
		if rng.Float64() < 0.8 {
			b = append(b, Point{p[0] + 412.3 + rng.NormFloat64()*0.3, p[1] - 187.6 + rng.NormFloat64()*0.3})
		}
	}
	for range 15 {
		b = append(b, Point{rng.Float64() * 6000, rng.Float64() * 4000})
	}
	dx, dy, n, ok := MatchShift(a, b, 1500)
	if !ok || math.Abs(dx-412.3) > 0.3 || math.Abs(dy+187.6) > 0.3 {
		t.Errorf("shift (%.2f, %.2f) from %d pairs, ok=%v; want (412.3, -187.6)", dx, dy, n, ok)
	}
}
