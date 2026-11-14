package render

import (
	"archive/tar"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Shim serves build requests inside the renderer container. Each build gets
// a fresh directory, with its own temporary and cache directories, which is
// removed when the build ends.
type Shim struct {
	// Trmnlp is the path to the trmnlp command.
	Trmnlp string
	// Env is the environment every build starts from: the container's own,
	// with the display and the proxy settings.
	Env []string
	// Timeout bounds one build.
	Timeout time.Duration
	// slots holds a token for each build that may run at once.
	slots chan struct{}
}

// NewShim returns a shim that runs at most concurrency builds at once.
func NewShim(trmnlp string, env []string, timeout time.Duration, concurrency int) *Shim {
	return &Shim{Trmnlp: trmnlp, Env: env, Timeout: timeout, slots: make(chan struct{}, max(concurrency, 1))}
}

// ServeHTTP handles POST /build.
func (s *Shim) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost || r.URL.Path != "/build" {
		http.NotFound(w, r)
		return
	}
	dir, err := os.MkdirTemp("", "build-")
	if err != nil {
		http.Error(w, "create build dir", http.StatusInternalServerError)
		return
	}
	defer func() { _ = os.RemoveAll(dir) }()

	job, err := readRequest(r.Body, filepath.Join(dir, "project"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	select {
	case s.slots <- struct{}{}:
		defer func() { <-s.slots }()
	case <-r.Context().Done():
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), s.Timeout)
	defer cancel()
	start := time.Now()
	buildErr := s.build(ctx, dir, job)
	slog.Info("render: build", "width", job.Width, "height", job.Height, "palette", job.Palette,
		"seconds", time.Since(start).Seconds(), "err", buildErr)

	w.Header().Set("Content-Type", "application/x-tar")
	if buildErr != nil {
		w.WriteHeader(http.StatusUnprocessableEntity)
	}
	if err := writeResult(w, dir, buildErr == nil); err != nil {
		slog.Warn("render: write result", "err", err)
	}
}

// readRequest reads the job, the first member of the request's tar, and
// extracts the other members into dir as they stream in.
func readRequest(r io.Reader, dir string) (Job, error) {
	tr := tar.NewReader(io.LimitReader(r, maxProjectBytes))
	h, err := tr.Next()
	if err != nil || h.Name != jobMember || h.Typeflag != tar.TypeReg {
		return Job{}, errors.New("read request: the first member must be " + jobMember)
	}
	var job Job
	if err := json.NewDecoder(io.LimitReader(tr, maxJobBytes)).Decode(&job); err != nil {
		return Job{}, fmt.Errorf("read job: %w", err)
	}
	if err := job.validate(); err != nil {
		return Job{}, err
	}
	if err := extract(tr, dir); err != nil {
		return Job{}, err
	}
	return job, nil
}

// extract writes the remaining members of a project tar into dir. os.Root
// keeps every name inside dir, and only directories and regular files are
// written.
func extract(tr *tar.Reader, dir string) error {
	if err := os.Mkdir(dir, 0o700); err != nil {
		return fmt.Errorf("create project dir: %w", err)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return fmt.Errorf("open project dir: %w", err)
	}
	defer root.Close()
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("read project: %w", err)
		}
		name := path.Clean(h.Name)
		switch h.Typeflag {
		case tar.TypeDir:
			if err := root.MkdirAll(name, 0o700); err != nil {
				return fmt.Errorf("project dir %s: %w", name, err)
			}
		case tar.TypeReg:
			if err := root.MkdirAll(path.Dir(name), 0o700); err != nil {
				return fmt.Errorf("project dir %s: %w", name, err)
			}
			if err := writeFile(root, name, tr); err != nil {
				return err
			}
		default:
			return fmt.Errorf("project file %s: only directories and regular files", name)
		}
	}
}

func writeFile(root *os.Root, name string, r io.Reader) error {
	f, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("project file %s: %w", name, err)
	}
	if _, err := io.Copy(f, r); err != nil {
		_ = f.Close()
		return fmt.Errorf("project file %s: %w", name, err)
	}
	return f.Close()
}

// build runs trmnlp on the project in dir, writing its output to the build
// log.
func (s *Shim) build(ctx context.Context, dir string, job Job) error {
	for _, sub := range []string{"tmp", "cache"} {
		if err := os.Mkdir(filepath.Join(dir, sub), 0o700); err != nil {
			return fmt.Errorf("create %s dir: %w", sub, err)
		}
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return fmt.Errorf("open build dir: %w", err)
	}
	defer root.Close()
	logFile, err := root.Create(logMember)
	if err != nil {
		return fmt.Errorf("create build log: %w", err)
	}
	defer logFile.Close()

	project := filepath.Join(dir, "project")
	//nolint:gosec // G204: the command is the operator's -trmnlp flag, every argument is a number, a palette ID that Job.validate held to [a-z0-9-], or the build's own directory, and no shell runs.
	cmd := exec.CommandContext(ctx, s.Trmnlp, "build", "--png",
		"--width", strconv.Itoa(job.Width), "--height", strconv.Itoa(job.Height),
		"--palette", job.Palette, "-d", project)
	cmd.Dir = project
	cmd.Env = append(slices.Clone(s.Env), "TMPDIR="+filepath.Join(dir, "tmp"), "XDG_CACHE_HOME="+filepath.Join(dir, "cache"))
	for name, value := range job.Env {
		cmd.Env = append(cmd.Env, name+"="+value)
	}
	cmd.Stdout, cmd.Stderr = logFile, logFile
	inProcessGroup(cmd)
	err = cmd.Run()
	// Firefox or geckodriver may outlive trmnlp; nothing of a build stays.
	_ = killGroup(cmd)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrBuildFailed, err)
	}
	for _, layout := range Layouts {
		if _, err := os.Stat(filepath.Join(project, "_build", layout+".png")); err != nil {
			return fmt.Errorf("%w: no %s.png", ErrBuildFailed, layout)
		}
	}
	return nil
}

// writeResult streams a tar of the build log, and of the PNGs when the
// build succeeded.
func writeResult(w io.Writer, dir string, withPNGs bool) error {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return fmt.Errorf("open build dir: %w", err)
	}
	defer root.Close()
	tw := tar.NewWriter(w)
	names := []string{logMember}
	files := []string{logMember}
	if withPNGs {
		for _, layout := range Layouts {
			names = append(names, layout+".png")
			files = append(files, "project/_build/"+layout+".png")
		}
	}
	for i, file := range files {
		if err := addResultFile(tw, root, names[i], file); err != nil {
			return err
		}
	}
	return tw.Close()
}

// addResultFile streams one file of the build directory into a tar. A
// missing build log is written as empty.
func addResultFile(tw *tar.Writer, root *os.Root, name, file string) error {
	f, err := root.Open(file)
	if errors.Is(err, fs.ErrNotExist) && name == logMember {
		return tw.WriteHeader(&tar.Header{Name: name, Mode: 0o600, Typeflag: tar.TypeReg})
	}
	if err != nil {
		return fmt.Errorf("open %s: %w", name, err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return fmt.Errorf("stat %s: %w", name, err)
	}
	if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o600, Size: info.Size(), Typeflag: tar.TypeReg, ModTime: info.ModTime()}); err != nil {
		return err
	}
	_, err = io.Copy(tw, f)
	return err
}

// isLayout reports whether name is a layout's PNG.
func isLayout(name string) (string, bool) {
	layout, ok := strings.CutSuffix(name, ".png")
	return layout, ok && slices.Contains(Layouts, layout)
}
