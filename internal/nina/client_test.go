package nina

import (
	"context"
	"math"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/exploded/collimation/internal/fits"
)

func newStubClient(t *testing.T) (*Client, *Stub) {
	dir := t.TempDir()
	for i, pos := range []int{3400, 3400, 4400} {
		name := filepath.Join(dir, "f"+string(rune('a'+i))+".fits")
		if err := fits.Write(name, 4, 4, make([]float32, 16), map[string]any{"FOCPOS": pos}); err != nil {
			t.Fatal(err)
		}
	}
	stub, err := NewStub(dir)
	if err != nil {
		t.Fatal(err)
	}
	stub.ExposureScale = 0.01
	srv := httptest.NewServer(stub)
	t.Cleanup(srv.Close)
	c := &Client{Base: srv.URL + "/v2/api", HTTP: srv.Client(), Poll: 20 * time.Millisecond}
	return c, stub
}

// N.I.N.A.'s file pattern puts frames in subfolders such as date\SNAPSHOT.
func TestFindFileInSubfolder(t *testing.T) {
	root := t.TempDir()
	sub := filepath.Join(root, "2026-10-05", "SNAPSHOT")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(sub, "frame.fits")
	if err := os.WriteFile(want, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if got, err := FindFile(ctx, root, "", "frame.fits", time.Second); err != nil || got != want {
		t.Errorf("FindFile = %q, %v; want %q", got, err, want)
	}
	if _, err := FindFile(ctx, root, sub, "missing.fits", 0); err == nil {
		t.Error("expected an error for a missing file")
	}
	if _, err := FindFile(ctx, filepath.Join(root, "nope"), "", "frame.fits", time.Second); err == nil {
		t.Error("expected an error for a missing image folder")
	}
}

func TestClientAgainstStub(t *testing.T) {
	c, _ := newStubClient(t)
	ctx := context.Background()
	if v, err := c.Version(ctx); err != nil || !strings.HasPrefix(v, "stub") {
		t.Fatalf("Version = %q, %v", v, err)
	}
	if err := c.MoveFocuser(ctx, 4400); err != nil {
		t.Fatal(err)
	}
	if f, _ := c.Focuser(ctx); f.Position != 4400 {
		t.Errorf("focuser at %d", f.Position)
	}
	if err := c.SetFilter(ctx, "ha"); err != nil {
		t.Fatal(err)
	}
	if err := c.SetFilter(ctx, "OIII"); err == nil {
		t.Error("expected an error for a missing filter")
	}
	was, err := c.StopGuiding(ctx)
	if err != nil || !was {
		t.Errorf("StopGuiding = %v, %v; want true", was, err)
	}
	if was, _ := c.StopGuiding(ctx); was {
		t.Error("guiding still reported running")
	}
	name, err := c.Capture(ctx, 1, 100)
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(name) != name {
		t.Errorf("Capture = %q; want a bare file name, like the plugin", name)
	}
	root, err := c.ImageDir(ctx)
	if err != nil {
		t.Fatal(err)
	}
	path, err := FindFile(ctx, root, "", name, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if h, _ := fits.ReadHeader(path); h.Int("FOCPOS", 0) != 4400 {
		t.Errorf("captured %s, FOCPOS %d", path, h.Int("FOCPOS", 0))
	}
	if err := WaitForFile(ctx, path, time.Second); err != nil {
		t.Error(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Error(err)
	}
	var moves int
	if err := c.Slew(ctx, 10, -40, func(MountInfo) { moves++ }); err != nil {
		t.Fatal(err)
	}
	if m, _ := c.Mount(ctx); m.Declination != -40 || m.Slewing {
		t.Errorf("after the slew: dec %v, slewing %v", m.Declination, m.Slewing)
	}
	if moves == 0 {
		t.Error("no progress while slewing")
	}
	if _, err := c.Solve(ctx, 0, -1); err == nil {
		t.Error("solve with a 0 s exposure should fail")
	}
	if sol, err := c.Solve(ctx, 5, 100); err != nil || math.Abs(sol.Coordinates.DECDegrees-(-40-0.08)) > 1e-9 {
		t.Errorf("solve: %+v, %v", sol, err)
	}
	if err := c.Home(ctx); err != nil {
		t.Fatal(err)
	}
	if err := c.Park(ctx); err != nil {
		t.Fatal(err)
	}
	if m, _ := c.Mount(ctx); !m.AtPark {
		t.Error("mount not parked")
	}
	if err := c.Slew(ctx, 10, -40, nil); err == nil {
		t.Error("slew while parked should fail")
	}
	if err := c.Unpark(ctx); err != nil {
		t.Fatal(err)
	}
	if ci, err := c.Camera(ctx); err != nil || !ci.Connected {
		t.Errorf("camera %+v, %v", ci, err)
	}
}
