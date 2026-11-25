//go:build !unix

package render

import "os"

// removeTree removes dir and everything in it. Builds run only on Linux,
// so nothing here makes a tree that os.RemoveAll cannot remove.
func removeTree(dir string) error {
	return os.RemoveAll(dir)
}
