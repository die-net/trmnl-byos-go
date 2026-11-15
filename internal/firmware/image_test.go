package firmware

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/die-net/trmnl-byos-go/internal/errkind"
)

// imageSpec describes an ESP app image for buildImage. The zero value is a
// good ESP32-S3 image with version 1.8.19.
type imageSpec struct {
	chip uint16
	// magic, when not 0, replaces the header's magic byte.
	magic byte
	// descMagic, when not 0, replaces the app descriptor's magic.
	descMagic uint32
	// version is the descriptor's version string; "" means 1.8.19.
	version string
	idf     string
	// noHash leaves out the appended SHA-256.
	noHash bool
	// noDescriptor starts the first segment with something else.
	noDescriptor bool
	// badChecksum flips the checksum byte, and badHash the appended hash.
	badChecksum, badHash bool
	// segments is how many segments, 2 when 0.
	segments uint8
}

// chipS3 and chipC3 are the chip IDs in a header.
const (
	chipS3 = 0x0009
	chipC3 = 0x0005
)

// payloadLen is the length of the filler in each segment. The first segment
// has the app descriptor before it.
const payloadLen = 64

// buildImage builds an ESP app image the way esptool's elf2image lays it
// out, following esp_image_format.h.
func buildImage(s imageSpec) []byte {
	if s.chip == 0 {
		s.chip = chipS3
	}
	if s.version == "" {
		s.version = "1.8.19"
	}
	if s.idf == "" {
		s.idf = "v5.3.2"
	}
	if s.segments == 0 {
		s.segments = 2
	}
	var hdr [imageHeaderLen]byte
	hdr[0] = imageMagic
	if s.magic != 0 {
		hdr[0] = s.magic
	}
	hdr[offSegmentCount] = s.segments
	binary.LittleEndian.PutUint16(hdr[offChipID:], s.chip)
	if !s.noHash {
		hdr[offHashAppended] = 1
	}
	out := slices.Clone(hdr[:])
	sum := byte(checksumInit)
	for i := range s.segments {
		data := bytes.Repeat([]byte{0x31 + i}, payloadLen)
		dataLen := uint32(payloadLen)
		if i == 0 {
			dataLen += appDescLen
			data = append(make([]byte, appDescLen), data...)
			desc := data[:appDescLen]
			magic := uint32(appDescMagic)
			if s.descMagic != 0 {
				magic = s.descMagic
			}
			if !s.noDescriptor {
				binary.LittleEndian.PutUint32(desc, magic)
			}
			copy(desc[offDescVersion:], s.version)
			copy(desc[offDescProject:], "trmnl-firmware")
			copy(desc[offDescIDF:], s.idf)
		}
		out = binary.LittleEndian.AppendUint32(out, 0x3C000000+uint32(i)*0x100000)
		out = binary.LittleEndian.AppendUint32(out, dataLen)
		out = append(out, data...)
		for _, b := range data {
			sum ^= b
		}
	}
	pad := make([]byte, (len(out)+16)&^15-len(out))
	pad[len(pad)-1] = sum
	if s.badChecksum {
		pad[len(pad)-1] ^= 0xFF
	}
	out = append(out, pad...)
	if !s.noHash {
		h := sha256.Sum256(out)
		if s.badHash {
			h[0] ^= 0xFF
		}
		out = append(out, h[:]...)
	}
	return out
}

