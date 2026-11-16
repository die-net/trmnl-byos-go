package deviceapi

import (
	"fmt"
	"io"
	"net/http"

	"github.com/die-net/trmnl-byos-go/internal/profile"
	"github.com/die-net/trmnl-byos-go/internal/screen"
)

// maxScreens is how many system screens screens remembers. Past it the table
// is emptied, which costs one redraw per screen in use, and is simpler than
// tracking which entries were used last.
const maxScreens = 256

// tooLargeRefresh is how long a device showing the "too large" screen
// sleeps, so it picks up a fix soon.
const tooLargeRefresh = 300

// tooLarge renders the screen a device gets instead of an image over its
// size limit.
func (s *Server) tooLarge(sp screen.Profile, size, limit int64) (string, error) {
	return s.screen(sp, []screen.Line{
		{Text: "Image too large", Size: 44, Bold: true},
		{Text: fmt.Sprintf("This screen is %d KB; this display takes at most %d KB.", (size+1023)/1024, limit/1024), Size: 20},
		{Text: "Try a palette with fewer grays for it on the server.", Size: 18},
	})
}

// blockedRefresh is how long a blocked device sleeps, in seconds: a day, so
// it costs one request a day and still notices an unblock.
const blockedRefresh = 24 * 3600

// blockedScreen renders the screen a blocked MAC gets, from /api/display
// when display is true and from /api/setup otherwise. The text is constant
// for a MAC, so the image name is too, and the device does not repaint.
func (s *Server) blockedScreen(sp screen.Profile, mac string, display bool) (string, error) {
	retry := "This display tries again every 5 minutes."
	if display {
		retry = "This display checks again once a day."
	}
	return s.screen(sp, []screen.Line{
		{Text: "Blocked", Size: 44, Bold: true},
		{Text: "This device is blocked by its server.", Size: 22},
		{Text: mac, Size: 28},
		{Text: retry, Size: 18},
	})
}

// screenProfile is what system screens are drawn for on a device: its size
// and its richest gray palette, with the palette's measured light if known.
func screenProfile(p profile.Profile) screen.Profile {
	levels := p.GrayLevels()
	sp := screen.Profile{Width: p.Width, Height: p.Height, Levels: levels}
	if len(p.Light) == levels {
		sp.Light = p.Light
	}
	return sp
}

// systemScreen resolves the requesting device's profile from its headers
// and draws a system screen for it with draw.
func systemScreen(r *http.Request, draw func(screen.Profile) (string, error)) (string, error) {
	p := profile.Resolve(r.Header.Get("Model"), headerInt(r, "Width"), headerInt(r, "Height"), profile.Override{})
	return draw(screenProfile(p))
}

// screen renders a system screen of text lines, stores it, and returns its
// image name.
func (s *Server) screen(p screen.Profile, lines []screen.Line) (string, error) {
	name, _, err := s.screenSized(p, lines)
	return name, err
}

// screenSized is screen, also returning the image's size in bytes.
func (s *Server) screenSized(p screen.Profile, lines []screen.Line) (string, int64, error) {
	return s.cached(fmt.Sprintf("lines %+v %+v", p, lines), func(w io.Writer) error { return screen.Render(w, p, lines) })
}

// cached returns the name and size of the image draw writes for key. It
// draws the image straight into the image store the first time key is seen,
// and again only if the stored image is gone. Using it marks it used, so
// image expiry keeps it.
func (s *Server) cached(key string, draw func(io.Writer) error) (string, int64, error) {
	s.mu.Lock()
	name, ok := s.screens[key]
	s.mu.Unlock()
	if ok {
		size, stored, err := s.images.Refresh(name)
		if err != nil || stored {
			return name, size, err
		}
	}
	w, err := s.images.Create()
	if err != nil {
		return "", 0, err
	}
	if err := draw(w); err != nil {
		w.Abort()
		return "", 0, err
	}
	name, size, err := w.Commit()
	if err != nil {
		return "", 0, err
	}
	s.mu.Lock()
	if len(s.screens) >= maxScreens {
		clear(s.screens)
	}
	s.screens[key] = name
	s.mu.Unlock()
	return name, size, nil
}
