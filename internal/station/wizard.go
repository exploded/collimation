package station

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/exploded/collimation/internal/analysis"
	"github.com/exploded/collimation/internal/collim"
)

// Wizard is the screw-calibration walkthrough. Stage counts the measurements
// taken: 0 = baseline needed, 1 = after screw A, 2 = after B, 3 = after C.
type Wizard struct {
	Active bool
	Stage  int
	IDs    [4]int64
	// From is the measurement each screw's effect is measured from when it
	// was turned again after a rejected step (0 = the previous step's), so a
	// turn that the lock swallowed is not counted twice.
	From  [3]int64
	Saved bool // a calibration has been saved from this wizard
}

// Retrying reports whether the current screw is being turned again.
func (w Wizard) Retrying() bool {
	return w.Stage >= 1 && w.Stage <= 3 && w.From[w.Stage-1] != 0
}

func (w *Wizard) from(screw int) int64 {
	if w.From[screw] != 0 {
		return w.From[screw]
	}
	return w.IDs[screw]
}

// Wizard returns a copy of the wizard state.
func (s *Station) Wizard() Wizard {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.wiz == nil {
		return Wizard{}
	}
	return *s.wiz
}

// WizardStart begins a calibration, discarding any pending suggestion.
func (s *Station) WizardStart(ctx context.Context) error {
	if j := s.Job(); j.Running() {
		return ErrBusy
	}
	if err := s.Q.SkipPendingAdjustments(ctx); err != nil {
		return err
	}
	s.mu.Lock()
	s.wiz = &Wizard{Active: true}
	s.mu.Unlock()
	return nil
}

// WizardCancel abandons the walkthrough (a saved calibration is kept). A
// suggestion made after A and B is dropped, since C may have been turned.
func (s *Station) WizardCancel() {
	s.mu.Lock()
	w := s.wiz
	s.wiz = nil
	s.mu.Unlock()
	if w != nil && w.Active && w.Saved {
		if err := s.Q.SkipPendingAdjustments(context.Background()); err != nil {
			log.Printf("calibrate: %v", err)
		}
	}
}

// WizardMeasure takes the next calibration measurement.
func (s *Station) WizardMeasure() error {
	s.mu.Lock()
	w := s.wiz
	s.mu.Unlock()
	if w == nil || !w.Active {
		return errors.New("start the calibration first")
	}
	if w.Stage >= 4 {
		return errors.New("calibration already complete")
	}
	titles := []string{"Calibration: baseline", "Calibration: after screw A", "Calibration: after screw B", "Calibration: after screw C"}
	return s.start(KindCalibrate, titles[w.Stage], func(ctx context.Context, p progress) error {
		id, _, err := s.measureBoth(ctx, p, KindCalibrate)
		if err != nil {
			return err
		}
		p.setResult(id)
		s.mu.Lock()
		if s.wiz != w {
			s.mu.Unlock()
			return nil // cancelled meanwhile
		}
		w.IDs[w.Stage] = id
		w.Stage++
		stage := w.Stage
		s.mu.Unlock()
		if stage >= 3 {
			p.Step("Saving the calibration")
			if err := s.wizardSave(ctx, w, stage); err != nil {
				// Stay on this screw so it can be turned again, measured from
				// this reading.
				s.mu.Lock()
				w.Stage--
				w.From[w.Stage-1] = w.IDs[w.Stage]
				w.IDs[w.Stage] = 0
				s.mu.Unlock()
				return err
			}
			s.mu.Lock()
			w.Saved = true
			if stage == 4 {
				w.Active = false
			}
			s.mu.Unlock()
			// The last measurement is a good starting point: suggest the
			// first correction now rather than after another measurement.
			p.Step("Working out the first adjustment")
			if err := s.Q.SkipPendingAdjustments(ctx); err != nil {
				return err
			}
			cur, err := s.Q.GetMeasurement(ctx, id)
			if err != nil {
				return err
			}
			cal, haveCal, err := s.Calibration(ctx)
			if err != nil {
				return err
			}
			return s.suggest(ctx, cur, cal, haveCal)
		}
		return nil
	})
}

// wizardSave works out the per-⅛-turn effects from consecutive measurements.
// Where the stars can be matched, each screw's coma change comes from its
// star shift, which is far more precise than the coma difference.
func (s *Station) wizardSave(ctx context.Context, w *Wizard, stage int) error {
	set, err := s.settings(ctx)
	if err != nil {
		return err
	}
	k := analysis.ComaPerShift(s.Cfg)
	var cal collim.Calibration
	for i := 0; i < stage-1; i++ {
		a, err := s.Q.GetMeasurement(ctx, w.from(i))
		if err != nil {
			return err
		}
		b, err := s.Q.GetMeasurement(ctx, w.IDs[i+1])
		if err != nil {
			return err
		}
		dc := collim.Vec{X: b.ComaX - a.ComaX, Y: b.ComaY - a.ComaY}
		if sh, ok := Shift(a, b); ok {
			cal.Shift[i] = sh.Scale(1 / set.CalTurn)
			if collim.Agrees(dc, sh, k) {
				dc = collim.FromShift(sh, k)
			}
		}
		cal.Coma[i] = dc.Scale(1 / set.CalTurn)
	}
	cal.CMeasured = stage == 4
	if !cal.Valid() {
		return fmt.Errorf("screws A and B did not move the mirror in clearly different directions. "+
			"Back screw B's lock well off, turn B another %s clockwise and measure again; "+
			"I'll measure B from this reading, so only the new turn counts", collim.FormatTurn(set.CalTurn))
	}
	return s.saveCalibration(ctx, cal, "wizard")
}

// WizardFinish ends the walkthrough after A and B (C derived).
func (s *Station) WizardFinish() {
	s.mu.Lock()
	if s.wiz != nil && s.wiz.Saved {
		s.wiz.Active = false
	}
	s.mu.Unlock()
}

func fitsFiles(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range entries {
		n := strings.ToLower(e.Name())
		if !e.IsDir() && (strings.HasSuffix(n, ".fits") || strings.HasSuffix(n, ".fit") || strings.HasSuffix(n, ".fts")) {
			out = append(out, filepath.Join(dir, e.Name()))
		}
	}
	slices.Sort(out)
	return out, nil
}
