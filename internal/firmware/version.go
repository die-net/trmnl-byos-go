package firmware

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/die-net/trmnl-byos-go/internal/errkind"
)

// maxVersionParts bounds how many dotted numbers a version has.
const maxVersionParts = 4

// Version is a firmware version: dotted decimal numbers, such as 1.8.19.
type Version []uint32

// ParseVersion parses dotted decimal numbers. It takes nothing else: no
// prefix, no suffix, no empty part, and no sign. Parts that are not
// numbers are why a device that reports a version such as 1.8.19-dev
// is never offered firmware.
func ParseVersion(s string) (Version, error) {
	parts := strings.Split(s, ".")
	if len(parts) > maxVersionParts {
		return nil, errkind.New(errkind.ErrInvalid, fmt.Sprintf("the version %q has more than %d parts", s, maxVersionParts))
	}
	v := make(Version, len(parts))
	for i, p := range parts {
		if p == "" || len(p) > 9 || strings.Trim(p, "0123456789") != "" {
			return nil, errkind.New(errkind.ErrInvalid, fmt.Sprintf("the version %q is not dotted numbers, such as 1.8.19", s))
		}
		n, err := strconv.ParseUint(p, 10, 32)
		if err != nil {
			return nil, errkind.New(errkind.ErrInvalid, fmt.Sprintf("the version %q is not dotted numbers, such as 1.8.19", s))
		}
		v[i] = uint32(n)
	}
	return v, nil
}

// String returns the version as dotted numbers, without leading zeros.
func (v Version) String() string {
	parts := make([]string, len(v))
	for i, n := range v {
		parts[i] = strconv.FormatUint(uint64(n), 10)
	}
	return strings.Join(parts, ".")
}

// Compare returns -1, 0 or 1 as v is older than, the same as, or newer than
// w. A missing part counts as 0, so 1.8 and 1.8.0 are the same.
func (v Version) Compare(w Version) int {
	for i := range max(len(v), len(w)) {
		var a, b uint32
		if i < len(v) {
			a = v[i]
		}
		if i < len(w) {
			b = w[i]
		}
		if c := cmpUint(a, b); c != 0 {
			return c
		}
	}
	return 0
}

func cmpUint(a, b uint32) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

// cleanDescriptorVersion returns the version in an app descriptor's version
// string when it is plain X.Y.Z, optionally with a leading v, as git
// describe gives on a tag. Anything else, such as a commit hash or "1",
// says nothing about the firmware's own version.
func cleanDescriptorVersion(s string) (Version, bool) {
	v, err := ParseVersion(strings.TrimPrefix(s, "v"))
	if err != nil || len(v) != 3 {
		return nil, false
	}
	return v, true
}
