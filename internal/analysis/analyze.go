package analysis

import (
	"context"
	"fmt"
	"math"
	"slices"
	"sync"
	"time"

	"github.com/exploded/collimation/internal/donut"
	"github.com/exploded/collimation/internal/model"
)

// RegionResult is the coma fitted to the stars in one field region.
type RegionResult struct {
	Row, Col int
	CenterX  float64 // region centre relative to frame centre, in half-diagonals
	CenterY  float64
	Xmm, Ymm float64 // mean star position relative to the sensor centre (mm)
	Stars    int
	Cx, Cy   float64
	K        []float64 // fitted defocus radius per side (px, signed)
	OK       bool

	// Pupil geometry (pupil units, fit frame): shadow centre and spider hub.
	Sx, Sy, Vx, Vy float64
	PupilOK        bool
}

// FrameComa is the coma fitted to a single frame.
type FrameComa struct {
	Path   string
	Pos    int
	Stars  int
	Cx, Cy float64
}

// SideSummary describes one side of focus.
type SideSummary struct {
	Pos      int
	Frames   int
	Stars    int
	K        float64 // fitted defocus radius (px, signed)
	N        int
	Stack    []float64
	Model    []float64
	Rejected map[string]int
	// Positions are the star centroids in this side's first frame, brightest
	// first; comparing them between measurements gives the star shift.
	Positions []Point
}

// Result is a complete measurement.
type Result struct {
	Plan     Plan
	Fit      FitResult
	Sides    []SideSummary
	Regions  []RegionResult
	Frames   []FrameComa
	Infos    []FrameInfo
	Warnings []string

	// Derived quantities.
	ComaX, ComaY  float64 // px
	ComaMag       float64 // px
	ComaErr       float64 // 1σ on each component (px)
	AxisXmm       float64 // where the primary's axis lands, relative to the sensor centre
	AxisYmm       float64
	DecentreMM    float64
	RadialA       float64       // coma growth per half-diagonal (px), from the regions
	ParaxialFocus float64       // focuser position where K = 0 (two sides only)
	StepUM        float64       // measured µm per step (two sides only)
	Tilt          Tilt          // focal-plane tilt (two sides only)
	Pupil         PupilGeometry // secondary and spider in the beam
	Alt, Az       float64
	FocTemp       float64
	AmbTemp       float64
	Elapsed       time.Duration
	NRatio        float64
}

// Progress receives status messages during analysis.
type Progress func(msg string)

