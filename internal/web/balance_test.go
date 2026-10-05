package web

import (
	"strings"
	"testing"

	"github.com/exploded/collimation/internal/analysis"
	"github.com/exploded/collimation/internal/db"
)

// The 6 Oct reading under thin cloud: 2.7 mrad of tilt puts the corners
// outside ±23 µm, and the noisy pupil fit is flagged.
func TestBalanceViewCloudyNight(t *testing.T) {
	m := db.Measurement{TiltX: -2.65, TiltY: 0.45, TiltErr: 0.5, StepUm: 3.29,
		ShadowXMm: -9.9, ShadowYMm: 1.46, HubXMm: -8.48, HubYMm: 3.21, PupilErrMm: 3.6, IntraHigh: -1, PupilRough: 1}
	b := balanceView(m, nil, analysis.DefaultConfig())
	if b.Level != "bad" || !strings.Contains(b.Verdict, "±23 µm") {
		t.Errorf("level %q, verdict %q: want outside the ±23 µm zone", b.Level, b.Verdict)
	}
	if !b.PupilRough || b.TiltRough {
		t.Errorf("pupil rough %v, tilt rough %v: want true, false", b.PupilRough, b.TiltRough)
	}
	// 1.2 mrad: corners at 17 µm, inside the zone.
	m.TiltX, m.TiltY, m.TiltErr = 1.2, 0, 0.2
	if b := balanceView(m, nil, analysis.DefaultConfig()); b.Level != "soft" {
		t.Errorf("1.2 mrad: level %q, want soft", b.Level)
	}
}
