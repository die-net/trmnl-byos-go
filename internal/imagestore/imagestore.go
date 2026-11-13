// Package imagestore keeps rendered PNGs on disk, named by a hash of their
// bytes, and serves them to devices.
package imagestore

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// nameLen is the length of an image's name: the first hex digits of its
// SHA-256. 64 bits keep names distinct across far more images than a server
// holds. The firmware caches images in SPIFFS under the response's filename
// and keeps at most 31 characters of it as-is, so the name must fit in that.
const nameLen = 16

// Store is a directory of PNGs named by content hash. An image is stored as
// <first two hex digits>/<rest of name>.png, so no one directory gets large.
type Store struct {
	dir string
}

// New opens the store in dir, creating it mode 0750 if needed.
func New(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, fmt.Errorf("create image dir: %w", err)
	}
	return &Store{dir: dir}, nil
}

// touchAfter is how stale an image's modification time gets before it is
// refreshed. Expire's grace period is far longer, and touching at most hourly
// keeps metadata writes, and filesystem snapshot churn, low.
const touchAfter = time.Hour

// tmpPrefix starts the name of a file still being written.
const tmpPrefix = ".tmp-"

// Writer writes a new image. Write the PNG to it, then Commit, or Abort.
type Writer struct {
	store *Store
	file  *os.File
	hash  hash.Hash
	w     io.Writer
	done  bool
}

// Create starts a new image. Its name is the hash of what is written, so it
// is known only at Commit. Until then it is a temporary file in the store's
// top directory.
func (s *Store) Create() (*Writer, error) {
	f, err := os.CreateTemp(s.dir, tmpPrefix+"*")
	if err != nil {
		return nil, fmt.Errorf("create image: %w", err)
	}
	h := sha256.New()
	return &Writer{store: s, file: f, hash: h, w: io.MultiWriter(f, h)}, nil
}

// Write writes part of the image.
func (w *Writer) Write(p []byte) (int, error) {
	return w.w.Write(p)
}

// Commit stores the image and returns its name. The same pixels always get
// the same name, so a device skips a download, and a repaint, when nothing
// changed. When the image is already stored, Commit leaves that file alone
// rather than rewriting it, and only refreshes its time.
func (w *Writer) Commit() (string, error) {
	if w.done {
		return "", errors.New("image already finished")
	}
	w.done = true
	defer func() { _ = os.Remove(w.file.Name()) }() // A no-op once renamed.
	if err := w.file.Close(); err != nil {
		return "", fmt.Errorf("close image: %w", err)
	}
	name := hex.EncodeToString(w.hash.Sum(nil))[:nameLen]
	if ok, err := w.store.Refresh(name); err != nil || ok {
		return name, err
	}
	path := w.store.path(name)
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return "", fmt.Errorf("create image dir: %w", err)
	}
	if err := os.Rename(w.file.Name(), path); err != nil {
		return "", fmt.Errorf("rename image: %w", err)
	}
	return name, nil
}

// Abort discards the image. It does nothing after Commit.
func (w *Writer) Abort() {
	if w.done {
		return
	}
	w.done = true
	_ = w.file.Close()
	_ = os.Remove(w.file.Name())
}

// Put stores the PNG read from r and returns its name.
func (s *Store) Put(r io.Reader) (string, error) {
	w, err := s.Create()
	if err != nil {
		return "", err
	}
	if _, err := io.Copy(w, r); err != nil {
		w.Abort()
		return "", fmt.Errorf("write image: %w", err)
	}
	return w.Commit()
}

// Refresh reports whether the named image is stored, and marks it used now
// if its time is more than touchAfter old, so Expire keeps it.
func (s *Store) Refresh(name string) (bool, error) {
	if !validName(name) {
		return false, nil
	}
	path := s.path(name)
	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("stat image: %w", err)
	}
	if now := time.Now(); now.Sub(info.ModTime()) > touchAfter {
		if err := os.Chtimes(path, now, now); err != nil {
			return false, fmt.Errorf("touch image: %w", err)
		}
	}
	return true, nil
}

// Expire deletes each image that keep does not name and that was last
// stored before cutoff, plus temporary files a crashed write left behind, and
// returns how many images it deleted. The cutoff gives a grace period: a
// render writes its image before the row naming it commits, and a device
// fetches an image a moment after it is told the name.
func (s *Store) Expire(keep map[string]bool, cutoff time.Time) (int, error) {
	subs, err := os.ReadDir(s.dir)
	if err != nil {
		return 0, fmt.Errorf("list images: %w", err)
	}
	deleted := 0
	for _, sub := range subs {
		if !sub.IsDir() {
			// A temporary file left by a crashed write.
			if strings.HasPrefix(sub.Name(), tmpPrefix) {
				if err := removeIfOld(filepath.Join(s.dir, sub.Name()), sub, cutoff); err != nil {
					return deleted, err
				}
			}
			continue
		}
		if len(sub.Name()) != 2 {
			continue
		}
		dir := filepath.Join(s.dir, sub.Name())
		files, err := os.ReadDir(dir)
		if err != nil {
			return deleted, fmt.Errorf("list images: %w", err)
		}
		for _, f := range files {
			rest, isPNG := strings.CutSuffix(f.Name(), ".png")
			name := sub.Name() + rest
			isImage := isPNG && validName(name)
			isTmp := strings.HasPrefix(f.Name(), tmpPrefix)
			if (!isImage && !isTmp) || keep[name] {
				continue
			}
			info, err := f.Info()
			if errors.Is(err, os.ErrNotExist) {
				continue // Deleted since the listing.
			}
			if err != nil {
				return deleted, fmt.Errorf("stat image: %w", err)
			}
			if !info.ModTime().Before(cutoff) {
				continue
			}
			if err := os.Remove(filepath.Join(dir, f.Name())); err != nil && !errors.Is(err, os.ErrNotExist) {
				return deleted, fmt.Errorf("delete image: %w", err)
			}
			if !isTmp {
				deleted++
			}
		}
	}
	return deleted, nil
}

// removeIfOld removes the file at path if it was last modified before
// cutoff. A file already gone is fine.
func removeIfOld(path string, d os.DirEntry, cutoff time.Time) error {
	info, err := d.Info()
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("stat image: %w", err)
	}
	if !info.ModTime().Before(cutoff) {
		return nil
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("delete image: %w", err)
	}
	return nil
}

// path is where the image with a valid name is stored.
func (s *Store) path(name string) string {
	return filepath.Join(s.dir, name[:2], name[2:]+".png")
}

// ServeHTTP serves the image named by the request's {name} path value,
// which is an image name followed by ".png". An image never changes once
// stored, so it may be cached forever.
func (s *Store) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	name, ok := strings.CutSuffix(r.PathValue("name"), ".png")
	if !ok || !validName(name) {
		http.NotFound(w, r)
		return
	}
	f, err := os.Open(s.path(name))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		http.Error(w, "stat image", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	http.ServeContent(w, r, "", info.ModTime(), f)
}

// validName reports whether name could be an image name, so a request can
// never name a path outside the store.
func validName(name string) bool {
	if len(name) != nameLen {
		return false
	}
	for _, c := range name {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}
