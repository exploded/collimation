// Package fit is a small bounded Levenberg–Marquardt least-squares solver with
// a finite-difference Jacobian. Residuals are split into blocks so that a
// parameter that only affects one block (one side of focus, say) costs one
// block evaluation per Jacobian column.
package fit

import (
	"errors"
	"math"
	"sync"
)

// Problem describes a least-squares problem.
type Problem struct {
	// Blocks gives the length of each residual block.
	Blocks []int
	// Eval fills r (length Blocks[b]) with the residuals of block b at p.
	// It must be safe for concurrent use.
	Eval func(p []float64, b int, r []float64)
	// Affects lists the blocks each parameter influences; nil means all.
	Affects [][]int
	// Lower and Upper bound each parameter (use ±Inf for none).
	Lower, Upper []float64
	// Step is the finite-difference step for each parameter.
	Step []float64
	// Fixed parameters are not varied.
	Fixed []bool
}

// Result is the outcome of a fit.
type Result struct {
	P      []float64
	Cost   float64   // sum of squared residuals
	Sigma  []float64 // 1σ formal uncertainties (0 for fixed parameters)
	Iter   int
	Status string
}

// Options controls the solver.
type Options struct {
	MaxIter int     // default 60
	Tol     float64 // relative cost change to stop, default 1e-7
}

// Solve minimises the sum of squared residuals starting from p0.
func Solve(pr Problem, p0 []float64, opt Options) (Result, error) {
	if opt.MaxIter == 0 {
		opt.MaxIter = 60
	}
	if opt.Tol == 0 {
		opt.Tol = 1e-7
	}
	np := len(p0)
	if len(pr.Lower) != np || len(pr.Upper) != np || len(pr.Step) != np {
		return Result{}, errors.New("fit: bounds/step length mismatch")
	}
	fixed := pr.Fixed
	if fixed == nil {
		fixed = make([]bool, np)
	}
	free := []int{}
	for j := range np {
		if !fixed[j] {
			free = append(free, j)
		}
	}
	nb := len(pr.Blocks)
	off := make([]int, nb+1)
	for b, n := range pr.Blocks {
		off[b+1] = off[b] + n
	}
	m := off[nb]
	affects := make([][]int, np)
	for j := range np {
		if pr.Affects != nil && pr.Affects[j] != nil {
			affects[j] = pr.Affects[j]
		} else {
			for b := range nb {
				affects[j] = append(affects[j], b)
			}
		}
	}

	p := clampAll(append([]float64(nil), p0...), pr.Lower, pr.Upper)
	r := make([]float64, m)
	evalAll(pr, p, r, off)
	cost := sumsq(r)
	nf := len(free)
	J := make([][]float64, nf) // column-major: J[k] is column for free[k]
	for k := range J {
		J[k] = make([]float64, m)
	}
	lambda := 1e-3
	res := Result{Status: "max iterations"}
	pt := make([]float64, np)
	rt := make([]float64, m)
	var A []float64
	for it := 0; it < opt.MaxIter; it++ {
		res.Iter = it + 1
		jacobian(pr, p, r, off, free, affects, J)
		A = make([]float64, nf*nf)
		g := make([]float64, nf)
		for a := range nf {
			for b := a; b < nf; b++ {
				A[a*nf+b] = dotSparse(J[a], J[b], affects[free[a]], affects[free[b]], off)
				A[b*nf+a] = A[a*nf+b]
			}
			g[a] = dot(J[a], r)
		}
		improved := false
		for tries := 0; tries < 12; tries++ {
			M := make([]float64, nf*nf)
			copy(M, A)
			for a := range nf {
				d := A[a*nf+a]
				if d == 0 {
					d = 1e-12
				}
				M[a*nf+a] += lambda * d
			}
			delta, ok := cholSolve(M, g, nf)
			if !ok {
				lambda *= 10
				continue
			}
			copy(pt, p)
			for k, j := range free {
				pt[j] -= delta[k]
			}
			clampAll(pt, pr.Lower, pr.Upper)
			evalAll(pr, pt, rt, off)
			ct := sumsq(rt)
			if ct < cost {
				rel := (cost - ct) / math.Max(cost, 1e-300)
				copy(p, pt)
				copy(r, rt)
				cost = ct
				lambda = math.Max(lambda/3, 1e-9)
				improved = true
				if rel < opt.Tol {
					res.Status = "converged"
					it = opt.MaxIter
				}
				break
			}
			lambda *= 4
		}
		if !improved {
			res.Status = "converged (no further improvement)"
			break
		}
	}
	res.P = p
	res.Cost = cost
	res.Sigma = make([]float64, np)
	// Formal errors from the inverse of JᵀJ at the solution.
	if A != nil && m > nf {
		s2 := cost / float64(m-nf)
		if inv, ok := invert(A, nf); ok {
			for k, j := range free {
				if v := inv[k*nf+k] * s2; v > 0 {
					res.Sigma[j] = math.Sqrt(v)
				}
			}
		}
	}
	return res, nil
}

