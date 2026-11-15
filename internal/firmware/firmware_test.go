package firmware

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/die-net/trmnl-byos-go/internal/db"
	"github.com/die-net/trmnl-byos-go/internal/device"
	"github.com/die-net/trmnl-byos-go/internal/errkind"
)

const (
	testModel = "xiao_epaper_display"
	canaryID  = "ABC123"
	otherID   = "DEF456"
)

var testNow = time.Unix(1_800_000_000, 0)

type fixture struct {
	t      *testing.T
	client *db.Client
	dir    string
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	client, err := db.Open(t.Context(), filepath.Join(t.TempDir(), "trmnl.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	return &fixture{t: t, client: client, dir: filepath.Join(t.TempDir(), "firmware")}
}

// device makes a device of the test model that reports the version, with
// updates on.
func (f *fixture) device(friendly, mac, model, version string) {
	f.t.Helper()
	ctx := f.t.Context()
	_, err := f.client.Write.CreateDevice(ctx, db.CreateDeviceParams{
		Mac: mac, ApiKeyHash: []byte(friendly), FriendlyID: friendly, FwModel: model, FwVersion: version, CreatedAt: 1,
	})
	if err != nil {
		f.t.Fatal(err)
	}
	if _, err := SetPolicy(ctx, f.client, friendly, Policy{UpdatesEnabled: ptr(true)}); err != nil {
		f.t.Fatal(err)
	}
}

func ptr[T any](v T) *T { return &v }

// add adds a good image for the test model at the version.
func (f *fixture) add(version string) db.Firmware {
	f.t.Helper()
	fw, err := Add(f.t.Context(), f.client, f.dir, testModel, version, bytes.NewReader(buildImage(imageSpec{version: version})), testNow)
	if err != nil {
		f.t.Fatal(err)
	}
	return fw
}

// reportCanary records what the canary reported, as a display request does.
func (f *fixture) reportCanary(model, version string) {
	f.t.Helper()
	dev, err := device.FindDevice(f.t.Context(), f.client.Read, canaryID)
	if err != nil {
		f.t.Fatal(err)
	}
	err = f.client.Write.UpdateDeviceFirmware(f.t.Context(), db.UpdateDeviceFirmwareParams{FwModel: model, FwVersion: version, ID: dev.ID})
	if err != nil {
		f.t.Fatal(err)
	}
}

// files lists the names in the firmware directory.
func (f *fixture) files() []string {
	f.t.Helper()
	entries, err := os.ReadDir(f.dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		f.t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

func TestAdd(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	image := buildImage(imageSpec{version: "1.8.19"})
	sum := sha256.Sum256(image)
	fw, err := Add(t.Context(), f.client, f.dir, testModel, "1.8.19", bytes.NewReader(image), testNow)
	if err != nil {
		t.Fatal(err)
	}
	if fw.FwModel != testModel || fw.Version != "1.8.19" || fw.Chip != "esp32s3" || fw.IdfVersion != "v5.3.2" ||
		fw.Bytes != int64(len(image)) || fw.Sha256 != hex.EncodeToString(sum[:]) || fw.Source != SourceUpload || fw.UploadedAt != testNow.Unix() {
		t.Errorf("firmware = %+v", fw)
	}
	got, err := os.ReadFile(Path(f.dir, fw.Sha256))
	if err != nil || !bytes.Equal(got, image) {
		t.Errorf("stored file differs: %v", err)
	}
	if names := f.files(); len(names) != 1 || names[0] != fw.Sha256+".bin" {
		t.Errorf("files = %v", names)
	}
}

// TestAddVersions checks which version is recorded: the given one, or the
// descriptor's when it is a plain X.Y.Z.
func TestAddVersions(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, given, descriptor, want string
	}{
		{"given, descriptor is a commit", "1.8.19", "1a2b3c4", "1.8.19"},
		{"given, descriptor is 1", "1.8.19", "1", "1.8.19"},
		{"given, descriptor agrees", "1.8.19", "1.8.19", "1.8.19"},
		{"given, descriptor agrees with a v", "1.8.19", "v1.8.19", "1.8.19"},
		{"not given, descriptor is plain", "", "1.8.19", "1.8.19"},
		{"not given, descriptor has a v", "", "v1.8.19", "1.8.19"},
		{"given with zeros is normalized", "1.08.19", "1a2b3c4", "1.8.19"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newFixture(t)
			fw, err := Add(t.Context(), f.client, f.dir, testModel, tc.given, bytes.NewReader(buildImage(imageSpec{version: tc.descriptor})), testNow)
			if err != nil || fw.Version != tc.want {
				t.Fatalf("version = %q, %v; want %s", fw.Version, err, tc.want)
			}
		})
	}
}

// zeros reads n zero bytes.
type zeros int64

func (z *zeros) Read(p []byte) (int, error) {
	if *z <= 0 {
		return 0, io.EOF
	}
	n := min(int64(len(p)), int64(*z))
	clear(p[:n])
	*z -= zeros(n)
	return int(n), nil
}

// TestAddRefusals checks that each rule refuses an upload, with the right
// kind of error, and that a refused upload leaves no file and no row.
func TestAddRefusals(t *testing.T) {
	t.Parallel()
	good := buildImage(imageSpec{version: "1.8.19"})
	oversize := zeros(Models["og"].MaxBytes + 1)
	for _, tc := range []struct {
		name    string
		model   string
		version string
		body    io.Reader
		want    error
	}{
		{"unknown model", "no_such_model", "1.8.19", bytes.NewReader(good), errkind.ErrInvalid},
		{"empty model", "", "1.8.19", bytes.NewReader(good), errkind.ErrInvalid},
		{"model left out for two builds", "xteink_x4", "1.8.19", bytes.NewReader(good), errkind.ErrInvalid},
		{"wrong chip for the model", "og", "1.8.19", bytes.NewReader(good), ErrImage},
		{"C3 image for an S3 model", testModel, "1.8.19", bytes.NewReader(buildImage(imageSpec{chip: chipC3})), ErrImage},
		{"wrong magic", testModel, "1.8.19", bytes.NewReader(buildImage(imageSpec{magic: 0x7F})), ErrImage},
		{"no descriptor", testModel, "1.8.19", bytes.NewReader(buildImage(imageSpec{noDescriptor: true})), ErrImage},
		{"bad descriptor magic", testModel, "1.8.19", bytes.NewReader(buildImage(imageSpec{descMagic: 1})), ErrImage},
		{"truncated", testModel, "1.8.19", bytes.NewReader(good[:len(good)/2]), ErrImage},
		{"corrupt checksum", testModel, "1.8.19", bytes.NewReader(buildImage(imageSpec{badChecksum: true, noHash: true})), ErrImage},
		{"empty file", testModel, "1.8.19", bytes.NewReader(nil), ErrImage},
		{"not an image", testModel, "1.8.19", bytes.NewReader([]byte("hello")), ErrImage},
		{"version not dotted numbers", testModel, "1.8.x", bytes.NewReader(good), errkind.ErrInvalid},
		{"version with a prefix", testModel, "v1.8.19", bytes.NewReader(good), errkind.ErrInvalid},
		{"version differs from the descriptor", testModel, "1.8.20", bytes.NewReader(good), errkind.ErrInvalid},
		{"no version, descriptor is no version", testModel, "", bytes.NewReader(buildImage(imageSpec{version: "1a2b3c4"})), errkind.ErrInvalid},
		{"no version, descriptor is 1", testModel, "", bytes.NewReader(buildImage(imageSpec{version: "1"})), errkind.ErrInvalid},
		{"larger than the app partition", "og", "1.8.19", &oversize, ErrTooBig},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newFixture(t)
			_, err := Add(t.Context(), f.client, f.dir, tc.model, tc.version, tc.body, testNow)
			if !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
			if names := f.files(); len(names) != 0 {
				t.Errorf("a refused upload left %v", names)
			}
			if rows, err := List(t.Context(), f.client.Read); err != nil || len(rows) != 0 {
				t.Errorf("a refused upload left rows %v, %v", rows, err)
			}
		})
	}
}

