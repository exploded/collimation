package collim

import (
	"math"
	"testing"
)

func cal() Calibration {
	// Three screws 120° apart; ⅛ turn moves coma about 1 px.
	c := Calibration{}
	for i := range 3 {
		a := float64(i) * 2 * math.Pi / 3
		c.Coma[i] = Vec{math.Cos(a), math.Sin(a)}
		c.Shift[i] = Vec{-500 * math.Sin(a), 500 * math.Cos(a)}
	}
	c.CMeasured = true
	return c
}

func TestSolveNullsComa(t *testing.T) {
	c := cal()
	coma := Vec{-2.77, 2.68}
	turns := c.Solve(coma.Scale(-1), 1)
	after := coma.Add(c.Effect(turns))
	if after.Len() > 0.4 {
		t.Errorf("turns %v leave coma %v", turns, after)
	}
	n := 0
	for _, v := range turns {
		if v != 0 {
			n++
		}
	}
	if n > 2 {
		t.Errorf("used %d screws, want at most 2: %v", n, turns)
	}
	for _, v := range turns {
		if v*2 != math.Round(v*2) {
			t.Errorf("turn %v not a multiple of 1/16", v)
		}
	}
}

func TestSolveWithDerivedC(t *testing.T) {
	c := cal()
	c.CMeasured = false
	c.Coma[2] = Vec{}
	c.DeriveC()
	want := cal().Coma[2]
	if c.Coma[2].Sub(want).Len() > 1e-9 {
		t.Errorf("derived C %v, want %v", c.Coma[2], want)
	}
}

func TestGainAndMinimumNudge(t *testing.T) {
	c := cal()
	if got := c.Solve(Vec{0.1, 0}, 0.7); got == (Turns{}) {
		t.Error("tiny correction rounded to nothing; want a 1/16 nudge")
	}
}

func TestBroydenLearns(t *testing.T) {
	truth := cal()
	guess := cal()
	for i := range guess.Coma {
		guess.Coma[i] = guess.Coma[i].Scale(0.6) // calibration 40% low
	}
	for range 400 {
		for _, tr := range []Turns{{2, -1, 0}, {0, 1, -2}, {-1, 0, 2}} {
			guess.Update(tr, truth.Effect(tr), 0.5)
		}
	}
	for i := range 3 {
		if d := guess.Coma[i].Sub(truth.Coma[i]).Len(); d > 0.05 {
			t.Errorf("screw %s learned %v, want %v", Screws[i], guess.Coma[i], truth.Coma[i])
		}
	}
}

func TestBroydenLearnsShift(t *testing.T) {
	truth := cal()
	guess := cal()
	for i := range guess.Shift {
		guess.Shift[i] = guess.Shift[i].Scale(0.6) // calibration 40% low
	}
	for range 400 {
		for _, tr := range []Turns{{2, -1, 0}, {0, 1, -2}, {-1, 0, 2}} {
			guess.UpdateShift(tr, truth.ShiftEffect(tr), 0.5)
		}
	}
	for i := range 3 {
		if d := guess.Shift[i].Sub(truth.Shift[i]).Len(); d > 25 {
			t.Errorf("screw %s shift learned %v, want %v", Screws[i], guess.Shift[i], truth.Shift[i])
		}
	}
	if guess.Coma != truth.Coma {
		t.Error("UpdateShift changed the coma sensitivities")
	}
}

// The 5 Oct 2026 calibration steps: coma change and star shift for a ¼ turn.
func TestAgreesWithShift(t *testing.T) {
	const k = 1 / 510.7
	steps := []struct {
		name        string
		coma, shift Vec
		want        bool
	}{
		{"A", Vec{-0.01, -0.49}, Vec{102.9, 243.7}, true},
		{"B, lock biting", Vec{-0.07, 0.53}, Vec{-64.9, -203.2}, true},
		{"B, second turn", Vec{1.67, 1.56}, Vec{-683.1, -902.3}, true},
		{"C", Vec{-1.10, -0.09}, Vec{577.7, 26.7}, true},
		{"stars moved, coma didn't (mount)", Vec{0.05, -0.02}, Vec{600, 0}, false},
		{"coma moved the same way as the stars", Vec{1.1, 0}, Vec{577.7, 26.7}, false},
	}
	for _, s := range steps {
		if got := Agrees(s.coma, s.shift, k); got != s.want {
			t.Errorf("%s: Agrees = %v, want %v (shift implies %v)", s.name, got, s.want, FromShift(s.shift, k))
		}
	}
}

func TestFormatTurn(t *testing.T) {
	cases := map[float64]string{1: "⅛ turn", -0.5: "1/16 turn", 2: "¼ turn", 3: "⅜ turn", 8: "1 turn", 10: "1 ¼ turns", 16: "2 turns", 0: "no turn"}
	for in, want := range cases {
		if got := FormatTurn(in); got != want {
			t.Errorf("FormatTurn(%v) = %q, want %q", in, got, want)
		}
	}
}

func TestCollimationField(t *testing.T) {
	ra, dec := CollimationField(18.5, -37.8)
	if math.Abs(ra-17.2*15) > 1e-9 || dec != -37.8 {
		t.Errorf("field (%v, %v)", ra, dec)
	}
	// Altitude at hour angle 1.3 h on dec = latitude is about 75°.
	lat := -37.8 * math.Pi / 180
	ha := 1.3 * 15 * math.Pi / 180
	alt := math.Asin(math.Sin(lat)*math.Sin(lat)+math.Cos(lat)*math.Cos(lat)*math.Cos(ha)) * 180 / math.Pi
	if alt < 70 || alt > 80 {
		t.Errorf("altitude %.1f", alt)
	}
}
