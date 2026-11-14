// Package render runs trmnlp builds. The server's Client sends a build job
// to the Shim, which runs inside the renderer container, and gets the
// rendered PNGs back. Both directions stream: the request body is a tar
// whose first member is the JSON job and whose other members are the recipe
// project, and the response is a tar of the build log and the PNGs.
package render

import (
	"errors"
	"fmt"
	"regexp"
)

// Tar member names of the protocol. jobMember is the first member of a
// request. Every member after it is a project file, by its own name.
const (
	jobMember = "job.json"
	logMember = "build.log"
)

// Layouts are the views trmnlp renders for every recipe, by file name.
var Layouts = []string{"full", "half_horizontal", "half_vertical", "quadrant"}

// Limits on a job, which the shim enforces.
const (
	maxJobBytes = 64 << 10
	// maxProjectBytes bounds the whole request body, the job included.
	maxProjectBytes = 16 << 20
	maxDimension    = 4096
	// maxLogBytes bounds the build log a client keeps.
	maxLogBytes = 64 << 10
)

// Job is what to build.
type Job struct {
	Width   int    `json:"width"`
	Height  int    `json:"height"`
	Palette string `json:"palette"`
	// Env is extra environment for the build: the instance's field values,
	// which the project's .trmnlp.yml names as {{ env.NAME }}. They travel
	// in the request, never in the project, so no file holds a secret one.
	// Every name starts with FIELD_, so a job can never set a variable the
	// build tools read, such as LD_PRELOAD or a proxy setting.
	Env map[string]string `json:"env,omitempty"`
}

var (
	paletteRE = regexp.MustCompile(`^[a-z0-9-]{1,32}$`)
	envNameRE = regexp.MustCompile(`^FIELD_[A-Z0-9_]{1,56}$`)
)

// ErrBuildFailed means trmnlp ran but did not produce the screens. The
// build log says why.
var ErrBuildFailed = errors.New("trmnlp build failed")

// ErrUnavailable means the renderer could not be reached or did not answer
// the request with a build result: a refused or reset connection, a timeout,
// a busy shim, or a server error. It says nothing about the recipe, so it
// does not count toward a render's backoff.
var ErrUnavailable = errors.New("renderer unavailable")

func (j Job) validate() error {
	if j.Width < 1 || j.Height < 1 || j.Width > maxDimension || j.Height > maxDimension {
		return fmt.Errorf("bad size %dx%d", j.Width, j.Height)
	}
	if !paletteRE.MatchString(j.Palette) {
		return fmt.Errorf("bad palette %q", j.Palette)
	}
	for name := range j.Env {
		if !envNameRE.MatchString(name) {
			return fmt.Errorf("bad environment variable name %q", name)
		}
	}
	return nil
}
