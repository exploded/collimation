package analysis

import (
	"math"
	"sync"

	"github.com/exploded/collimation/internal/donut"
	"github.com/exploded/collimation/internal/fit"
	"github.com/exploded/collimation/internal/model"
)

// Parameter layout: 11 shared parameters, then 5 per side.
const (
	pCx = iota
	pCy
	pSx
	pSy
	pGx
	pGy
	pSigma
	pEps
	pSA
	pA1
	pA2
	nShared
)

const (
	sK = iota
	sX0
	sY0
	sAmp
	sBg
	nSide
)

// SideData is one side of focus ready to fit.
type SideData struct {
	Pos    int       // focuser position
	Sign   float64   // sign of K for this side (+1 for the higher position)
	Stack  []float64 // N×N, sums to about 1
	N      int
	Stars  int
	Frames int
}

// FitResult is the outcome of a donut fit.
type FitResult struct {
	Shared      model.Shared
	SharedSigma model.Shared
	Sides       []model.Side
	SidesSigma  []model.Side
	Cost        float64
	RMS         float64 // residual RMS relative to the stack peak
	Iter        int
	Status      string
}

func packShared(s model.Shared) []float64 {
	return []float64{s.Cx, s.Cy, s.Sx, s.Sy, s.Gx, s.Gy, s.Sigma, s.Eps, s.SA, s.A1, s.A2}
}

func unpackShared(p []float64) model.Shared {
	return model.Shared{Cx: p[pCx], Cy: p[pCy], Sx: p[pSx], Sy: p[pSy], Gx: p[pGx], Gy: p[pGy],
		Sigma: p[pSigma], Eps: p[pEps], SA: p[pSA], A1: p[pA1], A2: p[pA2]}
}

func unpackSide(p []float64, i int) model.Side {
	o := nShared + nSide*i
	return model.Side{K: p[o+sK], X0: p[o+sX0], Y0: p[o+sY0], Amp: p[o+sAmp], Bg: p[o+sBg]}
}

// FitMode selects which shared parameters are free.
type FitMode int

const (
	FitAll      FitMode = iota // every shared parameter
	FitComaOnly                // coma free; other shared parameters fixed at start
)

// initialSide estimates the starting K, amplitude and obstruction from a stack.
func initialSide(d SideData) (model.Side, float64) {
	prof := donut.RadialProfile(d.Stack, d.N)
	k := donut.EdgeRadius(prof)
	if k <= 0 {
		k = float64(d.N) / 3
	}
	// Inner half-maximum radius gives a first guess at the obstruction.
	im := 0
	for i, v := range prof {
		if v > prof[im] {
			im = i
		}
	}
	eps := 0.4
	for i := im; i > 0; i-- {
		if prof[i] < prof[im]/2 {
			eps = math.Max(0.15, math.Min(0.65, float64(i)/k))
			break
		}
	}
	var amp float64
	for _, v := range d.Stack {
		amp += v
	}
	return model.Side{K: d.Sign * k, Amp: amp}, eps
}

