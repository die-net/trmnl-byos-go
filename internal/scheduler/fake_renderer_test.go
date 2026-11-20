package scheduler

import (
	"archive/tar"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"strings"

	"github.com/die-net/trmnl-byos-go/internal/render"
)

var (
	friendlyIDRE = regexp.MustCompile(`(?m)^ *friendly_id: (.*)$`)
	headlineRE   = regexp.MustCompile(`(?m)^ *headline: (.*)$`)
)

// fakeRenderer answers a build request as the renderer would, without
// running a process, so no test waits on a subprocess or a deadline. Each
// "PNG" names its layout, size, palette and first field value, and the
// friendly_id the project's trmnl.device has, if it has one, and the
// headline pushed to the instance, if there is one. It fails while
// h.failFile exists, and fails if a secret reached the project's files.
// While h.fetchFail is set, it answers as for a failed data fetch.
func (h *harness) fakeRenderer(w http.ResponseWriter, r *http.Request) {
	h.builds.Add(1)
	if h.duringBuild != nil {
		h.duringBuild()
	}
	if h.down.Load() {
		http.Error(w, "busy", http.StatusServiceUnavailable)
		return
	}
	if h.lose.Add(-1) >= 0 {
		_, _ = io.Copy(io.Discard, r.Body)
		w.WriteHeader(http.StatusProcessing)
		if conn, _, err := http.NewResponseController(w).Hijack(); err == nil {
			_ = conn.Close()
		}
		return
	}
	h.lose.Store(0)
	if h.reject.Load() {
		http.Error(w, "read job: unexpected EOF", http.StatusBadRequest)
		return
	}
	job, files, err := readFakeRequest(r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	// The shim answers 102 Processing once it has the job, before any
	// result.
	w.WriteHeader(http.StatusProcessing)
	if h.fetchFail.Load() {
		url := "https://api.example/forecast"
		if h.fetchURL != "" {
			url = strings.ReplaceAll(h.fetchURL, "{{token}}", job.Env["FIELD_0"])
		}
		w.Header().Set("Content-Type", "application/x-tar")
		w.WriteHeader(http.StatusUnprocessableEntity)
		tw := tar.NewWriter(w)
		addFake(tw, "build.log", "warning: HTTP 401 from "+url+" — Unauthorized\nDone!\n")
		addFake(tw, "fetch_failed", "")
		_ = tw.Close()
		return
	}

	h.mu.Lock()
	h.configs = append(h.configs, string(files[".trmnlp.yml"]))
	h.mu.Unlock()

	var log strings.Builder
	failed := false
	switch {
	case exists(h.leakFile):
		fmt.Fprintf(&log, "fetching https://api.example/?key=%s\nGET https://api.example/?key=%s failed: 401\n", job.Env["FIELD_0"], job.Env["FIELD_0"])
		failed = true
	case exists(h.failFile):
		log.WriteString("starting\ndata source said 401\n")
		failed = true
	default:
		for _, body := range files {
			if bytes.Contains(body, []byte("hunter2")) {
				log.WriteString("a secret is in the project\n")
				failed = true
				break
			}
		}
	}

	w.Header().Set("Content-Type", "application/x-tar")
	if failed {
		w.WriteHeader(http.StatusUnprocessableEntity)
	}
	tw := tar.NewWriter(w)
	addFake(tw, "build.log", log.String())
	if !failed {
		first := func(re *regexp.Regexp) string {
			if m := re.FindSubmatch(files[".trmnlp.yml"]); m != nil {
				return string(m[1])
			}
			return ""
		}
		dev, hl := first(friendlyIDRE), first(headlineRE)
		for _, layout := range render.Layouts {
			img := layout
			if dev != "" {
				img += "@" + dev
			}
			img += fmt.Sprintf(" %dx%d %s %s", job.Width, job.Height, job.Palette, job.Env["FIELD_0"])
			if hl != "" {
				img += " hl=" + hl
			}
			addFake(tw, layout+".png", img)
		}
	}
	_ = tw.Close()
}

func addFake(tw *tar.Writer, name, body string) {
	_ = tw.WriteHeader(&tar.Header{Name: name, Mode: 0o600, Size: int64(len(body)), Typeflag: tar.TypeReg})
	_, _ = io.WriteString(tw, body)
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return !errors.Is(err, os.ErrNotExist)
}

// readFakeRequest reads a build request's job and the project's files by
// name.
func readFakeRequest(r io.Reader) (render.Job, map[string][]byte, error) {
	var job render.Job
	tr := tar.NewReader(r)
	if _, err := tr.Next(); err != nil {
		return job, nil, fmt.Errorf("read job: %w", err)
	}
	if err := json.NewDecoder(tr).Decode(&job); err != nil {
		return job, nil, fmt.Errorf("decode job: %w", err)
	}
	files := map[string][]byte{}
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return job, files, nil
		}
		if err != nil {
			return job, nil, fmt.Errorf("read project: %w", err)
		}
		if h.Typeflag != tar.TypeReg {
			continue
		}
		if files[h.Name], err = io.ReadAll(tr); err != nil {
			return job, nil, fmt.Errorf("read %s: %w", h.Name, err)
		}
	}
}
