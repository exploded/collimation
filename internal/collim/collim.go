// Package collim turns measured coma into screw turns, using an empirically
// calibrated sensitivity matrix.
package collim

import (
	"fmt"
	"math"
	"strings"
)

// Screws are labelled A, B and C (tape labels on the cell).
var Screws = [3]string{"A", "B", "C"}

// Vec is a 2-D vector in sensor pixels (x right, y down as N.I.N.A. shows).
type Vec struct{ X, Y float64 }

func (v Vec) Add(w Vec) Vec       { return Vec{v.X + w.X, v.Y + w.Y} }
func (v Vec) Sub(w Vec) Vec       { return Vec{v.X - w.X, v.Y - w.Y} }
func (v Vec) Scale(f float64) Vec { return Vec{v.X * f, v.Y * f} }
func (v Vec) Dot(w Vec) float64   { return v.X*w.X + v.Y*w.Y }
func (v Vec) Len() float64        { return math.Hypot(v.X, v.Y) }
func (v Vec) Cross(w Vec) float64 { return v.X*w.Y - v.Y*w.X }
func (v Vec) String() string      { return fmt.Sprintf("(%+.2f, %+.2f)", v.X, v.Y) }
func (v Vec) IsZero() bool        { return v.X == 0 && v.Y == 0 }

// Calibration is the effect of turning each screw +⅛ turn (clockwise seen
// from behind the cell): the change in coma and the shift of the stars.
type Calibration struct {
	Coma      [3]Vec // px of coma per +⅛ turn
	Shift     [3]Vec // px of star shift per +⅛ turn
	CMeasured bool   // false: C derived from A + B + C = 0
}

// Valid reports whether A and B have usable, independent effects.
func (c Calibration) Valid() bool {
	a, b := c.Coma[0], c.Coma[1]
	return a.Len() > 0.05 && b.Len() > 0.05 && math.Abs(a.Cross(b)) > 0.2*a.Len()*b.Len()
}

// DeriveC fills in screw C from A and B when it was not measured: turning all
// three screws equally pistons the mirror without tilting it, so their
// effects sum to zero.
func (c *Calibration) DeriveC() {
	if c.CMeasured {
		return
	}
	c.Coma[2] = c.Coma[0].Add(c.Coma[1]).Scale(-1)
	c.Shift[2] = c.Shift[0].Add(c.Shift[1]).Scale(-1)
}

// Turns is a turn for each screw in eighths of a turn; + is clockwise seen
// from behind the cell.
type Turns [3]float64

// Effect predicts the coma change for these turns.
func (c Calibration) Effect(t Turns) Vec {
	var v Vec
	for i := range 3 {
		v = v.Add(c.Coma[i].Scale(t[i]))
	}
	return v
}

// ShiftEffect predicts the star shift for these turns.
func (c Calibration) ShiftEffect(t Turns) Vec {
	var v Vec
	for i := range 3 {
		v = v.Add(c.Shift[i].Scale(t[i]))
	}
	return v
}

// Solve returns turns that move the coma by want, using the pair of screws
// that needs the least total turning. gain (0..1) scales the move down to
// allow for the spongy cell, and the result is rounded to 1/16 turn.
func (c Calibration) Solve(want Vec, gain float64) Turns {
	c.DeriveC()
	best := Turns{}
	bestCost := math.Inf(1)
	for _, pr := range [][2]int{{0, 1}, {0, 2}, {1, 2}} {
		i, j := pr[0], pr[1]
		a, b := c.Coma[i], c.Coma[j]
		det := a.Cross(b)
		if math.Abs(det) < 1e-9 {
			continue
		}
		ti := want.Cross(b) / det
		tj := a.Cross(want) / det
		if cost := math.Abs(ti) + math.Abs(tj); cost < bestCost {
			bestCost = cost
			best = Turns{}
			best[i], best[j] = ti, tj
		}
	}
	for i := range best {
		best[i] = roundSixteenth(best[i] * gain)
	}
	// If rounding removed everything, nudge the screw that matters most.
	if best == (Turns{}) && want.Len() > 0 && bestCost < math.Inf(1) {
		k, kv := 0, 0.0
		for i := range 3 {
			if d := c.Coma[i].Dot(want) / c.Coma[i].Len(); d > kv {
				k, kv = i, d
			}
		}
		if kv > 0 {
			best[k] = 0.5
		}
	}
	return best
}

func roundSixteenth(eighths float64) float64 { return math.Round(eighths*2) / 2 }

