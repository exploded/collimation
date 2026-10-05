// Package analysis turns defocused FITS frames into collimation measurements:
// coma (axis decentre), seeing, spherical aberration, obstruction and the
// focus scale.
package analysis

import (
	"cmp"
	"fmt"
	"math"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/exploded/collimation/internal/detect"
	"github.com/exploded/collimation/internal/donut"
	"github.com/exploded/collimation/internal/fits"
)

// Config holds telescope and camera constants.
type Config struct {
	PixelUM       float64 // pixel size (µm)
	ApertureMM    float64 // clear aperture (mm)
	FocalMM       float64 // system focal length (mm); 0 = FOCALLEN from the header
	PrimaryFRatio float64 // primary f-ratio used for coma → decentre
	StepUM        float64 // focuser travel per step (µm), first guess only
	FocusPos      int     // best-focus position; 0 = work it out
	Sat           float64 // saturation level (ADU)
	MaxStars      int     // per frame
	Regions       int     // region grid is Regions × Regions
}

// DefaultConfig is the AT12IN (305 mm f/4 primary, Wynne corrector) with an
// ASI2600MM (3.76 µm pixels).
func DefaultConfig() Config {
	return Config{
		PixelUM:       3.76,
		ApertureMM:    305,
		FocalMM:       0,
		PrimaryFRatio: 4.0,
		StepUM:        3.36,
		Sat:           60000,
		MaxStars:      400,
		Regions:       3,
	}
}

// FRatio returns the system focal ratio, using the header focal length if the
// config does not set one.
func (c Config) FRatio(h fits.Header) float64 {
	f := c.FocalMM
	if f == 0 {
		f = h.Float("FOCALLEN", 1156)
	}
	return f / c.ApertureMM
}

// FrameInfo is the header summary of one frame.
type FrameInfo struct {
	Path    string
	FocPos  int
	Time    time.Time
	Alt, Az float64
	FocTemp float64
	AmbTemp float64
	Filter  string
	Exp     float64
	Pier    string
	W, H    int
	NRatio  float64 // system f-ratio
}

// ReadInfo reads the header of a frame.
func ReadInfo(path string, cfg Config) (FrameInfo, error) {
	h, err := fits.ReadHeader(path)
	if err != nil {
		return FrameInfo{}, err
	}
	return infoFromHeader(path, h, cfg), nil
}

func infoFromHeader(path string, h fits.Header, cfg Config) FrameInfo {
	fi := FrameInfo{
		Path:    path,
		FocPos:  h.Int("FOCPOS", h.Int("FOCUSPOS", 0)),
		Alt:     h.Float("CENTALT", math.NaN()),
		Az:      h.Float("CENTAZ", math.NaN()),
		FocTemp: h.Float("FOCTEMP", math.NaN()),
		AmbTemp: h.Float("AMBTEMP", math.NaN()),
		Filter:  h.String("FILTER"),
		Exp:     h.Float("EXPTIME", 0),
		Pier:    h.String("PIERSIDE"),
		W:       h.Int("NAXIS1", 0),
		H:       h.Int("NAXIS2", 0),
		NRatio:  cfg.FRatio(h),
	}
	for _, key := range []string{"DATE-LOC", "DATE-OBS"} {
		if s := h.String(key); s != "" {
			for _, layout := range []string{"2006-01-02T15:04:05.9999999", "2006-01-02T15:04:05"} {
				if t, err := time.ParseInLocation(layout, s, time.Local); err == nil {
					fi.Time = t
					break
				}
			}
			if !fi.Time.IsZero() {
				break
			}
		}
	}
	return fi
}

// Run is a set of frames taken together (no gap longer than the split gap).
type Run struct {
	Frames []FrameInfo
}

// Label describes the run for display.
func (r Run) Label() string {
	if len(r.Frames) == 0 {
		return "empty"
	}
	pos := map[int]int{}
	for _, f := range r.Frames {
		pos[f.FocPos]++
	}
	keys := make([]int, 0, len(pos))
	for k := range pos {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	var parts []string
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%d×%d", k, pos[k]))
	}
	t := r.Frames[0].Time
	return fmt.Sprintf("%s  %d frames  FOCPOS %s", t.Format("2006-01-02 15:04"), len(r.Frames), strings.Join(parts, ", "))
}

