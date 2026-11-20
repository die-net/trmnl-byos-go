//go:build !unix

package render

import (
	"errors"
	"time"
)

var errNoFiles = errors.New("cleaning up after builds is built only for Unix")

func scratchRoots() []string { return nil }

func cleanFiles([]string, int, time.Duration) (int, error) { return 0, errNoFiles }

// CheckWritable refuses to start: the cleanup after builds is not built
// here.
func CheckWritable(...string) error { return errNoFiles }
