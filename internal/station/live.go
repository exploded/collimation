package station

import (
	"context"
	"math"
	"time"

	"github.com/exploded/collimation/internal/analysis"
	"github.com/exploded/collimation/internal/collim"
)

// Live statuses.
const (
	LiveStarting = "starting"
	LiveSettling = "settling" // waiting for two steady frames
	LiveSteady   = "steady"   // a fresh reading is shown
	LiveMoving   = "moving"   // the stars jumped: an adjustment is under way
	LiveBumped   = "bumped"   // the frame is trailed
	LiveNoStars  = "nostars"
)

// Live is the state of live mode.
type Live struct {
	Running    bool
	Status     string
	Frames     int
	Stars      int
	Shift      collim.Vec // since the previous frame (px)
	HasReading bool
	Coma       collim.Vec // px, mean of the last two steady frames
	AxisX      float64    // mm
	AxisY      float64
	Decentre   float64
	Trail      []collim.Vec // settled axis positions (mm)
	Turns      collim.Turns
	HasTurns   bool
	Updated    time.Time
	// Applied is the fraction of the suggested move the last adjustment made,
	// judged from the star shift.
	Applied    float64
	HasApplied bool

	cfg    analysis.Config
	e0     float64
	steady []analysis.FrameResult
	prev   []analysis.Point
	// The last steady reading and the suggestion shown with it: the
	// starting point of the next adjustment.
	ref liveRef
}

type liveRef struct {
	pos      []analysis.Point
	coma     collim.Vec
	turns    collim.Turns
	hasTurns bool
}

// liveMove is an adjustment seen in live mode: the suggestion that was shown
// and what happened between the steady readings either side of it.
type liveMove struct {
	turns collim.Turns
	coma  collim.Vec // change (px)
	shift collim.Vec // star shift (px)
}

// Live returns a copy of the live state.
func (s *Station) Live() Live {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.live == nil {
		return Live{}
	}
	l := *s.live
	l.Trail = append([]collim.Vec(nil), s.live.Trail...)
	return l
}

// StartLive loops single exposures on one side of focus and updates the
// reading after each frame. Press Stop to end it; the focuser returns to
// focus.
//
// Turns made in live mode are not the ones a pending suggestion asked for, so
// that suggestion is discarded rather than learned from.
func (s *Station) StartLive() error {
	if j := s.Job(); j.Running() {
		return ErrBusy
	}
	if err := s.Q.SkipPendingAdjustments(context.Background()); err != nil {
		return err
	}
	return s.start(KindLive, "Live mode", func(ctx context.Context, p progress) error {
		set, err := s.settings(ctx)
		if err != nil {
			return err
		}
		c, err := s.connect(ctx, p, set)
		if err != nil {
			return err
		}
		focus, err := s.prepare(ctx, p, c, set)
		if err != nil {
			return err
		}
		cal, haveCal, err := s.Calibration(ctx)
		if err != nil {
			return err
		}
		s.mu.Lock()
		s.live = &Live{Running: true, Status: LiveStarting, Updated: time.Now(), cfg: s.Cfg}
		shared := s.shared
		s.mu.Unlock()
		defer func() {
			s.mu.Lock()
			s.live.Running = false
			s.mu.Unlock()
			// Return to focus even though the job was cancelled.
			bctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()
			if err := moveFocuser(bctx, c, focus); err == nil {
				p.Step("Focuser returned to focus (%d)", focus)
			}
		}()

		pos := focus + set.DefocusSteps
		p.Step("Moving focuser to %d", pos)
		if err := moveFocuser(ctx, c, pos); err != nil {
			return err
		}
		cfg := s.Cfg
		var dir string
		for n := 1; ; n++ {
			p.Step("Live frame %d: exposing %.0f s", n, set.LiveExposureS)
			path, err := c.Capture(ctx, set.LiveExposureS, set.Gain)
			if err != nil {
				return err
			}
			if path, err = savedImage(ctx, c, set, path, &dir); err != nil {
				return err
			}
			p.Note("Live frame %d: analysing", n)
			fr, err := analysis.AnalyzeFrame(path, focus, shared, cfg)
			s.mu.Lock()
			mv := s.live.update(fr, err, cal, haveCal, set)
			s.mu.Unlock()
			if mv != nil && haveCal {
				if err := s.learnLive(ctx, &cal, set, *mv); err != nil {
					return err
				}
			}
			if ctx.Err() != nil {
				return ctx.Err()
			}
		}
	})
}