// FitDonuts fits the forward model to one or two sides jointly. start gives
// the initial shared parameters (nil: estimate). starts lists starting
// (shadow x, shadow y, spherical aberration) triples for a multi-start
// search; the lowest cost wins. The SA sign matters: one side of focus shows
// a crisp rim and the other a soft edge, and a fit started at the wrong sign
// can settle in a false minimum.
func FitDonuts(sides []SideData, start *model.Shared, mode FitMode, starts [][3]float64) FitResult {
	init := make([]model.Side, len(sides))
	var eps0 float64
	kmax := 0.0
	for i, d := range sides {
		init[i], eps0 = initialSide(d)
		kmax = math.Max(kmax, math.Abs(init[i].K))
	}
	sh := model.Shared{Sigma: 2.5, Eps: eps0}
	if start != nil {
		sh = *start
	}
	pupil := model.PupilFor(kmax)

	np := nShared + nSide*len(sides)
	lo := make([]float64, np)
	hi := make([]float64, np)
	step := make([]float64, np)
	fixed := make([]bool, np)
	set := func(j int, l, h, s float64) { lo[j], hi[j], step[j] = l, h, s }
	set(pCx, -40, 40, 0.02)
	set(pCy, -40, 40, 0.02)
	set(pSx, -0.35, 0.35, 0.004)
	set(pSy, -0.35, 0.35, 0.004)
	set(pGx, -0.9, 0.9, 0.004)
	set(pGy, -0.9, 0.9, 0.004)
	set(pSigma, 0.3, 25, 0.02)
	set(pEps, 0.05, 0.75, 0.004)
	set(pSA, -40, 40, 0.02)
	set(pA1, -30, 30, 0.02)
	set(pA2, -30, 30, 0.02)
	if mode == FitComaOnly {
		for j := pSx; j < nShared; j++ {
			fixed[j] = true
		}
	}
	affects := make([][]int, np)
	for i, d := range sides {
		o := nShared + nSide*i
		k := math.Abs(init[i].K)
		peak := 0.0
		for _, v := range d.Stack {
			peak = math.Max(peak, v)
		}
		if d.Sign > 0 {
			set(o+sK, 0.4*k, 2*k, 0.02)
		} else {
			set(o+sK, -2*k, -0.4*k, 0.02)
		}
		set(o+sX0, -20, 20, 0.02)
		set(o+sY0, -20, 20, 0.02)
		set(o+sAmp, 0, 10*init[i].Amp+1e-9, 1e-4*init[i].Amp)
		set(o+sBg, -0.1*peak, 0.1*peak, 1e-4*peak)
		for j := range nSide {
			affects[o+j] = []int{i}
		}
	}
	blocks := make([]int, len(sides))
	for i, d := range sides {
		blocks[i] = d.N * d.N
	}
	pr := fit.Problem{
		Blocks:  blocks,
		Affects: affects,
		Lower:   lo,
		Upper:   hi,
		Step:    step,
		Fixed:   fixed,
		Eval: func(p []float64, b int, r []float64) {
			d := sides[b]
			pupil.Render(r, d.N, unpackShared(p), unpackSide(p, b))
			for i, v := range d.Stack {
				r[i] -= v
			}
		},
	}
	if len(starts) == 0 || mode == FitComaOnly {
		starts = [][3]float64{{sh.Sx, sh.Sy, sh.SA}}
	}
	results := make([]fit.Result, len(starts))
	var wg sync.WaitGroup
	for si, s := range starts {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s0 := sh
			s0.Sx, s0.Sy, s0.SA = s[0], s[1], s[2]
			p0 := packShared(s0)
			for i := range sides {
				sd := init[i]
				if start != nil {
					// Coma moves the centroid by about c(1+ε²); start the
					// model centre on the other side so the stacks line up.
					f := 1 + s0.Eps*s0.Eps
					sd.X0, sd.Y0 = -s0.Cx*f, -s0.Cy*f
				}
				p0 = append(p0, sd.K, sd.X0, sd.Y0, sd.Amp, sd.Bg)
			}
			results[si], _ = fit.Solve(pr, p0, fit.Options{MaxIter: 80})
		}()
	}
	wg.Wait()
	best := results[0]
	for _, r := range results[1:] {
		if r.Cost < best.Cost {
			best = r
		}
	}
	out := FitResult{
		Shared:      unpackShared(best.P),
		SharedSigma: unpackShared(best.Sigma),
		Cost:        best.Cost,
		Iter:        best.Iter,
		Status:      best.Status,
	}
	var m, peak float64
	for i, d := range sides {
		out.Sides = append(out.Sides, unpackSide(best.P, i))
		out.SidesSigma = append(out.SidesSigma, unpackSide(best.Sigma, i))
		m += float64(d.N * d.N)
		for _, v := range d.Stack {
			peak = math.Max(peak, v)
		}
	}
	if m > 0 && peak > 0 {
		out.RMS = math.Sqrt(best.Cost/m) / peak
	}
	return out
}

// RenderModel renders the fitted model for side i (for diagnostics).
func RenderModel(fr FitResult, d SideData, i int) []float64 {
	p := model.PupilFor(fr.Sides[i].K)
	img := make([]float64, d.N*d.N)
	p.Render(img, d.N, fr.Shared, fr.Sides[i])
	return img
}
