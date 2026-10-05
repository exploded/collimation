package analysis

import (
	"fmt"
	"math"

	"github.com/exploded/collimation/internal/donut"
	"github.com/exploded/collimation/internal/model"
)

// Point is a position in frame pixels.
type Point [2]float64

// FrameResult is the quick analysis of a single defocused frame, used by
// live mode.
type FrameResult struct {
	Info        FrameInfo
	Stars       int
	K           float64
	ComaX       float64
	ComaY       float64
	AxisXmm     float64
	AxisYmm     float64
	DecentreMM  float64
	Ellipticity float64 // of the stack; a bumped (trailed) frame reads high
	Positions   []Point
	Fit         FitResult
}

// AnalyzeFrame measures coma from one frame. If shared is not nil (from the
// latest full measurement), only coma, K, centre and flux are fitted, which is
// fast and stable; otherwise all shared parameters are fitted on this side.
func AnalyzeFrame(path string, focus int, shared *model.Shared, cfg Config) (*FrameResult, error) {
	info, err := ReadInfo(path, cfg)
	if err != nil {
		return nil, err
	}
	steps := math.Abs(float64(info.FocPos - focus))
	if focus == 0 || steps < 1 {
		steps = 500
	}
	fr, err := LoadFrame(path, ExpectedK(steps, info.NRatio, cfg), cfg)
	if err != nil {
		return nil, err
	}
	if len(fr.Stars) < 5 {
		return nil, fmt.Errorf("only %d usable stars in %s", len(fr.Stars), path)
	}
	cs := cropToCommon(fr.Stars)
	sign := 1.0
	if focus != 0 && info.FocPos < focus {
		sign = -1
	}
	d := SideData{Pos: info.FocPos, Sign: sign, Stack: donut.Stack(cs), N: cs[0].N, Stars: len(cs), Frames: 1}
	var f FitResult
	if shared != nil {
		sh := *shared
		f = FitDonuts([]SideData{d}, &sh, FitComaOnly, nil)
	} else {
		f = FitDonuts([]SideData{d}, nil, FitAll, DefaultStarts([]SideData{d}))
	}
	r := &FrameResult{Info: info, Stars: len(cs), K: f.Sides[0].K, ComaX: f.Shared.Cx, ComaY: f.Shared.Cy,
		Positions: positions(fr.Stars), Fit: f}
	r.AxisXmm, r.AxisYmm = ComaToAxisMM(r.ComaX, r.ComaY, cfg)
	r.DecentreMM = math.Hypot(r.AxisXmm, r.AxisYmm)
	r.Ellipticity, _ = donut.Ellipticity(d.Stack, d.N)
	return r, nil
}

// MatchShift finds the translation that maps star positions a onto b (b ≈ a +
// shift). It votes over all pairs within maxShift in 4-px bins and refines
// with the mean of the matched pairs. ok is false if fewer than 4 stars match.
func MatchShift(a, b []Point, maxShift float64) (dx, dy float64, n int, ok bool) {
	const bin = 4.0
	a, b = head(a, 150), head(b, 150)
	votes := map[[2]int]int{}
	for _, p := range a {
		for _, q := range b {
			ddx, ddy := q[0]-p[0], q[1]-p[1]
			if math.Abs(ddx) > maxShift || math.Abs(ddy) > maxShift {
				continue
			}
			k := [2]int{int(math.Floor(ddx / bin)), int(math.Floor(ddy / bin))}
			votes[k]++
		}
	}
	// Sum each bin with its neighbours so a shift on a bin edge still wins.
	var best [2]int
	bestN := 0
	for k := range votes {
		s := 0
		for i := -1; i <= 1; i++ {
			for j := -1; j <= 1; j++ {
				s += votes[[2]int{k[0] + i, k[1] + j}]
			}
		}
		if s > bestN {
			best, bestN = k, s
		}
	}
	if bestN < 4 {
		return 0, 0, 0, false
	}
	cx, cy := (float64(best[0])+0.5)*bin, (float64(best[1])+0.5)*bin
	var sx, sy float64
	for _, p := range a {
		for _, q := range b {
			ddx, ddy := q[0]-p[0], q[1]-p[1]
			if math.Abs(ddx-cx) <= 2*bin && math.Abs(ddy-cy) <= 2*bin {
				sx += ddx
				sy += ddy
				n++
			}
		}
	}
	if n < 4 {
		return 0, 0, n, false
	}
	return sx / float64(n), sy / float64(n), n, true
}

func head(p []Point, n int) []Point {
	if len(p) > n {
		return p[:n]
	}
	return p
}
