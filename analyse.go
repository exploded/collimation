package main

import (
	"context"
	"flag"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"math"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/exploded/collimation/internal/analysis"
)

// runAnalyse is the hidden `collimation analyse` subcommand used for testing.
func runAnalyse(args []string) error {
	fs := flag.NewFlagSet("analyse", flag.ExitOnError)
	focus := fs.Int("focus", 0, "best-focus position (0 = work it out)")
	png := fs.String("png", "", "write stack|model|residual PNGs with this prefix")
	split := fs.Bool("runs", false, "split the files into runs by time and analyse each")
	fs.Parse(args)
	files := fs.Args()
	if len(files) == 1 {
		if st, err := os.Stat(files[0]); err == nil && st.IsDir() {
			files, _ = filepath.Glob(filepath.Join(files[0], "*.fit*"))
		}
	}
	if len(files) == 0 {
		return fmt.Errorf("no FITS files given")
	}
	cfg := analysis.DefaultConfig()
	cfg.FocusPos = *focus
	groups := [][]string{files}
	if *split {
		var infos []analysis.FrameInfo
		for _, f := range files {
			fi, err := analysis.ReadInfo(f, cfg)
			if err != nil {
				return err
			}
			infos = append(infos, fi)
		}
		groups = nil
		for _, r := range analysis.SplitRuns(infos, 15*time.Minute) {
			var g []string
			for _, f := range r.Frames {
				g = append(g, f.Path)
			}
			groups = append(groups, g)
			fmt.Println("Run:", r.Label())
		}
	}
	for gi, g := range groups {
		res, err := analysis.Analyze(context.Background(), g, cfg, func(m string) { fmt.Println("  ", m) })
		if err != nil {
			return err
		}
		printResult(res)
		if *png != "" {
			prefix := *png
			if len(groups) > 1 {
				prefix = fmt.Sprintf("%s_run%d", *png, gi+1)
			}
			for i, s := range res.Sides {
				name := fmt.Sprintf("%s_side%d_%d.png", prefix, i, s.Pos)
				if err := writeTriptych(name, s.Stack, s.Model, s.N); err != nil {
					return err
				}
				fmt.Println("  wrote", name)
			}
		}
	}
	return nil
}

func printResult(r *analysis.Result) {
	f := r.Fit.Shared
	e := r.Fit.SharedSigma
	fmt.Printf("\nFocus %d (%s); %d in-focus, %d unused frames\n", r.Plan.Focus, r.Plan.FocusFrom, len(r.Plan.InFocus), len(r.Plan.Unused))
	for _, s := range r.Sides {
		fmt.Printf("  FOCPOS %d: %d frames, %d stars, K = %+.2f px, cutout %d px, rejected %v\n", s.Pos, s.Frames, s.Stars, s.K, s.N, s.Rejected)
	}
	fmt.Printf("Coma          (%+.2f, %+.2f) px  |c| = %.2f ± %.2f px\n", r.ComaX, r.ComaY, r.ComaMag, r.ComaErr)
	fmt.Printf("Axis lands at (%+.2f, %+.2f) mm from sensor centre; decentre %.2f mm\n", r.AxisXmm, r.AxisYmm, r.DecentreMM)
	fmt.Printf("Seeing σ      %.2f px   obstruction %.3f ± %.3f   SA %+.2f px   astig (%+.2f, %+.2f)\n", f.Sigma, f.Eps, e.Eps, f.SA, f.A1, f.A2)
	fmt.Printf("Shadow        (%+.3f, %+.3f)   gradient (%+.3f, %+.3f)\n", f.Sx, f.Sy, f.Gx, f.Gy)
	if r.StepUM > 0 {
		fmt.Printf("Paraxial focus %.0f   focuser %.2f µm/step\n", r.ParaxialFocus, r.StepUM)
	}
	fmt.Printf("Fit: %s after %d iterations, RMS %.4f of peak; %.1f s\n", r.Fit.Status, r.Fit.Iter, r.Fit.RMS, r.Elapsed.Seconds())
	fmt.Println("Regions (row, col): coma px")
	for _, g := range r.Regions {
		if g.OK {
			fmt.Printf("  (%d,%d) %3d stars  (%+.2f, %+.2f)\n", g.Row, g.Col, g.Stars, g.Cx, g.Cy)
		} else {
			fmt.Printf("  (%d,%d) %3d stars  —\n", g.Row, g.Col, g.Stars)
		}
	}
	fmt.Printf("Radial coma term a = %+.2f px per half-diagonal\n", r.RadialA)
	if t := r.Tilt; t.OK {
		rough := ""
		if t.Rough {
			rough = " (rough: small donuts)"
		}
		fmt.Printf("Tilt          (%+.2f, %+.2f) ± %.2f mrad%s; corner %.0f µm; curvature %+.3f µm/mm²\n", t.X, t.Y, t.Err, rough, t.CornerUM, t.Curv)
		fmt.Printf("  if from the mirrors: secondary %.1f mrad off, primary pulled %.1f mrad\n", t.SecondaryMrad, t.PrimaryMrad)
	}
	if g := r.Pupil; g.OK {
		fmt.Printf("Shadow drift  %+.4f ± %.4f /mm (geometry %.4f: secondary %.0f mm up); inside focus: %+d (+1 = higher FOCPOS)\n",
			g.Slope, g.SlopeErr, g.SlopeGeo, g.HeightMM, g.IntraHigh)
		fmt.Printf("At the secondary, from the primary's axis: shadow (%+.1f, %+.1f) mm", g.ShadowX, g.ShadowY)
		if g.Hub {
			fmt.Printf(", spider hub (%+.1f, %+.1f) mm → %.1f mrad off the tube", g.HubX, g.HubY, g.TubeMrad)
		}
		fmt.Printf(" ± %.1f mm\n", g.ErrMM)
	}
	fmt.Println("Per frame:")
	for _, fc := range r.Frames {
		fmt.Printf("  %s FOCPOS %d %3d stars (%+.2f, %+.2f)\n", filepath.Base(fc.Path), fc.Pos, fc.Stars, fc.Cx, fc.Cy)
	}
	if len(r.Warnings) > 0 {
		fmt.Println("Warnings:", strings.Join(r.Warnings, " "))
	}
}

// writeTriptych writes stack | model | residual side by side.
func writeTriptych(path string, stack, mdl []float64, n int) error {
	img := image.NewGray(image.Rect(0, 0, 3*n+4, n))
	var peak float64
	for _, v := range stack {
		peak = math.Max(peak, v)
	}
	put := func(ox int, v []float64, scale, offset float64) {
		for y := range n {
			for x := range n {
				g := (v[y*n+x]/peak)*scale + offset
				img.SetGray(ox+x, y, color.Gray{uint8(math.Max(0, math.Min(255, g*255)))})
			}
		}
	}
	put(0, stack, 1, 0)
	put(n+2, mdl, 1, 0)
	res := make([]float64, n*n)
	for i := range res {
		res[i] = stack[i] - mdl[i]
	}
	put(2*n+4, res, 2.5, 0.5)
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return png.Encode(f, img)
}