func TestAddDuplicates(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.add("1.8.19")
	// The same file again, and a different file for the same version.
	other := buildImage(imageSpec{version: "1.8.19", idf: "v5.4"})
	for name, body := range map[string][]byte{
		"same file":    buildImage(imageSpec{version: "1.8.19"}),
		"same version": other,
	} {
		_, err := Add(t.Context(), f.client, f.dir, testModel, "1.8.19", bytes.NewReader(body), testNow)
		if !errors.Is(err, ErrExists) {
			t.Errorf("%s: err = %v, want ErrExists", name, err)
		}
	}
	if names := f.files(); len(names) != 1 {
		t.Errorf("files = %v", names)
	}
	// Another version of the same model is fine.
	f.add("1.8.20")
}

// TestAddEqualVersions checks that a version that compares equal to a
// build's, though it is spelled differently, is a conflict that leaves no
// file and no row, while another model may have the same version.
func TestAddEqualVersions(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ have, add string }{
		{"1.8.19", "1.8.19.0"},
		{"1.8.19.0", "1.8.19"},
	} {
		t.Run(tc.have+" then "+tc.add, func(t *testing.T) {
			t.Parallel()
			f := newFixture(t)
			existing := f.add(tc.have)
			body := buildImage(imageSpec{version: "1.8.19", idf: "v5.4"})
			_, err := Add(t.Context(), f.client, f.dir, testModel, tc.add, bytes.NewReader(body), testNow)
			if !errors.Is(err, ErrExists) || !errors.Is(err, errkind.ErrConflict) {
				t.Fatalf("err = %v, want ErrExists, a conflict", err)
			}
			if !strings.Contains(err.Error(), existing.Version) {
				t.Errorf("err = %q, want it to name %s", err, existing.Version)
			}
			if names := f.files(); len(names) != 1 {
				t.Errorf("files = %v", names)
			}
			if rows, err := List(t.Context(), f.client.Read); err != nil || len(rows) != 1 {
				t.Errorf("rows = %v, %v", rows, err)
			}
			// Another model may have the same version.
			other := buildImage(imageSpec{version: "1.8.19", idf: "v5.4"})
			if _, err := Add(t.Context(), f.client, f.dir, "reterminal_e1001", tc.add, bytes.NewReader(other), testNow); err != nil {
				t.Errorf("another model: %v", err)
			}
		})
	}
}

