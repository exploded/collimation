package fits

import (
	"os"
	"path/filepath"
	"testing"
)

func TestWriteRead(t *testing.T) {
	path := filepath.Join(t.TempDir(), "t.fits")
	w, h := 7, 5
	pix := make([]float32, w*h)
	for i := range pix {
		pix[i] = float32(i)
	}
	err := Write(path, w, h, pix, map[string]any{"FOCPOS": 4375, "FILTER": "L", "EXPTIME": 5.0})
	if err != nil {
		t.Fatal(err)
	}
	im, err := Read(path)
	if err != nil {
		t.Fatal(err)
	}
	if im.W != w || im.H != h {
		t.Fatalf("size %dx%d", im.W, im.H)
	}
	if im.At(3, 2) != float32(2*w+3) {
		t.Errorf("At(3,2) = %v", im.At(3, 2))
	}
	if got := im.Header.Int("FOCPOS", 0); got != 4375 {
		t.Errorf("FOCPOS = %d", got)
	}
	if got := im.Header.String("FILTER"); got != "L" {
		t.Errorf("FILTER = %q", got)
	}
	if got := im.Header.Float("EXPTIME", 0); got != 5 {
		t.Errorf("EXPTIME = %v", got)
	}
}

func TestParseValue(t *testing.T) {
	cases := map[string]string{
		"                 4375 / [step] Focuser position": "4375",
		"'ZWO ASI2600MM Duo'  / Imaging instrument name":  "ZWO ASI2600MM Duo",
		"'it''s'":                         "it's",
		"                    T / C# FITS": "T",
	}
	for in, want := range cases {
		if got := parseValue(in); got != want {
			t.Errorf("parseValue(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestNINAFrame reads one of the real 28 Sep frames if present.
func TestNINAFrame(t *testing.T) {
	path := filepath.Join("..", "..", "images", "2026-09-28_Snapshot", "SNAPSHOT", "2026-09-28_22-40-36_L_-10.00_5.00s_0000.fits")
	if _, err := os.Stat(path); err != nil {
		t.Skip("sample frame not present")
	}
	im, err := Read(path)
	if err != nil {
		t.Fatal(err)
	}
	if im.W != 6248 || im.H != 4176 {
		t.Fatalf("size %dx%d", im.W, im.H)
	}
	if im.Header.Int("FOCPOS", 0) != 4375 {
		t.Errorf("FOCPOS = %d", im.Header.Int("FOCPOS", 0))
	}
	var lo, hi float32 = 1e9, -1e9
	for _, v := range im.Pix {
		lo = min(lo, v)
		hi = max(hi, v)
	}
	if lo < 0 || hi > 65535 {
		t.Errorf("pixel range %v..%v outside 0..65535", lo, hi)
	}
}
