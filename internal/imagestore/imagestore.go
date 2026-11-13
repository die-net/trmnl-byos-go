// Package imagestore keeps rendered PNGs on disk, named by a hash of their
// bytes, and serves them to devices.
package imagestore

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
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

// Name returns the name an image with these bytes is stored under. The same
// pixels always get the same name, so a device skips a download, and a
// repaint, when nothing changed.
func Name(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])[:nameLen]
}

// touchAfter is how stale an image's modification time gets before Put
// refreshes it. Expire's grace period is far longer, and touching at most
// hourly keeps metadata writes, and filesystem snapshot churn, low.
const touchAfter = time.Hour

// Put stores a PNG and returns its name. When a file with the same name
// already exists, Put never rewrites it. It only refreshes the file's
// modification time, which Expire reads as when the image was last used.
func (s *Store) Put(data []byte) (string, error) {
	name := Name(data)
	path := s.path(name)
	info, err := os.Stat(path)
	switch {
	case err == nil:
		if now := time.Now(); now.Sub(info.ModTime()) > touchAfter {
			if err := os.Chtimes(path, now, now); err != nil {
				return "", fmt.Errorf("touch image: %w", err)
			}
		}
		return name, nil
	case !errors.Is(err, os.ErrNotExist):
		return "", fmt.Errorf("stat image: %w", err)
	}
	// Write to a temporary file and rename it, so a reader never sees part
	// of an image.
	sub := filepath.Dir(path)
	if err := os.MkdirAll(sub, 0o750); err != nil {
		return "", fmt.Errorf("create image dir: %w", err)
	}
	tmp, err := os.CreateTemp(sub, tmpPrefix+"*")
	if err != nil {
		return "", fmt.Errorf("create image: %w", err)
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return "", fmt.Errorf("write image: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return "", fmt.Errorf("close image: %w", err)
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return "", fmt.Errorf("rename image: %w", err)
	}
	return name, nil
}

// tmpPrefix starts the name of a file Put is still writing.
const tmpPrefix = ".tmp-"

// Expire deletes each image that keep does not name and that was last
// stored before cutoff, plus temporary files a crashed Put left behind, and
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
		if !sub.IsDir() || len(sub.Name()) != 2 {
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