func TestStartRollout(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		// canary is how the canary device is set up.
		model, version string
		updates        bool
		pin            string
		wantErr        bool
	}{
		{"eligible", testModel, "1.8.17", true, "", false},
		{"updates off", testModel, "1.8.17", false, "", true},
		{"pinned", testModel, "1.8.17", true, "1.8.17", true},
		{"other model", "reterminal_e1001", "1.8.17", true, "", true},
		{"unknown model", "mystery", "1.8.17", true, "", true},
		{"already on the version", testModel, "1.8.19", true, "", true},
		{"already newer", testModel, "1.8.20", true, "", true},
		{"unparsable version", testModel, "1.8.17-dev", true, "", true},
		{"firmware too old to trust its model", testModel, "1.8.16", true, "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newFixture(t)
			f.device(canaryID, "AA:00:00:00:00:01", tc.model, tc.version)
			if _, err := SetPolicy(t.Context(), f.client, canaryID, Policy{UpdatesEnabled: &tc.updates, Pin: &tc.pin}); err != nil {
				t.Fatal(err)
			}
			fw := f.add("1.8.19")
			e, err := StartRollout(t.Context(), f.client, fw.ID, canaryID, testNow)
			if tc.wantErr {
				if !errors.Is(err, errkind.ErrInvalid) {
					t.Fatalf("err = %v, want errkind.ErrInvalid", err)
				}
				if rows, _ := List(t.Context(), f.client.Read); rows[0].Stage != "" {
					t.Errorf("a refused rollout left stage %q", rows[0].Stage)
				}
				return
			}
			if err != nil || e.Stage != "canary" || e.Canary != canaryID || e.StartedAt != testNow.Unix() {
				t.Fatalf("entry = %+v, %v", e, err)
			}
		})
	}
}

func TestStartRolloutErrors(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.device(canaryID, "AA:00:00:00:00:01", testModel, "1.8.17")
	fw := f.add("1.8.19")
	if _, err := StartRollout(t.Context(), f.client, fw.ID+1, canaryID, testNow); !errors.Is(err, ErrNoFirmware) {
		t.Errorf("unknown firmware: %v", err)
	}
	if _, err := StartRollout(t.Context(), f.client, fw.ID, "ZZZ999", testNow); err == nil {
		t.Error("unknown canary: no error")
	}
	if err := CancelRollout(t.Context(), f.client, fw.ID); !errors.Is(err, errkind.ErrInvalid) {
		t.Errorf("cancel with no rollout: %v", err)
	}
	if err := CancelRollout(t.Context(), f.client, fw.ID+1); !errors.Is(err, ErrNoFirmware) {
		t.Errorf("cancel unknown: %v", err)
	}
}

