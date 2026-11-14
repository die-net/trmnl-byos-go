//go:build docker

package render

import (
	"archive/tar"
	"bytes"
	"image/png"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

// TestRendererImage builds a small recipe in a running renderer container.
// Start one, then run:
//
//	docker run --rm --init -p 8081:8081 trmnl-byos-render
//	TRMNL_RENDER_URL=http://localhost:8081 go test -tags docker -run TestRendererImage ./internal/render/
//
// The build loads the framework from trmnl.com, so it needs the internet.
func TestRendererImage(t *testing.T) {
	url := os.Getenv("TRMNL_RENDER_URL")
	if url == "" {
		t.Fatal("set TRMNL_RENDER_URL to a running renderer")
	}
	const settings = "strategy: static\nstatic_data: '{\"name\": \"renderer\"}'\nframework_version: latest\nrefresh_interval: 15\ncustom_fields: []\n"
	const view = `<div class="layout layout--col"><span class="title">Hello, {{ name }}</span><span class="description">{{ trmnl.user.time_zone_iana }}</span></div>`
	files := map[string]string{"src/settings.yml": settings, ".trmnlp.yml": "time_zone: America/Los_Angeles\n"}
	for _, layout := range Layouts {
		files["src/"+layout+".liquid"] = view
	}
	c := &Client{URL: url, HTTP: &http.Client{Timeout: 3 * time.Minute}}
	got := map[string][]byte{}
	log, err := c.Build(t.Context(), Job{Width: 800, Height: 480, Palette: "gray-4"},
		func(tw *tar.Writer) error {
			for name, body := range files {
				if err := AddFile(tw, name, int64(len(body)), strings.NewReader(body)); err != nil {
					return err
				}
			}
			return nil
		},
		func(layout string, r io.Reader) error {
			var buf bytes.Buffer
			_, err := buf.ReadFrom(r)
			got[layout] = buf.Bytes()
			return err
		})
	if err != nil {
		t.Fatalf("Build: %v\n%s", err, log)
	}
	for _, layout := range Layouts {
		cfg, err := png.DecodeConfig(bytes.NewReader(got[layout]))
		if err != nil || cfg.Width != 800 || cfg.Height != 480 {
			t.Errorf("%s: %dx%d, %v", layout, cfg.Width, cfg.Height, err)
		}
	}
	if err := os.WriteFile(os.TempDir()+"/trmnl-render-full.png", got["full"], 0o600); err != nil {
		t.Fatal(err)
	}
}