// learnLive judges an adjustment made in live mode and, if it looks like the
// suggested one, refines the calibration and the suggestion now showing.
// The turns are not known, only what was suggested, so a move in a clearly
// different direction (other screws turned) is not learned from.
func (s *Station) learnLive(ctx context.Context, cal *collim.Calibration, set Settings, mv liveMove) error {
	ps := cal.ShiftEffect(mv.turns)
	if ps.Len() < 20 || mv.shift.Len() < 10 { // nothing suggested, or nothing turned
		return nil
	}
	applied := collim.AppliedFraction(mv.shift, ps)
	cos := mv.shift.Dot(ps) / (mv.shift.Len() * ps.Len())
	s.mu.Lock()
	s.live.Applied, s.live.HasApplied = applied, true
	s.mu.Unlock()
	if applied < 0.2 || applied > 3 || cos < 0.5 {
		return nil
	}
	if err := s.learn(ctx, cal, mv.turns, mv.coma, mv.shift, true, "live"); err != nil {
		return err
	}
	s.mu.Lock()
	s.live.suggest(*cal, true, set)
	s.mu.Unlock()
	return nil
}

// update folds a new frame into the live state and returns the adjustment
// made since the last steady reading, if one has just settled. Called with
// s.mu held.
func (l *Live) update(fr *analysis.FrameResult, err error, cal collim.Calibration, haveCal bool, set Settings) *liveMove {
	l.Frames++
	l.Updated = time.Now()
	if err != nil {
		l.Status = LiveNoStars
		l.steady = nil
		return nil
	}
	l.Stars = fr.Stars
	moved := false
	if l.prev != nil {
		dx, dy, _, ok := analysis.MatchShift(l.prev, fr.Positions, 2500)
		l.Shift = collim.Vec{X: dx, Y: dy}
		moved = !ok || l.Shift.Len() > 8
	}
	l.prev = fr.Positions
	if l.e0 == 0 || fr.Ellipticity < l.e0 {
		l.e0 = fr.Ellipticity
	}
	switch {
	case moved:
		l.Status = LiveMoving
		l.steady = nil
		return nil
	case fr.Ellipticity > l.e0+0.05:
		l.Status = LiveBumped
		l.steady = nil
		return nil
	}
	l.steady = append(l.steady, *fr)
	if len(l.steady) > 2 {
		l.steady = l.steady[len(l.steady)-2:]
	}
	if len(l.steady) < 2 {
		l.Status = LiveSettling
		return nil
	}
	a, b := l.steady[0], l.steady[1]
	first := l.Status != LiveSteady
	l.Status = LiveSteady
	l.HasReading = true
	l.Coma = collim.Vec{X: (a.ComaX + b.ComaX) / 2, Y: (a.ComaY + b.ComaY) / 2}
	l.AxisX = (a.AxisXmm + b.AxisXmm) / 2
	l.AxisY = (a.AxisYmm + b.AxisYmm) / 2
	l.Decentre = math.Hypot(l.AxisX, l.AxisY)
	if first {
		l.Trail = append(l.Trail, collim.Vec{X: l.AxisX, Y: l.AxisY})
	} else if n := len(l.Trail); n > 0 {
		l.Trail[n-1] = collim.Vec{X: l.AxisX, Y: l.AxisY}
	}
	var mv *liveMove
	if first && l.ref.hasTurns {
		if dx, dy, _, ok := analysis.MatchShift(l.ref.pos, b.Positions, 2500); ok {
			mv = &liveMove{turns: l.ref.turns, coma: l.Coma.Sub(l.ref.coma), shift: collim.Vec{X: dx, Y: dy}}
		}
	}
	l.ref.pos, l.ref.coma = b.Positions, l.Coma
	l.suggest(cal, haveCal, set)
	return mv
}

// suggest works out the turns for the current reading. Called with s.mu held.
func (l *Live) suggest(cal collim.Calibration, haveCal bool, set Settings) {
	l.HasTurns = false
	if haveCal {
		l.Turns, l.HasTurns = nextTurns(cal, l.Coma, set, l.cfg)
	}
	l.ref.turns, l.ref.hasTurns = l.Turns, l.HasTurns
}
