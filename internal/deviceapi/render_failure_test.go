package deviceapi

import (
	"testing"

	"github.com/die-net/trmnl-byos-go/internal/db"
	"github.com/die-net/trmnl-byos-go/internal/profile"
	"github.com/die-net/trmnl-byos-go/internal/screen"
)

// couldNotRenderScreen is the name of the "Could not render" screen the
// test device gets for the instance called name and the failure.
func (ts *testServer) couldNotRenderScreen(t *testing.T, name, failure string) string {
	t.Helper()
	p := profile.Resolve("xiao_epaper_display", nil, nil, profile.Override{})
	img, _, err := ts.srv.screenSized(screenProfile(p), []screen.Line{
		{Text: "Could not render", Size: 44, Bold: true},
		{Text: name, Size: 28},
		{Text: failure, Size: 18},
	})
	if err != nil {
		t.Fatal(err)
	}
	return img
}

// TestDisplayFailedOnNewerSettings checks what a device is shown after
// failed builds since its instance's last good render, which read settings
// at 10. The latest counted failure decides: one of newer settings shows
// "Could not render" with its error, since the images no longer show what
// the instance is set to, and one of only newer data keeps the images. An
// unreachable renderer is not counted, and changes neither what is shown
// nor the error.
func TestDisplayFailedOnNewerSettings(t *testing.T) {
	t.Parallel()
	const (
		settingsErr = "trmnlp: undefined variable"
		dataErr     = "trmnlp: data source said 401"
		outage      = "render request: renderer unavailable: connection refused"
	)
	type failure struct {
		settingsAt, dataAt, counted int64
		err                         string
	}
	for _, tc := range []struct {
		name     string
		failures []failure
		// shown is the error on the Could not render screen, or "" for the
		// last good image.
		shown string
	}{
		{"newer settings", []failure{{20, 0, 1, settingsErr}}, settingsErr},
		{"newer settings and data", []failure{{20, 30, 1, settingsErr}}, settingsErr},
		{"newer data only", []failure{{10, 30, 1, dataErr}}, ""},
		{"newer settings, renderer unreachable", []failure{{20, 0, 0, outage}}, ""},
		{"newer data, then the renderer unreachable for newer settings", []failure{{10, 30, 1, dataErr}, {20, 30, 0, outage}}, ""},
		{"newer settings, then the renderer unreachable", []failure{{20, 0, 1, settingsErr}, {20, 0, 0, outage}}, settingsErr},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ts := newTestServer(t)
			key, first, _ := ts.playlistDevice(t)
			ctx := t.Context()
			img := ts.saveRender(t, first, "")
			// The good render read settings at 10.
			if _, err := ts.db.Writer.ExecContext(ctx, `UPDATE render SET settings_at = 10, rendered_settings_at = 10`); err != nil {
				t.Fatal(err)
			}
			p := profile.Resolve("xiao_epaper_display", nil, nil, profile.Override{})
			for _, f := range tc.failures {
				err := ts.db.Write.SaveRenderFailure(ctx, db.SaveRenderFailureParams{
					InstanceID: first, Width: int64(p.Width), Height: int64(p.Height), Palette: p.RenderPalette(""),
					TimeZone: testZone.String(), Locale: "en", AttemptedAt: ts.now.Unix(), Error: f.err,
					SettingsAt: f.settingsAt, DataAt: f.dataAt, Failures: f.counted,
				})
				if err != nil {
					t.Fatal(err)
				}
			}
			timer := displayHeaders(testMAC, key)
			timer["Update-Source"] = "timer"
			m := ts.getJSON(t, "/api/display", timer)
			want := img
			if tc.shown != "" {
				want = ts.couldNotRenderScreen(t, "Weather", tc.shown)
			}
			if m["filename"] != want {
				t.Errorf("%v; want %s (the image, or Could not render with %q)", m, want, tc.shown)
			}
			if tc.shown != "" && m["refresh_rate"] != float64(retryRefresh) {
				t.Errorf("refresh %v, want the retry interval %d", m["refresh_rate"], retryRefresh)
			}
		})
	}
}

// TestDisplayFailedOnNewerSettingsRecovers checks that a good build after
// a failure of newer settings shows its images again, since it read those
// settings.
func TestDisplayFailedOnNewerSettingsRecovers(t *testing.T) {
	t.Parallel()
	ts := newTestServer(t)
	key, first, _ := ts.playlistDevice(t)
	ctx := t.Context()
	ts.saveRender(t, first, "")
	p := profile.Resolve("xiao_epaper_display", nil, nil, profile.Override{})
	err := ts.db.Write.SaveRenderFailure(ctx, db.SaveRenderFailureParams{
		InstanceID: first, Width: int64(p.Width), Height: int64(p.Height), Palette: p.RenderPalette(""),
		TimeZone: testZone.String(), Locale: "en", AttemptedAt: ts.now.Unix(), Error: "boom", SettingsAt: 20, Failures: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	timer := displayHeaders(testMAC, key)
	timer["Update-Source"] = "timer"
	if m := ts.getJSON(t, "/api/display", timer); m["filename"] != ts.couldNotRenderScreen(t, "Weather", "boom") {
		t.Fatalf("after the failure: %v; want the Could not render screen", m)
	}
	img := ts.saveRender(t, first, "")
	if _, err := ts.db.Writer.ExecContext(ctx, `UPDATE render SET settings_at = 20, rendered_settings_at = 20`); err != nil {
		t.Fatal(err)
	}
	if m := ts.getJSON(t, "/api/display", timer); m["filename"] != img {
		t.Errorf("after a good build: %v; want its image %s", m, img)
	}
}
