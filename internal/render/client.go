package render

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptrace"
	"net/textproto"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
)

// Client sends build jobs to a shim.
type Client struct {
	// URL is the shim's base URL, such as http://renderer:8081.
	URL  string
	HTTP *http.Client
}

// Build sends job to the shim with the project writeProject writes, and
// calls png for each layout's PNG as it streams back. It returns the build
// log, cut to a bound. A build trmnlp failed returns its log and an error
// matching ErrBuildFailed, and one whose data fetch failed an error matching
// ErrFetchFailed. A job that is too big to send, or that the shim
// refuses with a 400, returns an error matching ErrRejected. A renderer that
// did not take the job returns an error matching ErrUnavailable: one that
// cannot be reached, that answers 503, or that fails before it answers 102
// Processing, which the shim sends once it has read the job and starts the
// build. A failure after that, such as a connection lost mid-build or a
// timeout, may be the build's doing, so it returns an error that matches
// none of these, which counts against the build. A project that writeProject
// cannot write, such as
// a recipe whose files are gone, returns writeProject's error, which matches
// none of these: the fault is in the project, not the renderer.
func (c *Client) Build(ctx context.Context, job Job, writeProject func(*tar.Writer) error,
	png func(layout string, r io.Reader) error,
) (string, error) {
	if err := job.validate(); err != nil {
		return "", err
	}
	enc, err := encodeJob(job)
	if err != nil {
		return "", err
	}
	if len(enc) > maxJobBytes {
		return "", fmt.Errorf("%w: the job is %d bytes, over the limit of %d", ErrRejected, len(enc), maxJobBytes)
	}
	pr, pw := io.Pipe()
	written := make(chan error, 1)
	go func() {
		err := writeRequest(bodyWriter{pw}, enc, writeProject)
		written <- err
		pw.CloseWithError(err)
	}()
	var accepted atomic.Bool
	trace := &httptrace.ClientTrace{Got1xxResponse: func(code int, _ textproto.MIMEHeader) error {
		if code == http.StatusProcessing {
			accepted.Store(true)
		}
		return nil
	}}
	req, err := http.NewRequestWithContext(httptrace.WithClientTrace(ctx, trace), http.MethodPost, strings.TrimRight(c.URL, "/")+"/build", pr)
	if err != nil {
		_ = pr.CloseWithError(err)
		return "", fmt.Errorf("render request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-tar")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		_ = pr.CloseWithError(err)
		// Closing the reader ends the writer's next write, so it is done.
		if werr := <-written; werr != nil && !errors.Is(werr, errBody) {
			return "", fmt.Errorf("write project: %w", werr)
		}
		if accepted.Load() {
			return "", fmt.Errorf("render request: the renderer stopped during the build: %w", err)
		}
		return "", fmt.Errorf("render request: %w: %w", ErrUnavailable, err)
	}
	// The shim sweeps up after the build before its response starts, so
	// the next build may come as soon as this returns, whether or not the
	// body was read to its end. One build every few minutes needs no
	// connection reuse, so an early close only costs a new connection.
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusUnprocessableEntity {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		var kind error
		switch {
		case resp.StatusCode == http.StatusBadRequest:
			kind = ErrRejected
		case resp.StatusCode == http.StatusServiceUnavailable || !accepted.Load():
			kind = ErrUnavailable
		default:
			kind = errFailedDuringBuild
		}
		return "", fmt.Errorf("render request: %w: %s: %s", kind, resp.Status, strings.TrimSpace(string(msg)))
	}
	// The shim answers 102 before every result, so a result without one
	// came through something that drops 1xx responses, such as an HTTP
	// proxy, and an outage after it could not be told from one before. It
	// counts, so the target backs off rather than being tried at once.
	if !accepted.Load() {
		return "", fmt.Errorf("render request: the renderer answered %s with no 102 Processing first: whatever is between the server and the renderer drops 1xx responses; reach it directly or through a TCP forward", resp.Status)
	}

	var log strings.Builder
	tr := tar.NewReader(resp.Body)
	got := 0
	fetchFailed := false
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return log.String(), fmt.Errorf("read render result: %w", err)
		}
		if h.Name == logMember {
			if _, err := io.Copy(&log, io.LimitReader(tr, maxLogBytes)); err != nil {
				return log.String(), fmt.Errorf("read build log: %w", err)
			}
			continue
		}
		if h.Name == fetchFailedMember {
			fetchFailed = true
			continue
		}
		layout, ok := isLayout(h.Name)
		if !ok {
			return log.String(), fmt.Errorf("read render result: unexpected %q", h.Name)
		}
		if h.Size > maxPNGBytes {
			return log.String(), fmt.Errorf("read render result: %s is %d bytes, over the limit of %d", h.Name, h.Size, maxPNGBytes)
		}
		if err := png(layout, tr); err != nil {
			return log.String(), err
		}
		got++
	}
	if resp.StatusCode == http.StatusUnprocessableEntity {
		if fetchFailed {
			return log.String(), ErrFetchFailed
		}
		return log.String(), ErrBuildFailed
	}
	if got != len(Layouts) {
		return log.String(), fmt.Errorf("render result has %d of %d layouts", got, len(Layouts))
	}
	return log.String(), nil
}

// errFailedDuringBuild marks an error status the renderer answered after it
// took the job, which counts against the build.
var errFailedDuringBuild = errors.New("the renderer failed during the build")

// errBody marks an error writing the request body, which means the
// renderer stopped reading it, so it can be told from an error reading the
// project.
var errBody = errors.New("write request body")

// bodyWriter is the request body, with each write's error marked errBody.
type bodyWriter struct{ w io.Writer }

func (b bodyWriter) Write(p []byte) (int, error) {
	n, err := b.w.Write(p)
	if err != nil {
		err = fmt.Errorf("%w: %w", errBody, err)
	}
	return n, err
}

// writeRequest writes the request tar: the encoded job, then the project.
func writeRequest(w io.Writer, enc []byte, writeProject func(*tar.Writer) error) error {
	tw := tar.NewWriter(w)
	if err := AddFile(tw, jobMember, int64(len(enc)), bytes.NewReader(enc)); err != nil {
		return err
	}
	if err := writeProject(tw); err != nil {
		return err
	}
	return tw.Close()
}

// AddDir streams the regular files under dir into tw, under prefix.
func AddDir(tw *tar.Writer, dir, prefix string) error {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return fmt.Errorf("open %s: %w", dir, err)
	}
	defer root.Close()
	return fs.WalkDir(root.FS(), ".", func(name string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.Type().IsRegular() {
			return nil
		}
		f, err := root.Open(name)
		if err != nil {
			return err
		}
		defer f.Close()
		info, err := f.Stat()
		if err != nil {
			return err
		}
		return AddFile(tw, filepath.ToSlash(filepath.Join(prefix, name)), info.Size(), f)
	})
}

// AddFile streams size bytes from r into tw as a regular file called name.
func AddFile(tw *tar.Writer, name string, size int64, r io.Reader) error {
	if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o600, Size: size, Typeflag: tar.TypeReg}); err != nil {
		return fmt.Errorf("add %s: %w", name, err)
	}
	if _, err := io.Copy(tw, r); err != nil {
		return fmt.Errorf("add %s: %w", name, err)
	}
	return nil
}
