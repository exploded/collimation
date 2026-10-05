// Package nina is a small client for the N.I.N.A. Advanced API plugin
// (https://github.com/christian-photo/ninaAPI), v2.
package nina

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Client talks to one N.I.N.A. instance.
type Client struct {
	Base string // e.g. http://localhost:1888/v2/api
	HTTP *http.Client
	// Poll is the interval for status polling (default 500 ms).
	Poll time.Duration
}

// New returns a client for host:port.
func New(host string, port int) *Client {
	return &Client{
		Base: fmt.Sprintf("http://%s:%d/v2/api", host, port),
		HTTP: &http.Client{Timeout: 10 * time.Minute},
		Poll: 500 * time.Millisecond,
	}
}

type envelope struct {
	Response   json.RawMessage
	Error      string
	StatusCode int
	Success    bool
}

// APIError is an error reported by the plugin.
type APIError struct {
	Path   string
	Status int
	Msg    string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("N.I.N.A. %s: %s (status %d)", e.Path, e.Msg, e.Status)
}

func (c *Client) get(ctx context.Context, path string, q url.Values, out any) error {
	u := c.Base + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("cannot reach N.I.N.A. at %s: %w", c.Base, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	var env envelope
	if err := json.Unmarshal(body, &env); err != nil {
		return fmt.Errorf("N.I.N.A. %s: unexpected reply (HTTP %d): %.200s", path, resp.StatusCode, body)
	}
	if !env.Success {
		msg := env.Error
		if msg == "" {
			msg = "request failed"
		}
		return &APIError{Path: path, Status: env.StatusCode, Msg: msg}
	}
	if out != nil && len(env.Response) > 0 {
		if err := json.Unmarshal(env.Response, out); err != nil {
			return fmt.Errorf("N.I.N.A. %s: decoding reply: %w", path, err)
		}
	}
	return nil
}

// Version returns the plugin version.
func (c *Client) Version(ctx context.Context) (string, error) {
	var v string
	err := c.get(ctx, "/version", nil, &v)
	return v, err
}

// FocuserInfo is the focuser state.
type FocuserInfo struct {
	Connected   bool
	Name        string
	Position    int
	IsMoving    bool
	IsSettling  bool
	Temperature float64
}

// Focuser returns the focuser state.
func (c *Client) Focuser(ctx context.Context) (FocuserInfo, error) {
	var f FocuserInfo
	err := c.get(ctx, "/equipment/focuser/info", nil, &f)
	return f, err
}

// MoveFocuser moves to pos and waits until the focuser has arrived.
func (c *Client) MoveFocuser(ctx context.Context, pos int) error {
	if err := c.get(ctx, "/equipment/focuser/move", url.Values{"position": {strconv.Itoa(pos)}}, nil); err != nil {
		return err
	}
	deadline := time.Now().Add(3 * time.Minute)
	for {
		if err := sleep(ctx, c.Poll); err != nil {
			return err
		}
		f, err := c.Focuser(ctx)
		if err != nil {
			return err
		}
		if !f.IsMoving && !f.IsSettling && f.Position == pos {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("focuser did not reach %d (at %d)", pos, f.Position)
		}
	}
}

// Filter is one filter wheel slot.
type Filter struct {
	Name string
	Id   int
}

// FilterWheelInfo is the filter wheel state.
type FilterWheelInfo struct {
	Connected        bool
	IsMoving         bool
	SelectedFilter   *Filter
	AvailableFilters []Filter
}

// FilterWheel returns the filter wheel state.
func (c *Client) FilterWheel(ctx context.Context) (FilterWheelInfo, error) {
	var f FilterWheelInfo
	err := c.get(ctx, "/equipment/filterwheel/info", nil, &f)
	return f, err
}

// SetFilter selects the named filter (case-insensitive) and waits for it.
// It does nothing if no filter wheel is connected or name is empty.
func (c *Client) SetFilter(ctx context.Context, name string) error {
	if name == "" {
		return nil
	}
	fw, err := c.FilterWheel(ctx)
	if err != nil {
		return err
	}
	if !fw.Connected {
		return nil
	}
	if fw.SelectedFilter != nil && strings.EqualFold(fw.SelectedFilter.Name, name) {
		return nil
	}
	id := -1
	var names []string
	for _, f := range fw.AvailableFilters {
		names = append(names, f.Name)
		if strings.EqualFold(f.Name, name) {
			id = f.Id
		}
	}
	if id < 0 {
		return fmt.Errorf("filter %q not found (have %s)", name, strings.Join(names, ", "))
	}
	if err := c.get(ctx, "/equipment/filterwheel/change-filter", url.Values{"filterId": {strconv.Itoa(id)}}, nil); err != nil {
		return err
	}
	deadline := time.Now().Add(2 * time.Minute)
	for {
		if err := sleep(ctx, c.Poll); err != nil {
			return err
		}
		fw, err := c.FilterWheel(ctx)
		if err != nil {
			return err
		}
		if !fw.IsMoving && fw.SelectedFilter != nil && fw.SelectedFilter.Id == id {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("filter wheel did not reach %q", name)
		}
	}
}

// GuiderInfo is the guider state.
type GuiderInfo struct {
	Connected bool
	Name      string
	State     string
}

// Guider returns the guider state.
func (c *Client) Guider(ctx context.Context) (GuiderInfo, error) {
	var g GuiderInfo
	err := c.get(ctx, "/equipment/guider/info", nil, &g)
	return g, err
}

// StopGuiding stops guiding if the guider is connected and active. It
// reports whether guiding was running.
func (c *Client) StopGuiding(ctx context.Context) (bool, error) {
	g, err := c.Guider(ctx)
	if err != nil || !g.Connected {
		return false, err
	}
	switch strings.ToLower(g.State) {
	case "", "stopped", "idle", "connected", "looping":
		return false, nil
	}
	return true, c.get(ctx, "/equipment/guider/stop", nil, nil)
}

// MountInfo is the mount state.
type MountInfo struct {
	Connected       bool
	Name            string
	SiderealTime    float64 // hours
	RightAscension  float64 // hours
	Declination     float64 // degrees
	SiteLatitude    float64
	SiteLongitude   float64
	Altitude        float64
	Azimuth         float64
	SideOfPier      string
	AtPark          bool
	AtHome          bool
	CanFindHome     bool
	CanPark         bool
	Slewing         bool
	TrackingEnabled bool
}

// Mount returns the mount state.
func (c *Client) Mount(ctx context.Context) (MountInfo, error) {
	var m MountInfo
	err := c.get(ctx, "/equipment/mount/info", nil, &m)
	return m, err
}

// Slew slews to RA/Dec (degrees, J2000) and waits until the mount has stopped,
// calling moving (if not nil) with the mount state while it slews. It does
// not centre: the plugin's slew-and-centre reports nothing until it ends, and
// can wait forever. Use Solve to centre. Cancelling ctx stops the slew.
func (c *Client) Slew(ctx context.Context, raDeg, decDeg float64, moving func(MountInfo)) error {
	q := url.Values{
		"ra":  {strconv.FormatFloat(raDeg, 'f', 6, 64)},
		"dec": {strconv.FormatFloat(decDeg, 'f', 6, 64)},
	}
	var reply json.RawMessage
	if err := c.get(ctx, "/equipment/mount/slew", q, &reply); err != nil {
		return err
	}
	// Failures still come back with Success true.
	if strings.Contains(strings.ToLower(string(reply)), "fail") {
		return fmt.Errorf("N.I.N.A. reported %s", reply)
	}
	start := time.Now()
	deadline := start.Add(5 * time.Minute)
	seen, still := false, 0
	for {
		if err := sleep(ctx, c.Poll); err != nil {
			c.StopSlew()
			return err
		}
		m, err := c.Mount(ctx)
		if err != nil {
			return err
		}
		if m.AtPark {
			return errors.New("the mount is parked")
		}
		if m.Slewing {
			seen, still = true, 0
			if moving != nil {
				moving(m)
			}
		} else {
			still++
			// The mount may not report Slewing straight away, so a stop only
			// counts once it has been seen moving, or after a grace period.
			if still >= 2 && (seen || time.Since(start) > 10*time.Second) {
				// A slew past the mount's limits is refused without an
				// error here (TheSkyX shows a dialog), so check where the
				// mount stopped.
				if d := separation(m.RightAscension*15, m.Declination, raDeg, decDeg); d > maxSlewMiss {
					return fmt.Errorf("the mount stopped %.0f° from the target: the slew was refused, probably by a mount limit. Close any TheSkyX error and use Slew to Collimation Field for a fresh field", d)
				}
				return nil
			}
		}
		if time.Now().After(deadline) {
			return errors.New("the mount was still slewing after 5 minutes")
		}
	}
}

// maxSlewMiss is how far (degrees) a finished slew may stop from the target.
// It allows for precession between J2000 and the mount's JNow and for the
// pointing error; a refused slew stays where it was.
const maxSlewMiss = 2.0

// separation is the angle (degrees) between two sky positions in degrees.
func separation(ra1, dec1, ra2, dec2 float64) float64 {
	r := math.Pi / 180
	c := math.Sin(dec1*r)*math.Sin(dec2*r) + math.Cos(dec1*r)*math.Cos(dec2*r)*math.Cos((ra1-ra2)*r)
	return math.Acos(math.Max(-1, math.Min(1, c))) / r
}

// StopSlew asks the mount to stop slewing. It is used when a job is stopped,
// so it does not take the (already cancelled) job context.
func (c *Client) StopSlew() {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = c.get(ctx, "/equipment/mount/slew/stop", nil, nil)
}

// Coordinates is a sky position from a plate solve.
type Coordinates struct {
	RADegrees  float64
	DECDegrees float64
}

// SolveResult is a plate solve of one exposure.
type SolveResult struct {
	Coordinates Coordinates
	PixelScale  float64
	Success     bool
}

// Solve takes an exposure (not saved) and plate-solves it. Stars must be in
// focus. The exposure must be given: without duration the plugin takes a 0 s
// frame rather than using the profile's plate-solve exposure.
func (c *Client) Solve(ctx context.Context, exposure float64, gain int) (SolveResult, error) {
	q := url.Values{
		"duration":      {strconv.FormatFloat(exposure, 'f', -1, 64)},
		"solve":         {"true"},
		"waitForResult": {"true"},
		"omitImage":     {"true"},
	}
	if gain >= 0 {
		q.Set("gain", strconv.Itoa(gain))
	}
	var reply struct{ PlateSolveResult *SolveResult }
	if err := c.get(ctx, "/equipment/camera/capture", q, &reply); err != nil {
		return SolveResult{}, err
	}
	if reply.PlateSolveResult == nil || !reply.PlateSolveResult.Success {
		return SolveResult{}, errors.New("plate solve failed (check focus, and that the plate solver works in N.I.N.A.)")
	}
	return *reply.PlateSolveResult, nil
}

// Park parks the mount and waits until it reports parked.
func (c *Client) Park(ctx context.Context) error {
	if err := c.get(ctx, "/equipment/mount/park", nil, nil); err != nil {
		return err
	}
	return c.waitMount(ctx, "park", func(m MountInfo) bool { return m.AtPark })
}

// Unpark unparks the mount and waits until it reports unparked.
func (c *Client) Unpark(ctx context.Context) error {
	if err := c.get(ctx, "/equipment/mount/unpark", nil, nil); err != nil {
		return err
	}
	return c.waitMount(ctx, "unpark", func(m MountInfo) bool { return !m.AtPark })
}

// Home sends the mount to its home position and waits for it to arrive.
func (c *Client) Home(ctx context.Context) error {
	if err := c.get(ctx, "/equipment/mount/home", nil, nil); err != nil {
		return err
	}
	return c.waitMount(ctx, "home", func(m MountInfo) bool { return m.AtHome && !m.Slewing })
}

func (c *Client) waitMount(ctx context.Context, what string, done func(MountInfo) bool) error {
	deadline := time.Now().Add(5 * time.Minute)
	for {
		if err := sleep(ctx, c.Poll); err != nil {
			return err
		}
		m, err := c.Mount(ctx)
		if err != nil {
			return err
		}
		if done(m) {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("the mount did not finish the %s within 5 minutes", what)
		}
	}
}

// CameraInfo is the camera state.
type CameraInfo struct {
	Connected   bool
	Name        string
	CameraState string
	Temperature float64
	CoolerOn    bool
	IsExposing  bool
}

// Camera returns the camera state.
func (c *Client) Camera(ctx context.Context) (CameraInfo, error) {
	var ci CameraInfo
	err := c.get(ctx, "/equipment/camera/info", nil, &ci)
	return ci, err
}

// ImageCount returns the number of images in N.I.N.A.'s image history.
func (c *Client) ImageCount(ctx context.Context) (int, error) {
	var n int
	err := c.get(ctx, "/image-history", url.Values{"count": {"true"}}, &n)
	return n, err
}

// HistoryImage is one image-history entry.
type HistoryImage struct {
	Filename     string
	ExposureTime float64
	Filter       string
	Date         string
	HFR          float64
	Stars        int
}

// Image returns the image-history entry at index.
func (c *Client) Image(ctx context.Context, index int) (HistoryImage, error) {
	var raw json.RawMessage
	if err := c.get(ctx, "/image-history", url.Values{"index": {strconv.Itoa(index)}}, &raw); err != nil {
		return HistoryImage{}, err
	}
	// The plugin returns either an object or a one-element array.
	var one HistoryImage
	if err := json.Unmarshal(raw, &one); err == nil && one.Filename != "" {
		return one, nil
	}
	var arr []HistoryImage
	if err := json.Unmarshal(raw, &arr); err == nil && len(arr) > 0 {
		return arr[0], nil
	}
	return HistoryImage{}, fmt.Errorf("image-history[%d]: no filename in reply", index)
}

// Capture takes and saves one exposure and returns the saved file's name.
// The plugin reports the name without its folder; see ImageDir and FindFile.
func (c *Client) Capture(ctx context.Context, exposure float64, gain int) (string, error) {
	before, err := c.ImageCount(ctx)
	if err != nil {
		return "", err
	}
	q := url.Values{
		"duration":      {strconv.FormatFloat(exposure, 'f', -1, 64)},
		"save":          {"true"},
		"waitForResult": {"true"},
		"omitImage":     {"true"},
	}
	if gain >= 0 {
		q.Set("gain", strconv.Itoa(gain))
	}
	if err := c.get(ctx, "/equipment/camera/capture", q, nil); err != nil {
		return "", err
	}
	// Saving finishes after the capture call returns; wait for the new
	// history entry and for the file to stop growing.
	deadline := time.Now().Add(time.Minute)
	for {
		n, err := c.ImageCount(ctx)
		if err != nil {
			return "", err
		}
		if n > before {
			img, err := c.Image(ctx, n-1)
			if err == nil && img.Filename != "" {
				return img.Filename, nil
			}
		}
		if time.Now().After(deadline) {
			return "", errors.New("N.I.N.A. did not report the saved image")
		}
		if err := sleep(ctx, c.Poll); err != nil {
			return "", err
		}
	}
}

// ImageDir returns the image folder set in N.I.N.A.'s active profile.
func (c *Client) ImageDir(ctx context.Context) (string, error) {
	var p struct{ ImageFileSettings struct{ FilePath string } }
	if err := c.get(ctx, "/profile/show", url.Values{"active": {"true"}}, &p); err != nil {
		return "", err
	}
	if p.ImageFileSettings.FilePath == "" {
		return "", errors.New("N.I.N.A.'s active profile has no image file path")
	}
	return p.ImageFileSettings.FilePath, nil
}

// FindFile waits for a file called name to appear under root and returns its
// path. The subfolder comes from N.I.N.A.'s file pattern (for example
// date\IMAGETYPE), so it is searched for. dir, if set, is checked first,
// because frames in one run share a folder.
func FindFile(ctx context.Context, root, dir, name string, timeout time.Duration) (string, error) {
	if st, err := os.Stat(root); err != nil || !st.IsDir() {
		return "", fmt.Errorf("N.I.N.A.'s image folder %s is not readable here (if N.I.N.A. runs on another PC, set the image folder mapping on the Setup page)", root)
	}
	deadline := time.Now().Add(timeout)
	for {
		if dir != "" {
			if p := filepath.Join(dir, name); fileExists(p) {
				return p, nil
			}
		}
		if p := findUnder(root, name); p != "" {
			return p, nil
		}
		if time.Now().After(deadline) {
			return "", fmt.Errorf("image file %s not found under %s", name, root)
		}
		if err := sleep(ctx, time.Second); err != nil {
			return "", err
		}
	}
}

func fileExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}

// findUnder returns the first file called name below root, or "".
func findUnder(root, name string) string {
	var found string
	filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // skip unreadable folders
		}
		if !d.IsDir() && strings.EqualFold(d.Name(), name) {
			found = p
			return filepath.SkipAll
		}
		return nil
	})
	return found
}

// WaitForFile waits until path exists and its size is stable.
func WaitForFile(ctx context.Context, path string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	var last int64 = -1
	for {
		if st, err := os.Stat(path); err == nil {
			if st.Size() > 0 && st.Size() == last {
				return nil
			}
			last = st.Size()
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("image file %s not readable (if N.I.N.A. runs on another PC, set the image folder mapping on the Setup page)", path)
		}
		if err := sleep(ctx, 300*time.Millisecond); err != nil {
			return err
		}
	}
}

func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
