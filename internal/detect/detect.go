// Package detect removes the sky background and finds isolated defocused stars.
package detect

import (
	"math"
	"runtime"
	"slices"
	"sort"
	"sync"
)

// Parallel splits [0, n) into chunks and runs fn on each chunk concurrently.
func Parallel(n int, fn func(lo, hi int)) {
	workers := runtime.GOMAXPROCS(0)
	if n < workers*4 {
		workers = max(1, n/4)
	}
	chunk := (n + workers - 1) / workers
	var wg sync.WaitGroup
	for lo := 0; lo < n; lo += chunk {
		hi := min(lo+chunk, n)
		wg.Add(1)
		go func() {
			defer wg.Done()
			fn(lo, hi)
		}()
	}
	wg.Wait()
}

// SubtractBackground removes a smooth sky background in place: medians of
// block×block tiles, a 5×5 median filter over the tile grid, then bilinear
// interpolation back to full resolution.
func SubtractBackground(pix []float32, w, h, block int) {
	gw, gh := (w+block-1)/block, (h+block-1)/block
	grid := make([]float32, gw*gh)
	Parallel(gh, func(lo, hi int) {
		buf := make([]float32, 0, block*block)
		for gy := lo; gy < hi; gy++ {
			for gx := 0; gx < gw; gx++ {
				buf = buf[:0]
				for y := gy * block; y < min((gy+1)*block, h); y++ {
					row := pix[y*w : (y+1)*w]
					buf = append(buf, row[gx*block:min((gx+1)*block, w)]...)
				}
				grid[gy*gw+gx] = median(buf)
			}
		}
	})
	grid = medianFilter(grid, gw, gh, 2)

	// Bilinear interpolation between tile centres.
	Parallel(h, func(lo, hi int) {
		for y := lo; y < hi; y++ {
			fy := (float64(y)+0.5)/float64(block) - 0.5
			y0 := clampInt(int(math.Floor(fy)), 0, gh-1)
			y1 := clampInt(y0+1, 0, gh-1)
			ty := float32(clamp(fy-float64(y0), 0, 1))
			for x := 0; x < w; x++ {
				fx := (float64(x)+0.5)/float64(block) - 0.5
				x0 := clampInt(int(math.Floor(fx)), 0, gw-1)
				x1 := clampInt(x0+1, 0, gw-1)
				tx := float32(clamp(fx-float64(x0), 0, 1))
				a := grid[y0*gw+x0]*(1-tx) + grid[y0*gw+x1]*tx
				b := grid[y1*gw+x0]*(1-tx) + grid[y1*gw+x1]*tx
				pix[y*w+x] -= a*(1-ty) + b*ty
			}
		}
	})
}

func medianFilter(g []float32, w, h, r int) []float32 {
	out := make([]float32, len(g))
	buf := make([]float32, 0, (2*r+1)*(2*r+1))
	for y := range h {
		for x := range w {
			buf = buf[:0]
			for yy := max(0, y-r); yy <= min(h-1, y+r); yy++ {
				for xx := max(0, x-r); xx <= min(w-1, x+r); xx++ {
					buf = append(buf, g[yy*w+xx])
				}
			}
			out[y*w+x] = median(buf)
		}
	}
	return out
}

// median returns the median of v, reordering v.
func median(v []float32) float32 {
	if len(v) == 0 {
		return 0
	}
	k := len(v) / 2
	quickselect(v, k)
	return v[k]
}

func quickselect(v []float32, k int) {
	lo, hi := 0, len(v)-1
	for lo < hi {
		p := v[(lo+hi)/2]
		i, j := lo, hi
		for i <= j {
			for v[i] < p {
				i++
			}
			for v[j] > p {
				j--
			}
			if i <= j {
				v[i], v[j] = v[j], v[i]
				i++
				j--
			}
		}
		if k <= j {
			hi = j
		} else if k >= i {
			lo = i
		} else {
			return
		}
	}
}

// Bin averages b×b blocks.
func Bin(pix []float32, w, h, b int) ([]float32, int, int) {
	bw, bh := w/b, h/b
	out := make([]float32, bw*bh)
	inv := 1 / float32(b*b)
	Parallel(bh, func(lo, hi int) {
		for by := lo; by < hi; by++ {
			for y := by * b; y < (by+1)*b; y++ {
				row := pix[y*w:]
				o := out[by*bw:]
				for bx := range bw {
					var s float32
					for x := bx * b; x < (bx+1)*b; x++ {
						s += row[x]
					}
					o[bx] += s
				}
			}
			for bx := range bw {
				out[by*bw+bx] *= inv
			}
		}
	})
	return out, bw, bh
}

// GaussianKernel returns a normalised 1-D Gaussian kernel of radius ceil(3σ).
func GaussianKernel(sigma float64) []float32 {
	r := max(1, int(math.Ceil(3*sigma)))
	k := make([]float32, 2*r+1)
	var s float64
	for i := -r; i <= r; i++ {
		v := math.Exp(-0.5 * float64(i*i) / (sigma * sigma))
		k[i+r] = float32(v)
		s += v
	}
	for i := range k {
		k[i] = float32(float64(k[i]) / s)
	}
	return k
}