// Analyze measures collimation from a set of defocused frames.
func Analyze(ctx context.Context, paths []string, cfg Config, progress Progress) (*Result, error) {
	t0 := time.Now()
	if progress == nil {
		progress = func(string) {}
	}
	var infos []FrameInfo
	for _, p := range paths {
		fi, err := ReadInfo(p, cfg)
		if err != nil {
			return nil, err
		}
		infos = append(infos, fi)
	}
	plan, err := MakePlan(infos, cfg)
	if err != nil {
		return nil, err
	}
	res := &Result{Plan: plan, Infos: infos, NRatio: infos[0].NRatio}

	// Load frames, three at a time to bound memory (~150 MB each).
	type job struct {
		side int
		info FrameInfo
	}
	var jobs []job
	for si, g := range plan.Sides {
		for _, f := range g.Frames {
			jobs = append(jobs, job{si, f})
		}
	}
	frames := make([][]*Frame, len(plan.Sides))
	var mu sync.Mutex
	var wg sync.WaitGroup
	sem := make(chan struct{}, 3)
	var firstErr error
	done := 0
	for _, j := range jobs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			if ctx.Err() != nil {
				return
			}
			steps := math.Abs(float64(j.info.FocPos - plan.Focus))
			if plan.FocusFrom == "single position" {
				steps = 500
			}
			k := ExpectedK(steps, j.info.NRatio, cfg)
			fr, err := LoadFrame(j.info.Path, k, cfg)
			mu.Lock()
			defer mu.Unlock()
			done++
			if err != nil {
				if firstErr == nil {
					firstErr = err
				}
				return
			}
			frames[j.side] = append(frames[j.side], fr)
			progress(fmt.Sprintf("Frame %d/%d: %d detected, %d isolated, %d used (K ≈ %.0f px)", done, len(jobs), fr.Detected, fr.Isolated, len(fr.Stars), fr.K))
		}()
	}
	wg.Wait()
	if firstErr != nil {
		return nil, firstErr
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	// Build per-side stacks.
	hiPos := plan.Sides[len(plan.Sides)-1].Pos
	var sides []SideData
	var sideStars [][]*donut.Cutout
	for si, g := range plan.Sides {
		var all []*donut.Cutout
		rej := map[string]int{}
		for _, fr := range frames[si] {
			all = append(all, fr.Stars...)
			for k, v := range fr.Rejected {
				rej[k] += v
			}
		}
		if len(all) < 5 {
			return nil, fmt.Errorf("only %d usable stars at FOCPOS %d", len(all), g.Pos)
		}
		all = cropToCommon(all)
		sign := -1.0
		if g.Pos == hiPos {
			sign = 1
		}
		if len(plan.Sides) == 1 {
			sign = 1
		}
		sides = append(sides, SideData{Pos: g.Pos, Sign: sign, Stack: donut.Stack(all), N: all[0].N, Stars: len(all), Frames: len(frames[si])})
		sideStars = append(sideStars, all)
		res.Sides = append(res.Sides, SideSummary{Pos: g.Pos, Frames: len(frames[si]), Stars: len(all), N: all[0].N, Rejected: rej,
			Positions: firstFramePositions(frames[si])})
	}

	progress("Fitting the donut model…")
	fr := FitDonuts(sides, nil, FitAll, DefaultStarts(sides))
	res.Fit = fr
	for i := range sides {
		res.Sides[i].K = fr.Sides[i].K
		res.Sides[i].Stack = sides[i].Stack
		res.Sides[i].Model = RenderModel(fr, sides[i], i)
	}

	// Pupil geometry with the spider, when the donuts are big enough.
	var pupil *model.Shared
	if minAbsK(fr) >= minPupilK {
		progress("Fitting the pupil (secondary shadow and spider)…")
		res.Pupil.Fit = fitPupil(sides, fr)
		res.Pupil.OK = true
		pupil = &res.Pupil.Fit.Shared
	}

	// Per-region coma (everything else fixed), then per-region pupil shift.
	progress("Fitting field regions…")
	w, h := infos[0].W, infos[0].H
	res.Regions = fitRegions(sideStars, sides, fr, pupil, w, h, cfg)

	// Per-frame coma for frame-to-frame stability.
	progress("Fitting individual frames…")
	res.Frames = fitFrames(frames, sides, fr)

	derive(res, cfg, w, h)
	res.Elapsed = time.Since(t0)
	return res, nil
}

// DefaultStarts returns the multi-start grid: five shadow offsets, each with
// spherical aberration of either sign scaled to the donut size.
func DefaultStarts(sides []SideData) [][3]float64 {
	k := 0.0
	for _, d := range sides {
		in, _ := initialSide(d)
		k = math.Max(k, math.Abs(in.K))
	}
	sa := math.Max(1, 0.06*k)
	var out [][3]float64
	for _, s := range [][2]float64{{0, 0}, {0.06, 0}, {-0.06, 0}, {0, 0.06}, {0, -0.06}} {
		out = append(out, [3]float64{s[0], s[1], sa}, [3]float64{s[0], s[1], -sa})
	}
	return out
}

func firstFramePositions(fs []*Frame) []Point {
	if len(fs) == 0 {
		return nil
	}
	first := fs[0]
	for _, f := range fs[1:] {
		if f.Info.Time.Before(first.Info.Time) {
			first = f
		}
	}
	return positions(first.Stars)
}

func positions(cs []*donut.Cutout) []Point {
	out := make([]Point, len(cs))
	for i, c := range cs {
		out[i] = Point{c.X, c.Y}
	}
	return out
}

// cropToCommon crops all cutouts to the smallest size among them.
func cropToCommon(cs []*donut.Cutout) []*donut.Cutout {
	n := cs[0].N
	for _, c := range cs {
		n = min(n, c.N)
	}
	out := make([]*donut.Cutout, len(cs))
	for i, c := range cs {
		out[i] = crop(c, n)
	}
	return out
}

func crop(c *donut.Cutout, n int) *donut.Cutout {
	if c.N == n {
		return c
	}
	off := (c.N - n) / 2
	pix := make([]float32, n*n)
	for y := range n {
		copy(pix[y*n:(y+1)*n], c.Pix[(y+off)*c.N+off:])
	}
	cc := *c
	cc.N, cc.Pix = n, pix
	return &cc
}

func minAbsK(fr FitResult) float64 {
	k := math.Inf(1)
	for _, s := range fr.Sides {
		k = math.Min(k, math.Abs(s.K))
	}
	return k
}