func TestParseImage(t *testing.T) {
	t.Parallel()
	good := buildImage(imageSpec{})
	for _, tc := range []struct {
		name string
		data []byte
		// want is a substring of the error, or "" for success.
		want string
	}{
		{"good with hash", good, ""},
		{"good without hash", buildImage(imageSpec{noHash: true}), ""},
		{"good with five segments", buildImage(imageSpec{segments: 5}), ""},
		{"wrong magic", buildImage(imageSpec{magic: 0xE8}), "does not start with the image header"},
		{"bad descriptor magic", buildImage(imageSpec{descMagic: 0x12345678}), "no valid app descriptor"},
		{"no descriptor", buildImage(imageSpec{noDescriptor: true}), "no valid app descriptor"},
		{"bad checksum", buildImage(imageSpec{badChecksum: true, noHash: true}), "its checksum does not match"},
		{"bad appended hash", buildImage(imageSpec{badHash: true}), "SHA-256"},
		{"corrupt data", func() []byte { b := bytes.Clone(good); b[100] ^= 1; return b }(), "its checksum does not match"},
		{"too short for a header", good[:10], "the file is too small to be an ESP firmware image"},
		{"header only", good[:imageHeaderLen], "the file is cut short"},
		{"ends inside a segment", good[:150], "runs past its end"},
		{"ends before the checksum", good[:len(good)-hashLen-3], "ends before the image's checksum"},
		{"ends inside the hash", good[:len(good)-5], "the file is cut short"},
		{"empty", nil, "the file is too small to be an ESP firmware image"},
		{"too many segments", func() []byte { b := bytes.Clone(good); b[offSegmentCount] = 17; return b }(), "lists 17 segments"},
		{"no segments", func() []byte { b := bytes.Clone(good); b[offSegmentCount] = 0; return b }(), "lists 0 segments"},
		{"bad hash_appended", func() []byte { b := bytes.Clone(good); b[offHashAppended] = 2; return b }(), "hash flag is 2"},
		{"binary version string", func() []byte {
			b := bytes.Clone(good)
			b[appDescOffset+offDescVersion] = 0x01
			return b
		}(), "not plain text"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			img, err := ParseImage(bytes.NewReader(tc.data), int64(len(tc.data)))
			if tc.want == "" {
				if err != nil || img.ChipID != chipS3 || img.Version != "1.8.19" || img.IDFVersion != "v5.3.2" || img.Project != "trmnl-firmware" {
					t.Fatalf("got %+v, %v", img, err)
				}
				return
			}
			if !errors.Is(err, ErrImage) || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want ErrImage containing %q", err, tc.want)
			}
		})
	}
}

func TestParseVersion(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		in   string
		want string // "" when it must be refused
	}{
		{"1.8.19", "1.8.19"},
		{"1.8", "1.8"},
		{"2", "2"},
		{"01.08.019", "1.8.19"},
		{"1.2.3.4", "1.2.3.4"},
		{"", ""},
		{"1.", ""},
		{".1", ""},
		{"1..2", ""},
		{"v1.8.19", ""},
		{"1.8.19-dev", ""},
		{"1.8.19 ", ""},
		{"-1.8", ""},
		{"+1.8", ""},
		{"1.2.3.4.5", ""},
		{"4294967296", ""},
		{"1234567890", ""},
		{"1.8.x", ""},
	} {
		v, err := ParseVersion(tc.in)
		if tc.want == "" {
			if !errors.Is(err, errkind.ErrInvalid) {
				t.Errorf("ParseVersion(%q) = %v, %v; want errkind.ErrInvalid", tc.in, v, err)
			}
			continue
		}
		if err != nil || v.String() != tc.want {
			t.Errorf("ParseVersion(%q) = %v, %v; want %s", tc.in, v, err, tc.want)
		}
	}
}

func TestVersionCompare(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		a, b string
		want int
	}{
		{"1.8.19", "1.8.19", 0},
		{"1.8.19", "1.8.2", 1},
		{"1.8.2", "1.8.19", -1},
		{"1.8", "1.8.0", 0},
		{"1.8", "1.8.1", -1},
		{"1.10.0", "1.9.9", 1},
		{"2", "1.99.99", 1},
	} {
		a, _ := ParseVersion(tc.a)
		b, _ := ParseVersion(tc.b)
		if got := a.Compare(b); got != tc.want {
			t.Errorf("%s.Compare(%s) = %d, want %d", tc.a, tc.b, got, tc.want)
		}
	}
}

func TestCleanDescriptorVersion(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		in string
		ok bool
	}{
		{"1.8.19", true},
		{"v1.8.19", true},
		{"1", false},
		{"v1.8.19-5-g1a2b3c4", false},
		{"1a2b3c4", false},
		{"", false},
		{"1.8", false},
	} {
		if _, ok := cleanDescriptorVersion(tc.in); ok != tc.ok {
			t.Errorf("cleanDescriptorVersion(%q) = %v, want %v", tc.in, ok, tc.ok)
		}
	}
}

// TestModelsAreSane checks the table against facts that must hold for every
// entry, so an edit cannot add a model with an impossible size.
func TestModelsAreSane(t *testing.T) {
	t.Parallel()
	for name, m := range Models {
		if name == "" || m.Chip.Name == "" || m.MaxBytes < 1<<20 || m.MaxBytes > 0x400000 {
			t.Errorf("model %q = %+v", name, m)
		}
	}
	// Before 1.8.17 a Model header did not name one build.
	if minReportedVersion.Compare(Version{1, 8, 17}) != 0 {
		t.Errorf("minReportedVersion = %v", minReportedVersion)
	}
	if _, ok := Models["xteink_x4"]; ok {
		t.Error("xteink_x4 is two builds and must not be in the table")
	}
}
