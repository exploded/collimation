package nina

import (
	"encoding/json"
	"math"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/exploded/collimation/internal/fits"
)

// Stub is a fake N.I.N.A. Advanced API for testing without a telescope. A
// capture "takes" the stored frame whose FOCPOS is nearest the focuser
// position, cycling through the frames at that position.
type Stub struct {
	mu       sync.Mutex
	dir      string           // the profile's image folder
	frames   map[int][]string // FOCPOS -> files
	next     map[int]int
	focuser  int
	target   int
	movingTo time.Time
	filter   int
	guiding  bool
	history  []HistoryImage
	ra, dec  float64
	parked   bool
	homed    bool
	slewEnd  time.Time
	// PointErr is the pointing error in degrees (RA, Dec) that plate solves
	// report, so centring has something to correct.
	PointErr [2]float64
	// ExposureScale shortens exposures (default 0.1).
	ExposureScale float64
}

// NewStub indexes the FITS files in dir.
func NewStub(dir string) (*Stub, error) {
	dir, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	files, err := filepath.Glob(filepath.Join(dir, "*.fit*"))
	if err != nil {
		return nil, err
	}
	s := &Stub{dir: dir, frames: map[int][]string{}, next: map[int]int{}, guiding: true, ExposureScale: 0.1,
		ra: 274.69, dec: -13.78, PointErr: [2]float64{0.15, -0.08}}
	for _, f := range files {
		h, err := fits.ReadHeader(f)
		if err != nil {
			continue
		}
		p := h.Int("FOCPOS", 0)
		s.frames[p] = append(s.frames[p], f)
		if s.focuser == 0 {
			s.focuser, s.target = p, p
		}
	}
	return s, nil
}

func (s *Stub) reply(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"Response": v, "Error": "", "StatusCode": 200, "Success": true, "Type": "API"})
}

func (s *Stub) fail(w http.ResponseWriter, msg string) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"Response": "", "Error": msg, "StatusCode": 400, "Success": false, "Type": "API"})
}

