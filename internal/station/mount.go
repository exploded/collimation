package station

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/exploded/collimation/internal/nina"
)

// Park parks the mount.
func (s *Station) Park() error {
	return s.mountJob("Parking the mount", "Parking", func(ctx context.Context, c *nina.Client) error {
		return c.Park(ctx)
	})
}

// Unpark unparks the mount.
func (s *Station) Unpark() error {
	return s.mountJob("Unparking the mount", "Unparking", func(ctx context.Context, c *nina.Client) error {
		return c.Unpark(ctx)
	})
}

// Home sends the mount to its home position.
func (s *Station) Home() error {
	return s.mountJob("Homing the mount", "Moving to home", func(ctx context.Context, c *nina.Client) error {
		return c.Home(ctx)
	})
}

func (s *Station) mountJob(title, step string, fn func(context.Context, *nina.Client) error) error {
	return s.start(KindSlew, title, func(ctx context.Context, p progress) error {
		set, err := s.settings(ctx)
		if err != nil {
			return err
		}
		c, err := s.connect(ctx, p, set)
		if err != nil {
			return err
		}
		m, err := c.Mount(ctx)
		if err != nil {
			return err
		}
		if !m.Connected {
			return errors.New("the mount is not connected in N.I.N.A.")
		}
		p.Step("%s", step)
		if err := fn(ctx, c); err != nil {
			return err
		}
		s.mu.Lock()
		s.slewed = true
		s.status = nil // refresh the header light now
		s.mu.Unlock()
		return nil
	})
}

// Device is one piece of equipment in the status light.
type Device struct {
	Name      string // "Camera"
	Connected bool
	Detail    string // "at 3870", "parked"
}

// NinaStatus is a quick read-only check of N.I.N.A. and its equipment.
type NinaStatus struct {
	Reachable bool
	Err       string
	Version   string
	Devices   []Device
	AtPark    bool
	CanHome   bool
	Checked   time.Time
}

// OK reports whether everything the station needs is connected.
func (n NinaStatus) OK() bool {
	if !n.Reachable {
		return false
	}
	for _, d := range n.Devices {
		if d.Name != "Guider" && d.Name != "Filter wheel" && !d.Connected {
			return false
		}
	}
	return true
}

// Summary is a short line for the header.
func (n NinaStatus) Summary() string {
	if !n.Reachable {
		return "not reachable"
	}
	var off []string
	for _, d := range n.Devices {
		if !d.Connected && d.Name != "Guider" && d.Name != "Filter wheel" {
			off = append(off, strings.ToLower(d.Name))
		}
	}
	switch {
	case len(off) > 0:
		return strings.Join(off, ", ") + " not connected"
	case n.AtPark:
		return "connected · mount parked"
	}
	return "connected"
}

// CachedStatus returns the last status without contacting N.I.N.A.
func (s *Station) CachedStatus() (NinaStatus, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.status == nil {
		return NinaStatus{}, false
	}
	return *s.status, true
}

// Status returns the N.I.N.A. status, re-checking at most every 5 s.
func (s *Station) Status(ctx context.Context) NinaStatus {
	s.mu.Lock()
	if s.status != nil && time.Since(s.status.Checked) < 5*time.Second {
		st := *s.status
		s.mu.Unlock()
		return st
	}
	s.mu.Unlock()

	st := s.checkStatus(ctx)
	s.mu.Lock()
	s.status = &st
	s.mu.Unlock()
	return st
}

func (s *Station) checkStatus(ctx context.Context) NinaStatus {
	st := NinaStatus{Checked: time.Now()}
	set, err := s.settings(ctx)
	if err != nil {
		st.Err = err.Error()
		return st
	}
	ctx, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()
	c := s.NewClient(set)
	v, err := c.Version(ctx)
	if err != nil {
		st.Err = fmt.Sprintf("N.I.N.A. at %s:%d: %v", set.NinaHost, set.NinaPort, err)
		return st
	}
	st.Reachable, st.Version = true, v

	cam := Device{Name: "Camera"}
	if ci, err := c.Camera(ctx); err == nil && ci.Connected {
		cam.Connected = true
		cam.Detail = fmt.Sprintf("%.0f °C", ci.Temperature)
		if ci.IsExposing {
			cam.Detail += ", exposing"
		}
	}
	foc := Device{Name: "Focuser"}
	if fi, err := c.Focuser(ctx); err == nil && fi.Connected {
		foc.Connected = true
		foc.Detail = fmt.Sprintf("at %d", fi.Position)
	}
	mnt := Device{Name: "Mount"}
	if mi, err := c.Mount(ctx); err == nil && mi.Connected {
		mnt.Connected = true
		st.AtPark, st.CanHome = mi.AtPark, mi.CanFindHome
		switch {
		case mi.AtPark:
			mnt.Detail = "parked"
		case mi.Slewing:
			mnt.Detail = "slewing"
		case !mi.TrackingEnabled:
			mnt.Detail = fmt.Sprintf("alt %.0f°, not tracking", mi.Altitude)
		default:
			mnt.Detail = fmt.Sprintf("alt %.0f°, tracking", mi.Altitude)
		}
	}
	fw := Device{Name: "Filter wheel"}
	if fi, err := c.FilterWheel(ctx); err == nil && fi.Connected {
		fw.Connected = true
		if fi.SelectedFilter != nil {
			fw.Detail = fi.SelectedFilter.Name
		}
	}
	gd := Device{Name: "Guider"}
	if gi, err := c.Guider(ctx); err == nil && gi.Connected {
		gd.Connected = true
		gd.Detail = strings.ToLower(gi.State)
	}
	st.Devices = []Device{cam, foc, mnt, fw, gd}
	return st
}