func TestSetPolicy(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.device(canaryID, "AA:00:00:00:00:01", testModel, "1.8.17")
	dev, err := SetPolicy(t.Context(), f.client, canaryID, Policy{Pin: ptr("1.08.17")})
	if err != nil || dev.FwPin != "1.8.17" || dev.UpdatesEnabled != 1 {
		t.Fatalf("pin: %+v, %v", dev, err)
	}
	dev, err = SetPolicy(t.Context(), f.client, canaryID, Policy{UpdatesEnabled: ptr(false), Pin: ptr("")})
	if err != nil || dev.FwPin != "" || dev.UpdatesEnabled != 0 {
		t.Fatalf("clear: %+v, %v", dev, err)
	}
	got, _ := device.FindDevice(t.Context(), f.client.Read, canaryID)
	if got.FwPin != "" || got.UpdatesEnabled != 0 {
		t.Errorf("stored %+v", got)
	}
	if _, err := SetPolicy(t.Context(), f.client, canaryID, Policy{Pin: ptr("latest")}); !errors.Is(err, errkind.ErrInvalid) {
		t.Errorf("bad pin: %v", err)
	}
}

// TestOffer is the offer matrix. The build is 1.8.19 for the test model.
// The canary is ABC123 and the other device DEF456. The canary is on
// 1.8.17 when the rollout starts, then becomes canaryNow.
func TestOffer(t *testing.T) {
	t.Parallel()
	type dev struct{ model, version string }
	for _, tc := range []struct {
		name string
		// rollout is "none", "active" or "canceled".
		rollout string
		// canaryNow is what the canary reports after the rollout starts.
		canaryNow dev
		// asks is the device that asks, and what it reports.
		asksOther  bool
		reports    dev
		updatesOff bool
		pin        string
		want       bool
	}{
		{"canary on an older version", "active", dev{testModel, "1.8.17"}, false, dev{testModel, "1.8.17"}, false, "", true},
		{"canary on 1.8.18", "active", dev{testModel, "1.8.18"}, false, dev{testModel, "1.8.18"}, false, "", true},
		{"canary has updates off", "active", dev{testModel, "1.8.17"}, false, dev{testModel, "1.8.17"}, true, "", false},
		{"canary is pinned", "active", dev{testModel, "1.8.17"}, false, dev{testModel, "1.8.17"}, false, "1.8.17", false},
		{"canary on the build's version", "active", dev{testModel, "1.8.19"}, false, dev{testModel, "1.8.19"}, false, "", false},
		{"canary on a newer version", "active", dev{testModel, "1.8.20"}, false, dev{testModel, "1.8.20"}, false, "", false},
		{"canary reports another model", "active", dev{testModel, "1.8.17"}, false, dev{"og", "1.8.17"}, false, "", false},
		{"canary reports an unknown model", "active", dev{testModel, "1.8.17"}, false, dev{"mystery", "1.8.17"}, false, "", false},
		{"canary reports no model", "active", dev{testModel, "1.8.17"}, false, dev{"", "1.8.17"}, false, "", false},
		{"canary reports an unparsable version", "active", dev{testModel, "1.8.17"}, false, dev{testModel, "1.8.17-dev"}, false, "", false},
		{"canary reports no version", "active", dev{testModel, "1.8.17"}, false, dev{testModel, ""}, false, "", false},
		{"canary reports a version too old to trust", "active", dev{testModel, "1.8.17"}, false, dev{testModel, "1.8.16"}, false, "", false},
		{"no rollout", "none", dev{testModel, "1.8.17"}, false, dev{testModel, "1.8.17"}, false, "", false},
		{"canceled rollout, canary", "canceled", dev{testModel, "1.8.17"}, false, dev{testModel, "1.8.17"}, false, "", false},
		{"canceled rollout, other after canary reported", "canceled", dev{testModel, "1.8.19"}, true, dev{testModel, "1.8.17"}, false, "", false},

		{"other before canary reports", "active", dev{testModel, "1.8.17"}, true, dev{testModel, "1.8.17"}, false, "", false},
		{"other after canary reports", "active", dev{testModel, "1.8.19"}, true, dev{testModel, "1.8.17"}, false, "", true},
		{"other after canary reports, device on 1.8.18", "active", dev{testModel, "1.8.19"}, true, dev{testModel, "1.8.18"}, false, "", true},
		{"other after canary reports another version", "active", dev{testModel, "1.8.18"}, true, dev{testModel, "1.8.17"}, false, "", false},
		{"other after canary moved past the build", "active", dev{testModel, "1.8.20"}, true, dev{testModel, "1.8.17"}, false, "", true},
		{"other after canary reports another model", "active", dev{"og", "1.8.19"}, true, dev{testModel, "1.8.17"}, false, "", false},
		{"other, updates off", "active", dev{testModel, "1.8.19"}, true, dev{testModel, "1.8.17"}, true, "", false},
		{"other, pinned", "active", dev{testModel, "1.8.19"}, true, dev{testModel, "1.8.17"}, false, "1.8.17", false},
		{"other on the same version", "active", dev{testModel, "1.8.19"}, true, dev{testModel, "1.8.19"}, false, "", false},
		{"other on a newer version", "active", dev{testModel, "1.8.19"}, true, dev{testModel, "1.8.25"}, false, "", false},
		{"other of another model", "active", dev{testModel, "1.8.19"}, true, dev{"reterminal_e1001", "1.8.17"}, false, "", false},
		{"other of an unknown model", "active", dev{testModel, "1.8.19"}, true, dev{"mystery", "1.8.17"}, false, "", false},
		{"other of a family name", "active", dev{testModel, "1.8.19"}, true, dev{"xiao", "1.8.17"}, false, "", false},
		{"other with an unparsable version", "active", dev{testModel, "1.8.19"}, true, dev{testModel, "1.8.17-dev"}, false, "", false},
		{"other with a v prefix", "active", dev{testModel, "1.8.19"}, true, dev{testModel, "v1.8.17"}, false, "", false},
		{"other with no version", "active", dev{testModel, "1.8.19"}, true, dev{testModel, ""}, false, "", false},
		{"other on firmware too old to trust its model", "active", dev{testModel, "1.8.19"}, true, dev{testModel, "1.8.10"}, false, "", false},
		{"other, canceled after canary reported", "canceled", dev{testModel, "1.8.19"}, true, dev{testModel, "1.8.17"}, false, "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newFixture(t)
			f.device(canaryID, "AA:00:00:00:00:01", testModel, "1.8.17")
			f.device(otherID, "AA:00:00:00:00:02", testModel, "1.8.17")
			fw := f.add("1.8.19")
			if tc.rollout != "none" {
				if _, err := StartRollout(t.Context(), f.client, fw.ID, canaryID, testNow); err != nil {
					t.Fatal(err)
				}
			}
			if tc.rollout == "canceled" {
				if err := CancelRollout(t.Context(), f.client, fw.ID); err != nil {
					t.Fatal(err)
				}
			}
			f.reportCanary(tc.canaryNow.model, tc.canaryNow.version)
			asks := canaryID
			if tc.asksOther {
				asks = otherID
			}
			if _, err := SetPolicy(t.Context(), f.client, asks, Policy{UpdatesEnabled: ptr(!tc.updatesOff), Pin: &tc.pin}); err != nil {
				t.Fatal(err)
			}
			d, err := device.FindDevice(t.Context(), f.client.Read, asks)
			if err != nil {
				t.Fatal(err)
			}
			got, err := Offer(t.Context(), f.client.Read, f.dir, &d, tc.reports.model, tc.reports.version)
			if err != nil {
				t.Fatal(err)
			}
			if (got != nil) != tc.want {
				t.Fatalf("offer = %+v, want offer %v", got, tc.want)
			}
			if got != nil && got.ID != fw.ID {
				t.Errorf("offered %+v, want %+v", got, fw)
			}
		})
	}
}