// SplitRuns sorts frames by time and splits wherever the gap exceeds gap.
func SplitRuns(infos []FrameInfo, gap time.Duration) []Run {
	s := slices.Clone(infos)
	slices.SortFunc(s, func(a, b FrameInfo) int { return a.Time.Compare(b.Time) })
	var runs []Run
	for i, f := range s {
		if i == 0 || f.Time.Sub(s[i-1].Time) > gap {
			runs = append(runs, Run{})
		}
		runs[len(runs)-1].Frames = append(runs[len(runs)-1].Frames, f)
	}
	return runs
}

// posGroup is a set of frames at (nearly) the same focuser position.
type posGroup struct {
	Pos    int
	Frames []FrameInfo
}

func groupByPos(infos []FrameInfo) []posGroup {
	s := slices.Clone(infos)
	slices.SortFunc(s, func(a, b FrameInfo) int { return cmp.Compare(a.FocPos, b.FocPos) })
	var gs []posGroup
	for _, f := range s {
		if len(gs) > 0 && f.FocPos-gs[len(gs)-1].Pos <= 20 {
			gs[len(gs)-1].Frames = append(gs[len(gs)-1].Frames, f)
			continue
		}
		gs = append(gs, posGroup{Pos: f.FocPos, Frames: []FrameInfo{f}})
	}
	return gs
}

// Plan says which frames form each side of focus.
type Plan struct {
	Focus     int         // focus position used to pick sides
	FocusFrom string      // "given", "middle group" or "midpoint"
	Sides     []posGroup  // 1 or 2 groups; Sides[0] has the lower position
	InFocus   []FrameInfo // frames near focus (not analysed as donuts)
	Unused    []FrameInfo
}

// MakePlan picks the focus position and the frame groups for each side.
func MakePlan(infos []FrameInfo, cfg Config) (Plan, error) {
	gs := groupByPos(infos)
	if len(gs) == 0 {
		return Plan{}, fmt.Errorf("no frames")
	}
	var p Plan
	switch {
	case cfg.FocusPos != 0:
		p.Focus, p.FocusFrom = cfg.FocusPos, "given"
	case len(gs) >= 3:
		// A group lying between the lowest and highest positions is focus;
		// take the one with the most frames.
		mid := gs[1]
		for _, g := range gs[1 : len(gs)-1] {
			if len(g.Frames) > len(mid.Frames) {
				mid = g
			}
		}
		p.Focus, p.FocusFrom = mid.Pos, "middle group"
	case len(gs) == 2:
		p.Focus, p.FocusFrom = (gs[0].Pos+gs[1].Pos)/2, "midpoint"
	default:
		p.Focus, p.FocusFrom = 0, "single position"
	}
	// Minimum defocus for a usable donut: K ≥ 8 px.
	n := infos[0].NRatio
	minSteps := 8 * 2 * n * cfg.PixelUM / cfg.StepUM
	var below, above []posGroup
	for _, g := range gs {
		d := g.Pos - p.Focus
		switch {
		case len(gs) == 1:
			above = append(above, g)
		case math.Abs(float64(d)) < minSteps:
			p.InFocus = append(p.InFocus, g.Frames...)
		case d < 0:
			below = append(below, g)
		default:
			above = append(above, g)
		}
	}
	pick := func(c []posGroup) *posGroup {
		if len(c) == 0 {
			return nil
		}
		best := c[0]
		for _, g := range c[1:] {
			if len(g.Frames) > len(best.Frames) ||
				(len(g.Frames) == len(best.Frames) && abs(g.Pos-p.Focus) > abs(best.Pos-p.Focus)) {
				best = g
			}
		}
		return &best
	}
	for _, c := range [][]posGroup{below, above} {
		if g := pick(c); g != nil {
			p.Sides = append(p.Sides, *g)
			for _, o := range c {
				if o.Pos != g.Pos {
					p.Unused = append(p.Unused, o.Frames...)
				}
			}
		}
	}
	if len(p.Sides) == 0 {
		return p, fmt.Errorf("no defocused frames (all within %.0f steps of focus %d)", minSteps, p.Focus)
	}
	return p, nil
}

