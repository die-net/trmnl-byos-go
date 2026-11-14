// Package cache keeps fetch results on disk. It is copied from plainweb's
// internal/cache (github.com/die-net/plainweb, dc3caea). An entry is one file: a JSON
// header line, then a body. A key is a SHA-256 hash, stored as hex, with the
// first two characters as a subdirectory. Entries are written to a temporary
// file and renamed into place, so readers, and other processes sharing the
// directory, see a whole entry or none. The file's modification time is when
// the entry was last known fresh.
package cache

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"math/rand/v2"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// Key names an entry.
type Key [sha256.Size]byte

// KeyOf hashes parts, each followed by a NUL, so ("ab", "c") and ("a", "bc") differ.
func KeyOf(parts ...string) Key {
	h := sha256.New()
	for _, p := range parts {
		_, _ = io.WriteString(h, p)
		_, _ = h.Write([]byte{0})
	}
	var k Key
	h.Sum(k[:0])
	return k
}

func (k Key) String() string { return hex.EncodeToString(k[:]) }

// tmpPrefix starts every file that is still being written.
const tmpPrefix = ".tmp-"

// Cache is a directory of entries.
type Cache struct {
	dir string
}

// New returns a cache in dir, creating it if needed.
func New(dir string) (*Cache, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create cache dir: %w", err)
	}
	return &Cache{dir: dir}, nil
}

func (c *Cache) path(k Key) string {
	s := k.String()
	return filepath.Join(c.dir, s[:2], s[2:])
}

// Entry is an open entry. Read the body from it, then Close it.
type Entry struct {
	*bufio.Reader
	file    *os.File
	ModTime time.Time
	// Size is the length of the body.
	Size int64
}

// Close closes the entry.
func (e *Entry) Close() error { return e.file.Close() }

// Open opens an entry and decodes its header into header. A missing entry
// returns an error matching fs.ErrNotExist.
func (c *Cache) Open(k Key, header any) (*Entry, error) {
	f, err := os.Open(c.path(k))
	if err != nil {
		return nil, err
	}
	st, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	r := bufio.NewReaderSize(f, 64*1024)
	line, err := r.ReadBytes('\n')
	if err == nil {
		err = json.Unmarshal(line, header)
	}
	if err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("cache entry %s: %w", k, err)
	}
	return &Entry{Reader: r, file: f, ModTime: st.ModTime(), Size: st.Size() - int64(len(line))}, nil
}

// Writer writes a new entry. Write the body to it, then Commit, or Abort.
type Writer struct {
	*bufio.Writer
	file *os.File
	path string
	done bool
}

// Create starts a new entry with header. It replaces any entry with the same
// key on Commit.
func (c *Cache) Create(k Key, header any) (*Writer, error) {
	line, err := json.Marshal(header)
	if err != nil {
		return nil, err
	}
	path := c.path(k)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	f, err := os.CreateTemp(filepath.Dir(path), tmpPrefix+"*")
	if err != nil {
		return nil, err
	}
	w := &Writer{Writer: bufio.NewWriterSize(f, 64*1024), file: f, path: path}
	if _, err := w.Write(append(line, '\n')); err != nil {
		w.Abort()
		return nil, err
	}
	return w, nil
}

// Commit puts the entry in place.
func (w *Writer) Commit() error {
	if w.done {
		return errors.New("cache entry already finished")
	}
	w.done = true
	err := w.Flush()
	if cerr := w.file.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(w.file.Name(), w.path)
	}
	if err != nil {
		_ = os.Remove(w.file.Name())
	}
	return err
}

// Abort discards the entry. It does nothing after Commit.
func (w *Writer) Abort() {
	if w.done {
		return
	}
	w.done = true
	_ = w.file.Close()
	_ = os.Remove(w.file.Name())
}

// Touch marks an entry fresh as of t.
func (c *Cache) Touch(k Key, t time.Time) error {
	return os.Chtimes(c.path(k), t, t)
}

// Janitor removes old entries every interval, give or take a tenth, until ctx
// is done. See Sweep.
func (c *Cache) Janitor(ctx context.Context, interval, maxAge time.Duration, maxBytes int64) {
	for {
		jitter := time.Duration(rand.Int64N(int64(interval)/5+1)) - interval/10 //nolint:gosec // Jitter needs no secrecy.
		select {
		case <-ctx.Done():
			return
		case <-time.After(interval + jitter):
		}
		removed, err := c.Sweep(time.Now(), maxAge, maxBytes)
		if err != nil {
			slog.Warn("cache sweep", "err", err)
		}
		if removed > 0 {
			slog.Debug("cache sweep", "removed", removed)
		}
	}
}

type file struct {
	path string
	size int64
	mod  time.Time
}

// Sweep removes entries not fresh within maxAge, and temporary files older
// than maxAge, then the oldest entries until the rest fit in maxBytes. A
// maxBytes of 0 means no size limit. It returns how many files it removed.
// Other processes may sweep the same directory; a file already gone is fine.
func (c *Cache) Sweep(now time.Time, maxAge time.Duration, maxBytes int64) (int, error) {
	root, err := os.OpenRoot(c.dir)
	if err != nil {
		return 0, err
	}
	defer root.Close()
	var kept []file
	var total int64
	removed := 0
	err = fs.WalkDir(root.FS(), ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		if d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil //nolint:nilerr // Another sweep removed it.
		}
		if now.Sub(info.ModTime()) > maxAge {
			if root.Remove(path) == nil {
				removed++
			}
			return nil
		}
		if !strings.HasPrefix(d.Name(), tmpPrefix) {
			kept = append(kept, file{path: path, size: info.Size(), mod: info.ModTime()})
			total += info.Size()
		}
		return nil
	})
	if maxBytes <= 0 || total <= maxBytes {
		return removed, err
	}
	slices.SortFunc(kept, func(a, b file) int { return a.mod.Compare(b.mod) })
	for _, f := range kept {
		if total <= maxBytes {
			break
		}
		if root.Remove(f.path) == nil {
			removed++
		}
		total -= f.size
	}
	return removed, err
}