func fitRegions(stars [][]*donut.Cutout, sides []SideData, global FitResult, pupil *model.Shared, w, h int, cfg Config) []RegionResult {
	grid := cfg.Regions
	if grid < 1 {
		return nil
	}
	halfDiag := math.Hypot(float64(w), float64(h)) / 2
	var out []RegionResult
	var mu sync.Mutex
	var wg sync.WaitGroup
	for row := range grid {
		for col := range grid {
			wg.Add(1)
			go func() {
				defer wg.Done()
				rr := RegionResult{Row: row, Col: col,
					CenterX: ((float64(col)+0.5)*float64(w)/float64(grid) - float64(w)/2) / halfDiag,
					CenterY: ((float64(row)+0.5)*float64(h)/float64(grid) - float64(h)/2) / halfDiag,
				}
				var sub []SideData
				ok := true
				var mx, my float64
				for si, cs := range stars {
					var in []*donut.Cutout
					for _, c := range cs {
						if int(c.X*float64(grid)/float64(w)) == col && int(c.Y*float64(grid)/float64(h)) == row {
							in = append(in, c)
							mx += c.X
							my += c.Y
						}
					}
					if len(in) < 3 {
						ok = false
					}
					rr.Stars += len(in)
					if len(in) > 0 {
						d := sides[si]
						d.Stack, d.Stars = donut.Stack(in), len(in)
						sub = append(sub, d)
					}
				}
				if rr.Stars > 0 {
					mm := cfg.PixelUM / 1000
					rr.Xmm = (mx/float64(rr.Stars) - float64(w)/2) * mm
					rr.Ymm = (my/float64(rr.Stars) - float64(h)/2) * mm
				}
				if ok {
					sh := global.Shared
					f := FitDonuts(sub, &sh, FitComaOnly, nil)
					rr.Cx, rr.Cy, rr.OK = f.Shared.Cx, f.Shared.Cy, true
					for _, s := range f.Sides {
						rr.K = append(rr.K, s.K)
					}
					if pupil != nil {
						st := startPupilRegion(*pupil, f.Shared)
						p := FitDonuts(sub, &st, FitPupilShift, nil)
						rr.Sx, rr.Sy, rr.Vx, rr.Vy, rr.PupilOK = p.Shared.Sx, p.Shared.Sy, p.Shared.Vx, p.Shared.Vy, true
					}
				}
				mu.Lock()
				out = append(out, rr)
				mu.Unlock()
			}()
		}
	}
	wg.Wait()
	slices.SortFunc(out, func(a, b RegionResult) int { return (a.Row*grid + a.Col) - (b.Row*grid + b.Col) })
	return out
}

func fitFrames(frames [][]*Frame, sides []SideData, global FitResult) []FrameComa {
	var out []FrameComa
	var mu sync.Mutex
	var wg sync.WaitGroup
	for si, fs := range frames {
		for _, fr := range fs {
			if len(fr.Stars) < 5 {
				continue
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				cs := cropToCommonSize(fr.Stars, sides[si].N)
				d := sides[si]
				d.Stack, d.Stars = donut.Stack(cs), len(cs)
				sh := global.Shared
				f := FitDonuts([]SideData{d}, &sh, FitComaOnly, nil)
				mu.Lock()
				out = append(out, FrameComa{Path: fr.Info.Path, Pos: fr.Info.FocPos, Stars: len(cs), Cx: f.Shared.Cx, Cy: f.Shared.Cy})
				mu.Unlock()
			}()
		}
	}
	wg.Wait()
	slices.SortFunc(out, func(a, b FrameComa) int {
		if a.Path < b.Path {
			return -1
		}
		if a.Path > b.Path {
			return 1
		}
		return 0
	})
	return out
}

func cropToCommonSize(cs []*donut.Cutout, n int) []*donut.Cutout {
	out := make([]*donut.Cutout, 0, len(cs))
	for _, c := range cs {
		if c.N >= n {
			out = append(out, crop(c, n))
		}
	}
	return out
}

// ComaToAxisMM converts coma (px) into where the primary's axis lands on the
// sensor (mm from centre). The axis lies opposite the flare direction:
// δ ≈ 16·N²·c / 0.95, with N the primary's focal ratio and c the sagittal
// coma radius.
func ComaToAxisMM(cx, cy float64, cfg Config) (float64, float64) {
	f := 16 * cfg.PrimaryFRatio * cfg.PrimaryFRatio / 0.95 * cfg.PixelUM * 1e-3
	return -cx * f, -cy * f
}

