package firmware

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"io"

	"github.com/die-net/trmnl-byos-go/internal/errkind"
)

// The ESP app image format, from ESP-IDF's esp_image_format.h and
// esp_app_format.h, and checked against esp_image_format.c, which the
// bootloader verifies an image with.
//
// An image is a 24-byte header, then segmentCount segments of an 8-byte
// header (load address, data length) and the data. Then come padding and
// one checksum byte, so the image so far is a multiple of 16 bytes, then
// a SHA-256 of everything before it when the header's hash_appended is 1.
// The first segment's data starts with the 256-byte app descriptor.
const (
	imageMagic       = 0xE9
	imageHeaderLen   = 24
	segmentHeaderLen = 8
	maxSegments      = 16
	checksumInit     = 0xEF
	hashLen          = sha256.Size

	// Offsets in the image header.
	offSegmentCount = 1
	offChipID       = 12
	offHashAppended = 23

	// The app descriptor.
	appDescMagic   = 0xABCD5432
	appDescLen     = 256
	appDescOffset  = imageHeaderLen + segmentHeaderLen
	offDescVersion = 16
	offDescProject = 48
	offDescIDF     = 112
	descStringLen  = 32
)

// Image is what the headers of an ESP app image say.
type Image struct {
	// ChipID is the header's chip_id: which ESP32 family the image is built
	// for.
	ChipID uint16
	// Version is the app descriptor's version string. For this firmware it
	// is the build's git description or "1", not necessarily the version
	// the firmware reports.
	Version string
	// Project is the descriptor's project name.
	Project string
	// IDFVersion is the ESP-IDF version the image was built with.
	IDFVersion string
}

// ParseImage checks the structure of the ESP app image in the first size
// bytes of r, and returns what its headers say. It checks the magic byte,
// that every segment is inside the file, the 8-bit checksum, the appended
// SHA-256 when there is one, and the app descriptor's magic. It reads r
// with ReadAt and never holds the file in memory. Every refusal wraps
// ErrImage.
func ParseImage(r io.ReaderAt, size int64) (Image, error) {
	var hdr [imageHeaderLen]byte
	if err := readAt(r, size, hdr[:], 0); err != nil {
		return Image{}, err
	}
	if hdr[0] != imageMagic {
		return Image{}, imageError("the file is not an ESP firmware image (it does not start with the image header)")
	}
	segments := int(hdr[offSegmentCount])
	if segments < 1 || segments > maxSegments {
		return Image{}, imageError("the image header lists %d segments, and an app image has 1 to %d", segments, maxSegments)
	}
	hashAppended := hdr[offHashAppended]
	if hashAppended > 1 {
		return Image{}, imageError("the image header is damaged (its hash flag is %d, and it can only be 0 or 1)", hashAppended)
	}
	img := Image{ChipID: binary.LittleEndian.Uint16(hdr[offChipID:])}

	// Walk the segments, checking each fits and folding its data into the
	// checksum.
	sum := byte(checksumInit)
	off := int64(imageHeaderLen)
	for i := range segments {
		var sh [segmentHeaderLen]byte
		if err := readAt(r, size, sh[:], off); err != nil {
			return Image{}, err
		}
		n := int64(binary.LittleEndian.Uint32(sh[4:]))
		off += segmentHeaderLen
		if n > size-off {
			return Image{}, imageError("the file is cut short (segment %d runs past its end)", i+1)
		}
		if i == 0 {
			if n < appDescLen {
				return Image{}, imageError("the image has no app descriptor (its first segment is too small to hold one)")
			}
			desc, err := parseDescriptor(r, size)
			if err != nil {
				return Image{}, err
			}
			img.Version, img.Project, img.IDFVersion = desc.Version, desc.Project, desc.IDFVersion
		}
		x := xorWriter{sum: sum}
		if _, err := io.Copy(&x, io.NewSectionReader(r, off, n)); err != nil {
			return Image{}, fmt.Errorf("read segment %d: %w", i, err)
		}
		sum = x.sum
		off += n
	}

	// The checksum byte is the last of the padding that makes the length a
	// multiple of 16.
	padded := (off + 16) &^ 15
	if padded > size {
		return Image{}, imageError("the file is cut short (it ends before the image's checksum)")
	}
	var got [1]byte
	if err := readAt(r, size, got[:], padded-1); err != nil {
		return Image{}, err
	}
	if got[0] != sum {
		return Image{}, imageError("the image is damaged (its checksum does not match its contents)")
	}
	if hashAppended == 1 {
		var want [hashLen]byte
		if err := readAt(r, size, want[:], padded); err != nil {
			return Image{}, err
		}
		h := sha256.New()
		if _, err := io.Copy(h, io.NewSectionReader(r, 0, padded)); err != nil {
			return Image{}, fmt.Errorf("hash image: %w", err)
		}
		if !bytes.Equal(h.Sum(nil), want[:]) {
			return Image{}, imageError("the image is damaged (its own SHA-256 does not match its contents)")
		}
	}
	return img, nil
}

// parseDescriptor reads the app descriptor at the start of the first
// segment's data.
func parseDescriptor(r io.ReaderAt, size int64) (Image, error) {
	var d [appDescLen]byte
	if err := readAt(r, size, d[:], appDescOffset); err != nil {
		return Image{}, err
	}
	if binary.LittleEndian.Uint32(d[:]) != appDescMagic {
		return Image{}, imageError("the image has no valid app descriptor")
	}
	var img Image
	var err error
	if img.Version, err = descString(d[offDescVersion:]); err != nil {
		return Image{}, err
	}
	if img.Project, err = descString(d[offDescProject:]); err != nil {
		return Image{}, err
	}
	if img.IDFVersion, err = descString(d[offDescIDF:]); err != nil {
		return Image{}, err
	}
	return img, nil
}

// descString returns the NUL-terminated string in the first descStringLen
// bytes of b. It refuses a character that is not printable ASCII, since the
// string is stored and shown.
func descString(b []byte) (string, error) {
	b = b[:descStringLen]
	if i := bytes.IndexByte(b, 0); i >= 0 {
		b = b[:i]
	}
	for _, c := range b {
		if c < ' ' || c > '~' {
			return "", imageError("the image's app descriptor has a string that is not plain text")
		}
	}
	return string(b), nil
}

// imageError returns an error that wraps ErrImage and whose text is the
// sentence given, which an admin reads as it is.
func imageError(format string, args ...any) error {
	return errkind.New(ErrImage, fmt.Sprintf(format, args...))
}

// readAt fills p from offset off of the first size bytes of r. A file too
// short for it is refused: as too small to be an image when it cannot hold
// the image header, and as cut short otherwise.
func readAt(r io.ReaderAt, size int64, p []byte, off int64) error {
	if off < 0 || off+int64(len(p)) > size {
		if size < imageHeaderLen {
			return imageError("the file is too small to be an ESP firmware image")
		}
		return imageError("the file is cut short")
	}
	if _, err := r.ReadAt(p, off); err != nil && !errors.Is(err, io.EOF) {
		return fmt.Errorf("read image: %w", err)
	}
	return nil
}

// xorWriter folds the bytes written to it into sum with XOR, which is the
// image checksum.
type xorWriter struct{ sum byte }

func (x *xorWriter) Write(p []byte) (int, error) {
	for _, b := range p {
		x.sum ^= b
	}
	return len(p), nil
}