// TestOfferNewest checks that of several builds in active rollouts, the
// newest that qualifies is offered, never an older one or the same one.
func TestOfferNewest(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.device(canaryID, "AA:00:00:00:00:01", testModel, "1.8.17")
	f.device(otherID, "AA:00:00:00:00:02", testModel, "1.8.17")
	b18 := f.add("1.8.18")
	b19 := f.add("1.8.19")
	b20 := f.add("1.8.20")
	for _, fw := range []db.Firmware{b18, b19, b20} {
		if _, err := StartRollout(t.Context(), f.client, fw.ID, canaryID, testNow); err != nil {
			t.Fatal(err)
		}
	}
	offer := func(who, version string) string {
		t.Helper()
		d, err := device.FindDevice(t.Context(), f.client.Read, who)
		if err != nil {
			t.Fatal(err)
		}
		fw, err := Offer(t.Context(), f.client.Read, f.dir, &d, testModel, version)
		if err != nil {
			t.Fatal(err)
		}
		if fw == nil {
			return ""
		}
		return fw.Version
	}
	if got := offer(canaryID, "1.8.17"); got != "1.8.20" {
		t.Errorf("canary offered %q, want 1.8.20", got)
	}
	if got := offer(canaryID, "1.8.20"); got != "" {
		t.Errorf("canary on the newest offered %q", got)
	}
	if got := offer(canaryID, "1.8.19"); got != "1.8.20" {
		t.Errorf("canary on 1.8.19 offered %q, want 1.8.20", got)
	}
	if got := offer(otherID, "1.8.17"); got != "" {
		t.Errorf("other offered %q before any canary report", got)
	}
	// The canary reports 1.8.19: only that build is open, the newer one is
	// not, and the other device takes it.
	f.reportCanary(testModel, "1.8.19")
	if got := offer(otherID, "1.8.17"); got != "1.8.19" {
		t.Errorf("other offered %q, want 1.8.19", got)
	}
	f.reportCanary(testModel, "1.8.20")
	if got := offer(otherID, "1.8.17"); got != "1.8.20" {
		t.Errorf("other offered %q, want 1.8.20", got)
	}
}