// ExpectedK is the defocus radius (px) expected from the focuser offset.
func ExpectedK(steps float64, nRatio float64, cfg Config) float64 {
	return steps * cfg.StepUM / (2 * nRatio * cfg.PixelUM)
}

// Frame is one analysed frame.
type Frame struct {
	Info     FrameInfo
	K        float64 // measured donut edge radius (px)
	Noise    float64
	Detected int
	Isolated int
	Rejected map[string]int
	Stars    []*donut.Cutout
}

// LoadFrame reads a frame, removes the background, finds isolated donuts and
// cuts them out. kGuess is the expected donut radius in px.
func LoadFrame(path string, kGuess float64, cfg Config) (*Frame, error) {
	im, err := fits.Read(path)
	if err != nil {
		return nil, err
	}
	fr := &Frame{Info: infoFromHeader(path, im.Header, cfg), Rejected: map[string]int{}}
	w, h := im.W, im.H
	kGuess = math.Max(kGuess, 6)

	// Keep a copy of raw values for saturation via the Sat threshold on the
	// background-subtracted image (the sky is small next to saturation).
	detect.SubtractBackground(im.Pix, w, h, 128)
	fr.Noise = detect.RobustSigma(im.Pix)

	b := max(1, int(kGuess/10))
	bin, bw, bh := im.Pix, w, h
	if b > 1 {
		bin, bw, bh = detect.Bin(im.Pix, w, h, b)
	}
	// Smooth enough to fill the donut hole, and suppress every peak within a
	// donut diameter so that one star gives one peak.
	sm := detect.Smooth(bin, bw, bh, math.Max(1, kGuess/3/float64(b)))
	smNoise := detect.RobustSigma(sm)
	nms := max(2, int(1.6*kGuess/float64(b)))
	all := detect.FindPeaks(sm, bw, bh, 3*smNoise, nms, float64(b))
	var cands []detect.Peak
	for _, p := range all {
		if p.Value > 10*smNoise {
			cands = append(cands, p)
		}
	}
	fr.Detected = len(cands)
	margin := float64(donut.HalfSize(kGuess*1.3) + 4)
	iso := detect.Isolated(cands, all, 2.2*kGuess, 0.04, w, h, margin)
	sort.Slice(iso, func(i, j int) bool { return iso[i].Value > iso[j].Value })

	sat := cfg.Sat - 1000
	// Pass 1: measure the donut radius from a few bright stars.
	k := kGuess
	{
		var cs []*donut.Cutout
		for _, p := range iso {
			c, _ := donut.Extract(im.Pix, w, h, p.X, p.Y, donut.Options{K: k, R: donut.HalfSize(k * 1.3), Noise: fr.Noise, Sat: sat})
			if c != nil {
				cs = append(cs, c)
			}
			if len(cs) >= 25 {
				break
			}
		}
		if len(cs) >= 3 {
			st := donut.Stack(cs)
			if e := donut.EdgeRadius(donut.RadialProfile(st, cs[0].N)); e > 3 {
				k = e
			}
		}
	}
	fr.K = k
	// Pass 2: extract with the measured radius.
	if math.Abs(k-kGuess) > 0.2*kGuess {
		iso = detect.Isolated(cands, all, 2.2*k, 0.04, w, h, float64(donut.HalfSize(k)+4))
		sort.Slice(iso, func(i, j int) bool { return iso[i].Value > iso[j].Value })
	}
	fr.Isolated = len(iso)
	R := donut.HalfSize(k)
	for _, p := range iso {
		c, why := donut.Extract(im.Pix, w, h, p.X, p.Y, donut.Options{K: k, R: R, Noise: fr.Noise, Sat: sat})
		if c == nil {
			fr.Rejected[why]++
			continue
		}
		fr.Stars = append(fr.Stars, c)
		if len(fr.Stars) >= cfg.MaxStars {
			break
		}
	}
	return fr, nil
}

func abs(i int) int {
	if i < 0 {
		return -i
	}
	return i
}
