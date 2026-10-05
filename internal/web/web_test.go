package web

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/exploded/collimation/internal/analysis"
	"github.com/exploded/collimation/internal/collim"
	"github.com/exploded/collimation/internal/db"
	"github.com/exploded/collimation/internal/station"
)

// TestPagesRender renders every page and fragment in the main states, so a
// template error fails the build rather than showing up at the telescope.
func TestPagesRender(t *testing.T) {
	conn, err := db.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	st := station.New(conn, analysis.DefaultConfig())
	srv, err := New(st)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv)
	defer ts.Close()

	get := func(path string, htmx bool) string {
		t.Helper()
		req, _ := http.NewRequest("GET", ts.URL+path, nil)
		if htmx {
			req.Header.Set("HX-Request", "true")
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != 200 {
			t.Errorf("GET %s (htmx=%v): %d %s", path, htmx, resp.StatusCode, b)
		}
		return string(b)
	}
	// Point at a closed port so the status light never contacts a real N.I.N.A.
	set := station.DefaultSettings()
	set.NinaPort = 1
	if err := set.Save(context.Background(), st.Q); err != nil {
		t.Fatal(err)
	}
	if body := get("/nina/status", true); !strings.Contains(body, "not reachable") {
		t.Errorf("status light: %s", body)
	}
	pages := []string{"/", "/station", "/calibrate", "/calibrate/panel", "/analyse", "/history", "/setup"}
	check := func(state string) {
		for _, p := range pages {
			for _, h := range []bool{false, true} {
				if body := get(p, h); strings.Contains(body, "template error") {
					t.Errorf("%s: %s rendered a template error", state, p)
				}
			}
		}
	}
	check("empty")

	// A measurement with a calibration and a pending adjustment.
	ctx := context.Background()
	q := st.Q
	now := time.Now().Format(time.RFC3339)
	for _, c := range []float64{-2.8, -1.2} {
		err := q.CreateMeasurement(ctx, db.CreateMeasurementParams{CreatedAt: now, Kind: "measure",
			ComaX: c, ComaY: 2.6, ComaErr: 0.1, AxisXMm: -c, AxisYMm: -2.6, DecentreMm: 3.9, SeeingPx: 3.2,
			Obstruction: 0.37, Stars: 120, Frames: 4, Files: "a.fits\nb.fits"})
		if err != nil {
			t.Fatal(err)
		}
	}
	id, _ := q.GetLastMeasurementID(ctx)
	cal := collim.Calibration{Coma: [3]collim.Vec{{X: 1, Y: 0}, {X: -0.5, Y: 0.87}}}
	cal.DeriveC()
	if err := q.CreateCalibration(ctx, db.CreateCalibrationParams{CreatedAt: now, Source: "test",
		ACx: 1, BCx: -0.5, BCy: 0.87, CCx: -0.5, CCy: -0.87}); err != nil {
		t.Fatal(err)
	}
	if err := q.CreateAdjustment(ctx, db.CreateAdjustmentParams{CreatedAt: now, BeforeID: id,
		TurnsA: 2, TurnsB: -1.5, PredDcx: 1.5, PredDcy: -1.3}); err != nil {
		t.Fatal(err)
	}
	check("pending")
	body := get("/", false)
	for _, want := range []string{"Next adjustment", "¼ turn", "anticlockwise", "Done – measure again", "Slew to Collimation Field", "Park", "nina-light"} {
		if !strings.Contains(body, want) {
			t.Errorf("collimate page missing %q", want)
		}
	}
	get("/result/1", false)

	// Calibration wizard in progress.
	if err := st.WizardStart(ctx); err != nil {
		t.Fatal(err)
	}
	check("wizard")
}