func evalAll(pr Problem, p, r []float64, off []int) {
	var wg sync.WaitGroup
	for b := range pr.Blocks {
		wg.Add(1)
		go func() {
			defer wg.Done()
			pr.Eval(p, b, r[off[b]:off[b+1]])
		}()
	}
	wg.Wait()
}

func jacobian(pr Problem, p, r []float64, off []int, free []int, affects [][]int, J [][]float64) {
	var wg sync.WaitGroup
	sem := make(chan struct{}, 16)
	for k, j := range free {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			col := J[k]
			for i := range col {
				col[i] = 0
			}
			pp := append([]float64(nil), p...)
			h := pr.Step[j]
			// Step away from a bound if necessary.
			if pp[j]+h > pr.Upper[j] {
				h = -h
			}
			pp[j] += h
			for _, b := range affects[j] {
				seg := col[off[b]:off[b+1]]
				pr.Eval(pp, b, seg)
				base := r[off[b]:off[b+1]]
				for i := range seg {
					seg[i] = (seg[i] - base[i]) / h
				}
			}
		}()
	}
	wg.Wait()
}

func dotSparse(a, b []float64, ba, bb []int, off []int) float64 {
	var s float64
	for _, x := range ba {
		for _, y := range bb {
			if x == y {
				s += dot(a[off[x]:off[x+1]], b[off[x]:off[x+1]])
			}
		}
	}
	return s
}

func dot(a, b []float64) float64 {
	var s float64
	for i := range a {
		s += a[i] * b[i]
	}
	return s
}

func sumsq(r []float64) float64 { return dot(r, r) }

func clampAll(p, lo, hi []float64) []float64 {
	for i := range p {
		p[i] = math.Max(lo[i], math.Min(hi[i], p[i]))
	}
	return p
}

// cholSolve solves M x = g for symmetric positive-definite M (n×n).
func cholSolve(M, g []float64, n int) ([]float64, bool) {
	L := make([]float64, n*n)
	for i := range n {
		for j := 0; j <= i; j++ {
			s := M[i*n+j]
			for k := range j {
				s -= L[i*n+k] * L[j*n+k]
			}
			if i == j {
				if s <= 0 {
					return nil, false
				}
				L[i*n+i] = math.Sqrt(s)
			} else {
				L[i*n+j] = s / L[j*n+j]
			}
		}
	}
	y := make([]float64, n)
	for i := range n {
		s := g[i]
		for k := range i {
			s -= L[i*n+k] * y[k]
		}
		y[i] = s / L[i*n+i]
	}
	x := make([]float64, n)
	for i := n - 1; i >= 0; i-- {
		s := y[i]
		for k := i + 1; k < n; k++ {
			s -= L[k*n+i] * x[k]
		}
		x[i] = s / L[i*n+i]
	}
	return x, true
}

// invert returns the inverse of a symmetric positive-definite matrix.
func invert(A []float64, n int) ([]float64, bool) {
	inv := make([]float64, n*n)
	e := make([]float64, n)
	for c := range n {
		for i := range e {
			e[i] = 0
		}
		e[c] = 1
		x, ok := cholSolve(A, e, n)
		if !ok {
			return nil, false
		}
		for i := range n {
			inv[i*n+c] = x[i]
		}
	}
	return inv, true
}