// ServeHTTP implements the subset of the API that the station uses.
func (s *Stub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p := strings.TrimPrefix(r.URL.Path, "/v2/api")
	q := r.URL.Query()
	s.mu.Lock()
	defer s.mu.Unlock()
	moving := time.Now().Before(s.movingTo)
	if !moving {
		s.focuser = s.target
	}
	filters := []Filter{{"L", 0}, {"R", 1}, {"G", 2}, {"B", 3}, {"Ha", 4}}
	switch p {
	case "/version":
		s.reply(w, "stub-2.2.15")
	case "/equipment/focuser/info":
		s.reply(w, FocuserInfo{Connected: true, Name: "Stub EAF", Position: s.focuser, IsMoving: moving})
	case "/equipment/focuser/move":
		t, err := strconv.Atoi(q.Get("position"))
		if err != nil {
			s.fail(w, "bad position")
			return
		}
		s.target = t
		s.movingTo = time.Now().Add(400 * time.Millisecond)
		s.reply(w, "Move started")
	case "/equipment/filterwheel/info":
		f := filters[s.filter]
		s.reply(w, FilterWheelInfo{Connected: true, SelectedFilter: &f, AvailableFilters: filters})
	case "/equipment/filterwheel/change-filter":
		id, _ := strconv.Atoi(q.Get("filterId"))
		if id < 0 || id >= len(filters) {
			s.fail(w, "no such filter")
			return
		}
		s.filter = id
		s.reply(w, "Filter changed")
	case "/equipment/guider/info":
		st := "Stopped"
		if s.guiding {
			st = "Guiding"
		}
		s.reply(w, GuiderInfo{Connected: true, Name: "Stub PHD2", State: st})
	case "/equipment/guider/stop":
		s.guiding = false
		s.reply(w, "Guiding stopped")
	case "/equipment/mount/info":
		lst := math.Mod(float64(time.Now().Unix())/3590.17, 24)
		s.reply(w, MountInfo{Connected: true, Name: "Stub TheSkyX", SiderealTime: lst, RightAscension: s.ra / 15,
			Declination: s.dec, SiteLatitude: -37.776, SiteLongitude: 145.185, Altitude: 70, Azimuth: 280,
			SideOfPier: "pierEast", TrackingEnabled: !s.parked, AtPark: s.parked, AtHome: s.homed,
			CanFindHome: true, CanPark: true, Slewing: time.Now().Before(s.slewEnd)})
	case "/equipment/mount/park":
		s.parked, s.homed = true, false
		s.reply(w, "Parking")
	case "/equipment/mount/unpark":
		s.parked = false
		s.reply(w, "Unparking")
	case "/equipment/mount/home":
		if s.parked {
			s.fail(w, "Mount parked")
			return
		}
		s.homed = true
		s.reply(w, "Homing")
	case "/equipment/camera/info":
		s.reply(w, CameraInfo{Connected: true, Name: "Stub ASI2600MM Duo", CameraState: "Idle", Temperature: -10, CoolerOn: true})
	case "/equipment/mount/slew":
		if s.parked {
			s.fail(w, "Mount parked")
			return
		}
		s.homed = false
		s.ra, _ = strconv.ParseFloat(q.Get("ra"), 64)
		s.dec, _ = strconv.ParseFloat(q.Get("dec"), 64)
		// Like the plugin without waitForResult: reply at once, then report
		// Slewing for a while.
		s.slewEnd = time.Now().Add(300 * time.Millisecond)
		s.reply(w, "Started Slew")
	case "/equipment/mount/slew/stop":
		s.slewEnd = time.Time{}
		s.reply(w, "Stopped slew")
	case "/equipment/camera/capture":
		exp, _ := strconv.ParseFloat(q.Get("duration"), 64)
		if q.Get("solve") == "true" {
			if exp <= 0 { // like the plugin: a 0 s frame has no stars
				s.reply(w, map[string]any{"PlateSolveResult": SolveResult{}})
				return
			}
			s.reply(w, map[string]any{"PlateSolveResult": SolveResult{Success: true, PixelScale: 0.67,
				Coordinates: Coordinates{RADegrees: s.ra + s.PointErr[0], DECDegrees: s.dec + s.PointErr[1]}}})
			return
		}
		f := s.pick()
		if f == "" {
			s.fail(w, "no frames")
			return
		}
		s.mu.Unlock()
		time.Sleep(time.Duration(exp * s.ExposureScale * float64(time.Second)))
		s.mu.Lock()
		// Like the plugin, report only the file name.
		s.history = append(s.history, HistoryImage{Filename: filepath.Base(f), ExposureTime: exp, Filter: filters[s.filter].Name, Date: time.Now().Format(time.RFC3339)})
		s.reply(w, "Capture finished")
	case "/image-history":
		if q.Get("count") == "true" {
			s.reply(w, len(s.history))
			return
		}
		i, err := strconv.Atoi(q.Get("index"))
		if err != nil || i < 0 || i >= len(s.history) {
			s.fail(w, "index out of range")
			return
		}
		s.reply(w, []HistoryImage{s.history[i]})
	case "/profile/show":
		s.reply(w, map[string]any{"ImageFileSettings": map[string]any{"FilePath": s.dir}})
	default:
		s.fail(w, "stub: not implemented: "+p)
	}
}

// pick returns the next frame at the FOCPOS nearest the focuser.
func (s *Stub) pick() string {
	best, bd := 0, math.MaxInt
	for p := range s.frames {
		if d := abs(p - s.focuser); d < bd {
			best, bd = p, d
		}
	}
	fs := s.frames[best]
	if len(fs) == 0 {
		return ""
	}
	f := fs[s.next[best]%len(fs)]
	s.next[best]++
	return f
}

func abs(i int) int {
	if i < 0 {
		return -i
	}
	return i
}
