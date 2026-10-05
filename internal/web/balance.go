package web

import (
	"fmt"
	"math"

	"github.com/exploded/collimation/internal/analysis"
	"github.com/exploded/collimation/internal/db"
)

// Tilt tolerance: the critical focus zone 4.88·λ·N² is 46 µm deep in Hα at
// f/3.8, so ±23 µm about best focus. The corners of the ASI2600 (14.1 mm
// half-diagonal) stay inside it below about 1.6 mrad.
const (
	cornerTolUM    = 23
	halfDiagMM     = 14.1
	halfWidthMM    = 6248 * 3.76 / 2000
	halfHeightMM   = 4176 * 3.76 / 2000
	designOffsetMM = 6.35 // 4" secondary at f/4: minor axis / (4·N)
)

// BalanceView says whether the mirrors are fighting each other.
type BalanceView struct {
	// Focal-plane tilt.
	Tilt          bool
	TiltRough     bool    // donuts too small for a reliable tilt
	TiltMag       float64 // mrad
	TiltErr       float64
	CornerUM      float64
	Level         string // ok | soft | bad: alert class
	Verdict       string
	Detail        string
	AcrossX       [2]string // focus change left to right: value, words
	AcrossY       [2]string // top to bottom
	SecondaryMrad float64
	PrimaryMrad   float64

	// Pupil geometry.
	Pupil      bool
	Hub        bool
	HubMM      float64
	HubX       float64
	HubY       float64
	TubeMrad   float64
	ShadowMM   float64
	ShadowX    float64
	ShadowY    float64
	HolderMM   float64
	ErrMM      float64
	Side       string // which side of focus is inside
	SideKnown  bool
	Slope      string // measured against expected shadow drift, if the result is cached
	Design     float64
	PupilRough bool   // too noisy to trust
	Why        string // which test failed, if the result is cached
}

func balanceView(m db.Measurement, res *analysis.Result, cfg analysis.Config) *BalanceView {
	if m.TiltErr <= 0 && m.PupilErrMm <= 0 {
		return nil
	}
	b := &BalanceView{}
	if m.TiltErr > 0 {
		b.Tilt = true
		b.TiltMag = math.Hypot(m.TiltX, m.TiltY)
		b.TiltErr = m.TiltErr
		b.CornerUM = b.TiltMag * halfDiagMM
		f := cfg.PrimaryFRatio * cfg.ApertureMM
		d := cfg.SecondaryToFocusMM
		b.SecondaryMrad = b.TiltMag / (2 * (1 - d/f))
		b.PrimaryMrad = b.TiltMag * d / (f - d)
		b.TiltRough = m.TiltRough != 0
		if res != nil {
			b.CornerUM = res.Tilt.CornerUM
		}
		b.AcrossX = across(m.TiltX, 2*halfWidthMM, m.StepUm, "right", "left")
		b.AcrossY = across(m.TiltY, 2*halfHeightMM, m.StepUm, "bottom", "top")
		switch {
		case b.TiltMag < math.Max(1, 2*m.TiltErr):
			b.Level = "ok"
			b.Verdict = "Focus is flat across the field. The mirrors aren't fighting each other."
			b.Detail = fmt.Sprintf("Any compensation has pulled the primary less than about %.1f mrad from its ideal angle.",
				math.Max(b.PrimaryMrad, 2*m.TiltErr*d/(f-d)))
		case b.CornerUM <= cornerTolUM:
			b.Level = "soft"
			b.Verdict = "Small focal-plane tilt. The corners stay inside the critical focus zone."
			b.Detail = fmt.Sprintf("If the tilt comes from the mirrors, the secondary is about %.1f mrad off and the primary has been pulled about %.1f mrad to compensate. That's small; levelling it is optional.",
				b.SecondaryMrad, b.PrimaryMrad)
		default:
			b.Level = "bad"
			b.Verdict = fmt.Sprintf("The focal plane is tilted: the worst corner is %.0f µm out of focus, outside the ±%d µm critical focus zone.", b.CornerUM, cornerTolUM)
			b.Detail = fmt.Sprintf("If the tilt comes from the mirrors, the secondary is about %.1f mrad off and the primary has been pulled about %.1f mrad to compensate. Tilt the secondary to level the focus, then re-null coma with the primary. That gives a flat field even if the tilt is in the camera or corrector.",
				b.SecondaryMrad, b.PrimaryMrad)
		}
	}
	if m.PupilErrMm > 0 {
		b.Pupil = true
		b.Design = designOffsetMM
		b.ErrMM = m.PupilErrMm
		b.PupilRough = m.PupilRough != 0
		if res != nil {
			b.Why = res.Pupil.Why
		}
		b.ShadowX, b.ShadowY = m.ShadowXMm, m.ShadowYMm
		b.ShadowMM = math.Hypot(m.ShadowXMm, m.ShadowYMm)
		b.Hub = m.HubXMm != 0 || m.HubYMm != 0
		if b.Hub {
			b.HubX, b.HubY = m.HubXMm, m.HubYMm
			b.HubMM = math.Hypot(m.HubXMm, m.HubYMm)
			b.TubeMrad = b.HubMM / cfg.SecondaryHeightMM * 1000
			b.HolderMM = math.Hypot(m.ShadowXMm-m.HubXMm, m.ShadowYMm-m.HubYMm)
		}
		switch m.IntraHigh {
		case 1:
			b.SideKnown = true
			b.Side = "The higher focuser position is inside focus."
		case -1:
			b.SideKnown = true
			b.Side = "The lower focuser position is inside focus."
		default:
			b.Side = "The side of focus wasn't determined, so the signs below assume the higher focuser position is inside focus."
		}
		if res != nil && res.Pupil.OK && res.Pupil.SlopeGeo > 0 {
			g := res.Pupil
			b.Slope = fmt.Sprintf("Across the field the shadow drifts %.2f ± %.2f %% of the pupil radius per mm; geometry predicts %.2f %% (secondary %.0f mm above the primary, against %.0f mm assumed).",
				math.Abs(g.Slope)*100, g.SlopeErr*100, g.SlopeGeo*100, g.HeightMM, cfg.SecondaryHeightMM)
		}
	}
	return b
}

// across describes how best focus changes over a span (mm) of the sensor
// for a tilt (µm per mm toward +): a value (in focuser steps when the scale
// is known) and which edge it's higher at.
func across(tilt, span, stepUM float64, plus, minus string) [2]string {
	um := tilt * span
	side := plus
	if um < 0 {
		side = minus
	}
	um = math.Abs(um)
	if stepUM > 0 {
		return [2]string{fmt.Sprintf("%.0f steps", um/stepUM), fmt.Sprintf("%.0f µm, best focus higher at the %s edge", um, side)}
	}
	return [2]string{fmt.Sprintf("%.0f µm", um), fmt.Sprintf("best focus higher at the %s edge", side)}
}