// ComaPerShift is the coma change (px) per pixel of star shift when the
// primary tilts. A tilt θ moves the stars by 2θ·F (system focal length) and
// the axis by θ·f (primary) the same way, so the coma changes the opposite
// way to the shift. On 5 Oct 2026 the screw turns gave 1/494 to 1/543,
// against 1/511 from this.
func ComaPerShift(cfg Config) float64 {
	F := cfg.FocalMM
	if F == 0 {
		F = 1156
	}
	mmPerShiftPx := cfg.PrimaryFRatio * cfg.ApertureMM / (2 * F) * cfg.PixelUM * 1e-3
	mmPerComaPx, _ := ComaToAxisMM(-1, 0, cfg)
	return mmPerShiftPx / mmPerComaPx
}

func derive(res *Result, cfg Config, w, h int) {
	f := res.Fit
	res.ComaX, res.ComaY = f.Shared.Cx, f.Shared.Cy
	res.ComaMag = math.Hypot(res.ComaX, res.ComaY)
	res.AxisXmm, res.AxisYmm = ComaToAxisMM(res.ComaX, res.ComaY, cfg)
	res.DecentreMM = math.Hypot(res.AxisXmm, res.AxisYmm)

	// Uncertainty: frame-to-frame scatter of the mean, or the formal error.
	res.ComaErr = math.Max(f.SharedSigma.Cx, f.SharedSigma.Cy)
	if n := len(res.Frames); n >= 2 {
		var mx, my float64
		for _, fc := range res.Frames {
			mx += fc.Cx
			my += fc.Cy
		}
		mx /= float64(n)
		my /= float64(n)
		var v float64
		for _, fc := range res.Frames {
			v += (fc.Cx-mx)*(fc.Cx-mx) + (fc.Cy-my)*(fc.Cy-my)
		}
		sd := math.Sqrt(v / float64(2*(n-1)))
		res.ComaErr = math.Max(res.ComaErr, sd/math.Sqrt(float64(n)))
	}

	// Uniform + radial coma: c(p) = a·p + b over the regions.
	var ata [3][3]float64
	var atb [3]float64
	nOK := 0
	for _, r := range res.Regions {
		if !r.OK {
			continue
		}
		nOK++
		rows := [][4]float64{{r.CenterX, 1, 0, r.Cx}, {r.CenterY, 0, 1, r.Cy}}
		for _, row := range rows {
			for i := range 3 {
				for j := range 3 {
					ata[i][j] += row[i] * row[j]
				}
				atb[i] += row[i] * row[3]
			}
		}
	}
	if nOK >= 3 {
		if x, ok := solve3(ata, atb); ok {
			res.RadialA = x[0]
		}
	}

	// Focus scale from the two fitted defocus radii.
	if len(res.Sides) == 2 {
		lo, hi := res.Sides[0], res.Sides[1]
		slope := (hi.K - lo.K) / float64(hi.Pos-lo.Pos) // px per step
		if slope != 0 {
			res.ParaxialFocus = float64(hi.Pos) - hi.K/slope
			res.StepUM = math.Abs(slope) * 2 * res.NRatio * cfg.PixelUM
		}
	}

	deriveTilt(res, cfg)
	derivePupil(res, cfg)

	var alt, az, ft, at float64
	for _, fi := range res.Infos {
		alt += fi.Alt
		az += fi.Az
		ft += fi.FocTemp
		at += fi.AmbTemp
	}
	n := float64(len(res.Infos))
	res.Alt, res.Az, res.FocTemp, res.AmbTemp = alt/n, az/n, ft/n, at/n

	if f.Shared.Eps > 0.42 {
		res.Warnings = append(res.Warnings, fmt.Sprintf("Obstruction %.2f is larger than the 4\" secondary explains (0.33).", f.Shared.Eps))
	}
	for _, s := range res.Sides {
		if s.Stars < 20 {
			res.Warnings = append(res.Warnings, fmt.Sprintf("Only %d stars at FOCPOS %d.", s.Stars, s.Pos))
		}
	}
}

func solve3(a [3][3]float64, b [3]float64) ([3]float64, bool) {
	det := func(m [3][3]float64) float64 {
		return m[0][0]*(m[1][1]*m[2][2]-m[1][2]*m[2][1]) -
			m[0][1]*(m[1][0]*m[2][2]-m[1][2]*m[2][0]) +
			m[0][2]*(m[1][0]*m[2][1]-m[1][1]*m[2][0])
	}
	d := det(a)
	if math.Abs(d) < 1e-12 {
		return [3]float64{}, false
	}
	var x [3]float64
	for i := range 3 {
		m := a
		for r := range 3 {
			m[r][i] = b[r]
		}
		x[i] = det(m) / d
	}
	return x, true
}