// TestOfferNeedsTheFile checks that a build whose file is missing or is not
// the size that was added is never offered.
func TestOfferNeedsTheFile(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.device(canaryID, "AA:00:00:00:00:01", testModel, "1.8.17")
	fw := f.add("1.8.19")
	if _, err := StartRollout(t.Context(), f.client, fw.ID, canaryID, testNow); err != nil {
		t.Fatal(err)
	}
	d, _ := device.FindDevice(t.Context(), f.client.Read, canaryID)
	if got, err := Offer(t.Context(), f.client.Read, f.dir, &d, testModel, "1.8.17"); err != nil || got == nil {
		t.Fatalf("offer = %v, %v", got, err)
	}
	path := Path(f.dir, fw.Sha256)
	if err := os.WriteFile(path, []byte("short"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, _ := Offer(t.Context(), f.client.Read, f.dir, &d, testModel, "1.8.17"); got != nil {
		t.Error("offered a file of the wrong size")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if got, _ := Offer(t.Context(), f.client.Read, f.dir, &d, testModel, "1.8.17"); got != nil {
		t.Error("offered a missing file")
	}
}

func TestOpen(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.device(canaryID, "AA:00:00:00:00:01", testModel, "1.8.17")
	image := buildImage(imageSpec{version: "1.8.19"})
	fw := f.add("1.8.19")
	open := func(sha string) error {
		file, _, err := Open(t.Context(), f.client.Read, f.dir, sha)
		if err == nil {
			_ = file.Close()
		}
		return err
	}
	if err := open(fw.Sha256); !errors.Is(err, ErrNoFirmware) {
		t.Errorf("not in a rollout: %v", err)
	}
	if _, err := StartRollout(t.Context(), f.client, fw.ID, canaryID, testNow); err != nil {
		t.Fatal(err)
	}
	file, got, err := Open(t.Context(), f.client.Read, f.dir, fw.Sha256)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(file)
	_ = file.Close()
	if err != nil || !bytes.Equal(body, image) || got.ID != fw.ID {
		t.Errorf("open: %d bytes, %v", len(body), err)
	}
	for name, sha := range map[string]string{
		"unknown hash":    sha256Hex("nothing"),
		"upper case":      upper(fw.Sha256),
		"short":           fw.Sha256[:63],
		"long":            fw.Sha256 + "0",
		"path":            "../" + fw.Sha256[3:],
		"empty":           "",
		"slash":           fw.Sha256[:10] + "/" + fw.Sha256[11:],
		"non hex":         "g" + fw.Sha256[1:],
		"with extension":  fw.Sha256 + ".bin",
		"leading percent": "%2e" + fw.Sha256[3:],
	} {
		if err := open(sha); !errors.Is(err, ErrNoFirmware) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if err := CancelRollout(t.Context(), f.client, fw.ID); err != nil {
		t.Fatal(err)
	}
	if err := open(fw.Sha256); !errors.Is(err, ErrNoFirmware) {
		t.Errorf("canceled: %v", err)
	}
}

func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func upper(s string) string {
	b := []byte(s)
	for i, c := range b {
		if c >= 'a' && c <= 'f' {
			b[i] = c - 'a' + 'A'
		}
	}
	return string(b)
}

func TestList(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.device(canaryID, "AA:00:00:00:00:01", testModel, "1.8.17")
	a, b := f.add("1.8.18"), f.add("1.8.19")
	if _, err := StartRollout(t.Context(), f.client, b.ID, canaryID, testNow); err != nil {
		t.Fatal(err)
	}
	stages := func() []string {
		rows, err := List(t.Context(), f.client.Read)
		if err != nil {
			t.Fatal(err)
		}
		out := make([]string, 0, len(rows))
		for _, r := range rows {
			out = append(out, strconv.FormatInt(r.ID, 10)+":"+r.Stage)
		}
		return out
	}
	want := func(s ...string) {
		t.Helper()
		got := stages()
		if len(got) != len(s) {
			t.Fatalf("stages = %v, want %v", got, s)
		}
		for i := range s {
			if got[i] != s[i] {
				t.Fatalf("stages = %v, want %v", got, s)
			}
		}
	}
	id := func(x db.Firmware) string { return strconv.FormatInt(x.ID, 10) }
	want(id(a)+":", id(b)+":canary")
	lastOffered := func() int64 {
		t.Helper()
		rows, err := List(t.Context(), f.client.Read)
		if err != nil {
			t.Fatal(err)
		}
		return rows[1].CanaryLastOfferedAt
	}
	if got := lastOffered(); got != 0 {
		t.Errorf("last offered before any offer = %d, want 0", got)
	}
	dev, err := device.FindDevice(t.Context(), f.client.Read, canaryID)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.client.Write.RecordFirmwareOffer(t.Context(), db.RecordFirmwareOfferParams{DeviceID: dev.ID, FirmwareID: b.ID, LastOfferedAt: 1234}); err != nil {
		t.Fatal(err)
	}
	if got := lastOffered(); got != 1234 {
		t.Errorf("last offered = %d, want 1234", got)
	}
	f.reportCanary(testModel, "1.8.19")
	want(id(a)+":", id(b)+":open")
	f.reportCanary(testModel, "1.8.20")
	want(id(a)+":", id(b)+":open") // Past the build is as good as on it.
	if err := CancelRollout(t.Context(), f.client, b.ID); err != nil {
		t.Fatal(err)
	}
	want(id(a)+":", id(b)+":canceled")
}

func TestValidSHA(t *testing.T) {
	t.Parallel()
	good := sha256Hex("x")
	for s, want := range map[string]bool{
		good:               true,
		"":                 false,
		good[:63]:          false,
		good + "0":         false,
		upper(good):        false,
		"../" + good[3:]:   false,
		good[:63] + "g":    false,
		good[:63] + "\x00": false,
	} {
		if got := ValidSHA(s); got != want {
			t.Errorf("ValidSHA(%q) = %v, want %v", s, got, want)
		}
	}
	if sha, ok := SHAFromName(good + ".bin"); !ok || sha != good {
		t.Errorf("SHAFromName = %q, %v", sha, ok)
	}
	for _, name := range []string{good, good + ".png", ".bin", "x.bin", good + ".bin.bin"} {
		if _, ok := SHAFromName(name); ok {
			t.Errorf("SHAFromName(%q) accepted", name)
		}
	}
}

// TestOfferCap checks a build that never takes, as one added under the wrong
// version label would not: the canary reports the same old version after
// every update. It is offered MaxOffers times and then never, the rollout
// stalls, and no other device is offered it.
func TestOfferCap(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.device(canaryID, "AA:00:00:00:00:01", testModel, "1.8.17")
	f.device(otherID, "AA:00:00:00:00:02", testModel, "1.8.17")
	fw := f.add("1.8.19")
	if _, err := StartRollout(t.Context(), f.client, fw.ID, canaryID, testNow); err != nil {
		t.Fatal(err)
	}
	wake := func(who string) bool {
		t.Helper()
		d, err := device.FindDevice(t.Context(), f.client.Read, who)
		if err != nil {
			t.Fatal(err)
		}
		got, err := OfferAndRecord(t.Context(), f.client, f.dir, &d, testModel, "1.8.17", testNow)
		if err != nil {
			t.Fatal(err)
		}
		return got != nil
	}
	stage := func() Entry {
		t.Helper()
		e, err := Get(t.Context(), f.client.Read, fw.ID)
		if err != nil {
			t.Fatal(err)
		}
		return e
	}
	for i := range MaxOffers {
		if e := stage(); e.Stage != "canary" {
			t.Fatalf("before offer %d: stage %q", i+1, e.Stage)
		}
		if !wake(canaryID) {
			t.Fatalf("offer %d was not made", i+1)
		}
	}
	for range 3 {
		if wake(canaryID) {
			t.Fatal("offered after the cap")
		}
	}
	if e := stage(); e.Stage != "stalled" || e.Canary != canaryID || e.CanaryVersion != "1.8.17" {
		t.Errorf("entry %+v, want stalled with the canary's version", e)
	}
	if wake(otherID) {
		t.Error("another device was offered a stalled rollout")
	}
	// The same canary again is an explicit retry: three more offers.
	if _, err := StartRollout(t.Context(), f.client, fw.ID, canaryID, testNow); err != nil {
		t.Fatal(err)
	}
	if e := stage(); e.Stage != "canary" {
		t.Errorf("after restart: stage %q", e.Stage)
	}
	for i := range MaxOffers {
		if !wake(canaryID) {
			t.Fatalf("retry offer %d was not made", i+1)
		}
	}
	if wake(canaryID) {
		t.Error("offered after the cap, second round")
	}
	// Another canary starts fresh, and the first one stays capped.
	if _, err := StartRollout(t.Context(), f.client, fw.ID, otherID, testNow); err != nil {
		t.Fatal(err)
	}
	if wake(canaryID) {
		t.Error("the capped device was offered the build again")
	}
	if !wake(otherID) {
		t.Error("the new canary was not offered the build")
	}
}

// TestOfferAndRecordFiles checks that a build whose file is gone is skipped
// for the next newest one, is never counted, and that nothing is offered or
// counted when no file is left.
func TestOfferAndRecordFiles(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.device(canaryID, "AA:00:00:00:00:01", testModel, "1.8.17")
	older, newer := f.add("1.8.18"), f.add("1.8.19")
	for _, fw := range []db.Firmware{older, newer} {
		if _, err := StartRollout(t.Context(), f.client, fw.ID, canaryID, testNow); err != nil {
			t.Fatal(err)
		}
	}
	d, _ := device.FindDevice(t.Context(), f.client.Read, canaryID)
	offers := func(id int64) int64 {
		t.Helper()
		all, err := DeviceOffers(t.Context(), f.client.Read, f.dir, id)
		if err != nil {
			t.Fatal(err)
		}
		for _, o := range all {
			if o.DeviceID == d.ID {
				return o.Offers
			}
		}
		t.Fatalf("device %d not listed for build %d", d.ID, id)
		return 0
	}
	if err := os.Remove(Path(f.dir, newer.Sha256)); err != nil {
		t.Fatal(err)
	}
	got, err := OfferAndRecord(t.Context(), f.client, f.dir, &d, testModel, "1.8.17", testNow)
	if err != nil || got == nil || got.ID != older.ID {
		t.Fatalf("offered %v, %v; want the older build", got, err)
	}
	if offers(older.ID) != 1 || offers(newer.ID) != 0 {
		t.Errorf("counted %d and %d, want 1 and 0", offers(older.ID), offers(newer.ID))
	}
	if err := os.Remove(Path(f.dir, older.Sha256)); err != nil {
		t.Fatal(err)
	}
	if got, err := OfferAndRecord(t.Context(), f.client, f.dir, &d, testModel, "1.8.17", testNow); err != nil || got != nil {
		t.Fatalf("offered %v, %v with no file left", got, err)
	}
	if offers(older.ID) != 1 {
		t.Errorf("an offer with no file was counted: %d", offers(older.ID))
	}
}

// TestOfferCapCascade checks that deleting a device deletes its offers.
func TestOfferCapCascade(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.device(canaryID, "AA:00:00:00:00:01", testModel, "1.8.17")
	f.device(otherID, "AA:00:00:00:00:02", testModel, "1.8.17")
	fw := f.add("1.8.19")
	if _, err := StartRollout(t.Context(), f.client, fw.ID, canaryID, testNow); err != nil {
		t.Fatal(err)
	}
	d, _ := device.FindDevice(t.Context(), f.client.Read, canaryID)
	if got, err := OfferAndRecord(t.Context(), f.client, f.dir, &d, testModel, "1.8.17", testNow); err != nil || got == nil {
		t.Fatalf("offer %v, %v", got, err)
	}
	count := func() int64 {
		var n int64
		if err := f.client.Reader.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM firmware_offer`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if count() != 1 {
		t.Fatalf("offers = %d", count())
	}
	if _, err := f.client.Writer.ExecContext(t.Context(), `DELETE FROM device WHERE id = ?`, d.ID); err != nil {
		t.Fatal(err)
	}
	if count() != 0 {
		t.Errorf("a deleted device left %d offers", count())
	}
}