// Smooth convolves with a separable Gaussian (edges clamped).
func Smooth(pix []float32, w, h int, sigma float64) []float32 {
	k := GaussianKernel(sigma)
	r := len(k) / 2
	tmp := make([]float32, len(pix))
	Parallel(h, func(lo, hi int) {
		for y := lo; y < hi; y++ {
			row := pix[y*w : (y+1)*w]
			for x := range w {
				var s float32
				for i, kv := range k {
					xx := clampInt(x+i-r, 0, w-1)
					s += kv * row[xx]
				}
				tmp[y*w+x] = s
			}
		}
	})
	out := make([]float32, len(pix))
	Parallel(h, func(lo, hi int) {
		for y := lo; y < hi; y++ {
			for x := range w {
				var s float32
				for i, kv := range k {
					yy := clampInt(y+i-r, 0, h-1)
					s += kv * tmp[yy*w+x]
				}
				out[y*w+x] = s
			}
		}
	})
	return out
}

// RobustSigma estimates the noise of a background-subtracted image from the
// median absolute deviation of a pixel subsample.
func RobustSigma(pix []float32) float64 {
	step := max(1, len(pix)/200000)
	s := make([]float32, 0, len(pix)/step+1)
	for i := 0; i < len(pix); i += step {
		s = append(s, pix[i])
	}
	m := median(s)
	for i := range s {
		s[i] = float32(math.Abs(float64(s[i] - m)))
	}
	return 1.4826 * float64(median(s))
}

// Peak is a local maximum in a smoothed image.
type Peak struct {
	X, Y  float64 // full-resolution pixel coordinates
	Value float64 // smoothed peak height (background subtracted)
}

// FindPeaks returns local maxima above thresh that are the highest point
// within radius pixels. Coordinates are scaled by scale (the bin factor).
func FindPeaks(sm []float32, w, h int, thresh float64, radius int, scale float64) []Peak {
	var cands []Peak
	var mu sync.Mutex
	Parallel(h, func(lo, hi int) {
		var local []Peak
		for y := max(lo, 1); y < min(hi, h-1); y++ {
			for x := 1; x < w-1; x++ {
				v := sm[y*w+x]
				if float64(v) < thresh {
					continue
				}
				if v < sm[y*w+x-1] || v < sm[y*w+x+1] || v < sm[(y-1)*w+x] || v < sm[(y+1)*w+x] ||
					v < sm[(y-1)*w+x-1] || v < sm[(y-1)*w+x+1] || v < sm[(y+1)*w+x-1] || v < sm[(y+1)*w+x+1] {
					continue
				}
				local = append(local, Peak{X: float64(x), Y: float64(y), Value: float64(v)})
			}
		}
		mu.Lock()
		cands = append(cands, local...)
		mu.Unlock()
	})
	// Non-maximum suppression: brightest first.
	sort.Slice(cands, func(i, j int) bool { return cands[i].Value > cands[j].Value })
	var kept []Peak
	r2 := float64(radius * radius)
	cell := float64(max(radius, 1))
	grid := map[[2]int][]int{}
	for _, c := range cands {
		gx, gy := int(c.X/cell), int(c.Y/cell)
		ok := true
	search:
		for dy := -1; dy <= 1; dy++ {
			for dx := -1; dx <= 1; dx++ {
				for _, i := range grid[[2]int{gx + dx, gy + dy}] {
					k := kept[i]
					if (k.X-c.X)*(k.X-c.X)+(k.Y-c.Y)*(k.Y-c.Y) <= r2 {
						ok = false
						break search
					}
				}
			}
		}
		if ok {
			grid[[2]int{gx, gy}] = append(grid[[2]int{gx, gy}], len(kept))
			kept = append(kept, c)
		}
	}
	for i := range kept {
		// Centre of the binned pixel in full-resolution coordinates.
		kept[i].X = (kept[i].X+0.5)*scale - 0.5
		kept[i].Y = (kept[i].Y+0.5)*scale - 0.5
	}
	return kept
}

// Isolated returns the peaks in stars that have no neighbour in all within
// dist pixels brighter than frac times their own value, and that lie at least
// margin pixels from the frame edge.
func Isolated(stars, all []Peak, dist, frac float64, w, h int, margin float64) []Peak {
	var out []Peak
	d2 := dist * dist
	for _, s := range stars {
		if s.X < margin || s.Y < margin || s.X > float64(w)-1-margin || s.Y > float64(h)-1-margin {
			continue
		}
		ok := true
		for _, n := range all {
			dx, dy := n.X-s.X, n.Y-s.Y
			if dx == 0 && dy == 0 {
				continue
			}
			if dx*dx+dy*dy < d2 && n.Value > frac*s.Value {
				ok = false
				break
			}
		}
		if ok {
			out = append(out, s)
		}
	}
	return out
}

// Percentile returns the p-th percentile (0..100) of v without modifying it.
func Percentile(v []float64, p float64) float64 {
	if len(v) == 0 {
		return math.NaN()
	}
	s := slices.Clone(v)
	slices.Sort(s)
	i := clampInt(int(math.Round(p/100*float64(len(s)-1))), 0, len(s)-1)
	return s[i]
}

func clamp(v, lo, hi float64) float64 { return math.Max(lo, math.Min(hi, v)) }

func clampInt(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}