// AppliedFraction projects an observed change onto the predicted change:
// 1 means the mirror moved exactly as asked.
func AppliedFraction(observed, predicted Vec) float64 {
	p2 := predicted.Dot(predicted)
	if p2 == 0 {
		return math.NaN()
	}
	return observed.Dot(predicted) / p2
}

// Update refines the coma sensitivities with a damped Broyden step after an
// adjustment t produced the observed coma change. damping is 0..1.
func (c *Calibration) Update(t Turns, observed Vec, damping float64) bool {
	if !broyden(&c.Coma, t, observed, damping) {
		return false
	}
	if t[2] != 0 {
		c.CMeasured = true
	}
	return true
}

// UpdateShift refines the star-shift sensitivities the same way.
func (c *Calibration) UpdateShift(t Turns, observed Vec, damping float64) bool {
	return broyden(&c.Shift, t, observed, damping)
}

func broyden(m *[3]Vec, t Turns, observed Vec, damping float64) bool {
	tt := t[0]*t[0] + t[1]*t[1] + t[2]*t[2]
	if tt == 0 {
		return false
	}
	var pred Vec
	for i := range 3 {
		pred = pred.Add(m[i].Scale(t[i]))
	}
	resid := observed.Sub(pred)
	// Ignore wild outliers (a slipped lock, a slew, cloud).
	if resid.Len() > 3*pred.Len()+1 {
		return false
	}
	for i := range 3 {
		m[i] = m[i].Add(resid.Scale(damping * t[i] / tt))
	}
	return true
}

// FromShift is the coma change implied by a star shift: a primary tilt moves
// the axis along the shift, so the coma changes the opposite way, by k px per
// px of shift (analysis.ComaPerShift).
func FromShift(shift Vec, k float64) Vec { return shift.Scale(-k) }

// Agrees reports whether a measured coma change matches the one a star shift
// implies, allowing for the noise of a coma measurement (about 0.2 px). When
// they agree, the shift is the better measure: it is good to a pixel or two
// in hundreds, where the coma is good to a few tenths in one or two. When
// they don't, something other than a primary tilt moved the stars.
func Agrees(coma, shift Vec, k float64) bool {
	return coma.Sub(FromShift(shift, k)).Len() <= math.Max(0.4, 0.5*coma.Len())
}

// FormatTurn writes eighths of a turn as a friendly fraction, e.g. "⅛ turn",
// "3/16 turn", "1 ¼ turns". The sign is ignored.
func FormatTurn(eighths float64) string {
	sixteenths := int(math.Round(math.Abs(eighths) * 2))
	if sixteenths == 0 {
		return "no turn"
	}
	whole := sixteenths / 16
	rem := sixteenths % 16
	frac := map[int]string{1: "1/16", 2: "⅛", 3: "3/16", 4: "¼", 5: "5/16", 6: "⅜", 7: "7/16", 8: "½",
		9: "9/16", 10: "⅝", 11: "11/16", 12: "¾", 13: "13/16", 14: "⅞", 15: "15/16"}
	var parts []string
	if whole > 0 {
		parts = append(parts, fmt.Sprint(whole))
	}
	if rem > 0 {
		parts = append(parts, frac[rem])
	}
	s := strings.Join(parts, " ")
	if sixteenths > 16 || whole >= 1 && rem == 0 && whole > 1 {
		return s + " turns"
	}
	return s + " turn"
}

// Direction says which way to turn for a signed number of eighths.
func Direction(eighths float64) string {
	if eighths >= 0 {
		return "clockwise"
	}
	return "anticlockwise"
}

// Step is one instruction line.
type Step struct {
	Screw     string
	Eighths   float64
	Amount    string // "⅛ turn"
	Direction string // "clockwise"
}

// Steps lists the non-zero turns as instructions.
func (t Turns) Steps() []Step {
	var out []Step
	for i, e := range t {
		if e == 0 {
			continue
		}
		out = append(out, Step{Screw: Screws[i], Eighths: e, Amount: FormatTurn(e), Direction: Direction(e)})
	}
	return out
}

// CollimationField returns a field about 75° up, just west of the meridian
// (so no meridian flip is due), as RA and Dec in degrees: hour angle +1.3 h
// on the declination equal to the site latitude.
func CollimationField(lstHours, latDeg float64) (raDeg, decDeg float64) {
	ra := math.Mod(lstHours-1.3+24, 24) * 15
	return ra, latDeg
}
