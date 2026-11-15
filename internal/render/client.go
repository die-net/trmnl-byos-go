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
	"os"
	"path/filepath"
	"strings"
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
// matching ErrBuildFailed. A job that is too big to send, or that the shim
// refuses with a 400, returns an error matching ErrRejected. A renderer that
// cannot be reached or does not answer with a result returns an error
// matching ErrUnavailable.
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
	go func() {
		pw.CloseWithError(writeRequest(pw, enc, writeProject))
	}()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(c.URL, "/")+"/build", pr)
	if err != nil {
		_ = pr.CloseWithError(err)
		return "", fmt.Errorf("render request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-tar")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		_ = pr.CloseWithError(err)
		return "", fmt.Errorf("render request: %w: %w", ErrUnavailable, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusUnprocessableEntity {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		kind := ErrUnavailable
		if resp.StatusCode == http.StatusBadRequest {
			kind = ErrRejected
		}
		return "", fmt.Errorf("render request: %w: %s: %s", kind, resp.Status, strings.TrimSpace(string(msg)))
	}

	var log strings.Builder
	tr := tar.NewReader(resp.Body)
	got := 0
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
		layout, ok := isLayout(h.Name)
		if !ok {
			return log.String(), fmt.Errorf("read render result: unexpected %q", h.Name)
		}
		if err := png(layout, tr); err != nil {
			return log.String(), err
		}
		got++
	}
	if resp.StatusCode == http.StatusUnprocessableEntity {
		return log.String(), ErrBuildFailed
	}
	if got != len(Layouts) {
		return log.String(), fmt.Errorf("render result has %d of %d layouts", got, len(Layouts))
	}
	return log.String(), nil
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
