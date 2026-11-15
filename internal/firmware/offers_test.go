package firmware

import (
	"errors"
	"os"
	"strconv"
	"testing"

	"github.com/die-net/trmnl-byos-go/internal/db"
	"github.com/die-net/trmnl-byos-go/internal/device"
)

// setVersion records what a device reported, as a display request does.
func (f *fixture) setVersion(friendly, version string) {
	f.t.Helper()
	dev, err := device.FindDevice(f.t.Context(), f.client.Read, friendly)
	if err != nil {
		f.t.Fatal(err)
	}
	err = f.client.Write.UpdateDeviceFirmware(f.t.Context(), db.UpdateDeviceFirmwareParams{FwModel: dev.FwModel, FwVersion: version, ID: dev.ID})
	if err != nil {
		f.t.Fatal(err)
	}
}

// start starts the build's rollout with the canary device.
func (f *fixture) start(fw db.Firmware) {
	f.t.Helper()
	if _, err := StartRollout(f.t.Context(), f.client, fw.ID, canaryID, testNow); err != nil {
		f.t.Fatal(err)
	}
}

// offered records that the device was offered the build n times.
func (f *fixture) offered(friendly string, fw db.Firmware, n int) {
	f.t.Helper()
	dev, err := device.FindDevice(f.t.Context(), f.client.Read, friendly)
	if err != nil {
		f.t.Fatal(err)
	}
	for range n {
		err := f.client.Write.RecordFirmwareOffer(f.t.Context(), db.RecordFirmwareOfferParams{DeviceID: dev.ID, FirmwareID: fw.ID, LastOfferedAt: testNow.Unix()})
		if err != nil {
			f.t.Fatal(err)
		}
	}
}

// rawBuild adds a build row that Add would refuse, a good one changed by
// edit, and rolls it out to the canary without StartRollout's checks.
func (f *fixture) rawBuild(edit func(*db.CreateFirmwareParams)) db.Firmware {
	f.t.Helper()
	p := db.CreateFirmwareParams{
		FwModel: testModel, Version: "1.8.19", Sha256: sha256Hex("raw"), Chip: ChipESP32S3.Name, Bytes: 1000, Source: SourceUpload, UploadedAt: 1,
	}
	edit(&p)
	fw, err := f.client.Write.CreateFirmware(f.t.Context(), p)
	if err != nil {
		f.t.Fatal(err)
	}
	dev, err := device.FindDevice(f.t.Context(), f.client.Read, canaryID)
	if err != nil {
		f.t.Fatal(err)
	}
	err = f.client.Write.StartFirmwareRollout(f.t.Context(), db.StartFirmwareRolloutParams{FirmwareID: fw.ID, CanaryDeviceID: dev.ID, StartedAt: 1})
	if err != nil {
		f.t.Fatal(err)
	}
	return fw
}

// TestDeviceOffers checks every reason code through DeviceOffers, and that
// Offer, which /api/display uses, offers the build exactly when the reason is
// empty.
func TestDeviceOffers(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		// setup makes the build, with the canary and the other device on
		// 1.8.17, and returns the build and the device to look at.
		setup  func(f *fixture) (db.Firmware, string)
		want   Reason
		canary bool
	}{
		{"offered canary", func(f *fixture) (db.Firmware, string) {
			fw := f.add("1.8.19")
			f.start(fw)
			return fw, canaryID
		}, "", true},
		{"offered after canary reported", func(f *fixture) (db.Firmware, string) {
			fw := f.add("1.8.19")
			f.start(fw)
			f.setVersion(canaryID, "1.8.19")
			return fw, otherID
		}, "", false},
		{"updates off", func(f *fixture) (db.Firmware, string) {
			fw := f.add("1.8.19")
			f.start(fw)
			f.setVersion(canaryID, "1.8.19")
			if _, err := SetPolicy(f.t.Context(), f.client, otherID, Policy{UpdatesEnabled: ptr(false)}); err != nil {
				f.t.Fatal(err)
			}
			return fw, otherID
		}, ReasonUpdatesOff, false},
		{"pinned", func(f *fixture) (db.Firmware, string) {
			fw := f.add("1.8.19")
			f.start(fw)
			f.setVersion(canaryID, "1.8.19")
			if _, err := SetPolicy(f.t.Context(), f.client, otherID, Policy{Pin: ptr("1.8.17")}); err != nil {
				f.t.Fatal(err)
			}
			return fw, otherID
		}, ReasonPinned, false},
		{"chip is not the model's", func(f *fixture) (db.Firmware, string) {
			return f.rawBuild(func(p *db.CreateFirmwareParams) { p.Chip = ChipESP32C3.Name }), canaryID
		}, ReasonWrongModel, false},
		{"build over the model's partition", func(f *fixture) (db.Firmware, string) {
			return f.rawBuild(func(p *db.CreateFirmwareParams) { p.Bytes = Models[testModel].MaxBytes + 1 }), canaryID
		}, ReasonWrongModel, false},
		{"build version is not dotted numbers", func(f *fixture) (db.Firmware, string) {
			return f.rawBuild(func(p *db.CreateFirmwareParams) { p.Version = "dev" }), canaryID
		}, ReasonNotNewer, false},
		{"version is not dotted numbers", func(f *fixture) (db.Firmware, string) {
			fw := f.add("1.8.19")
			f.start(fw)
			f.setVersion(canaryID, "1.8.19")
			f.setVersion(otherID, "dev")
			return fw, otherID
		}, ReasonBadVersion, false},
		{"firmware too old to trust", func(f *fixture) (db.Firmware, string) {
			fw := f.add("1.8.19")
			f.start(fw)
			f.setVersion(canaryID, "1.8.19")
			f.setVersion(otherID, "1.8.10")
			return fw, otherID
		}, ReasonOldFirmware, false},
		{"already on the version", func(f *fixture) (db.Firmware, string) {
			fw := f.add("1.8.19")
			f.start(fw)
			f.setVersion(canaryID, "1.8.19")
			return fw, canaryID
		}, ReasonNotNewer, false},
		{"bad SHA", func(f *fixture) (db.Firmware, string) {
			return f.rawBuild(func(p *db.CreateFirmwareParams) { p.Sha256 = "../x" }), canaryID
		}, ReasonBadBuild, true},
		{"no rollout", func(f *fixture) (db.Firmware, string) {
			return f.add("1.8.19"), canaryID
		}, ReasonNoRollout, true},
		{"canceled", func(f *fixture) (db.Firmware, string) {
			fw := f.add("1.8.19")
			f.start(fw)
			if err := CancelRollout(f.t.Context(), f.client, fw.ID); err != nil {
				f.t.Fatal(err)
			}
			return fw, canaryID
		}, ReasonCanceled, true},
		{"canary stalled, for the canary", func(f *fixture) (db.Firmware, string) {
			fw := f.add("1.8.19")
			f.start(fw)
			f.offered(canaryID, fw, MaxOffers)
			return fw, canaryID
		}, ReasonStalled, true},
		{"canary stalled, for another", func(f *fixture) (db.Firmware, string) {
			fw := f.add("1.8.19")
			f.start(fw)
			f.offered(canaryID, fw, MaxOffers)
			return fw, otherID
		}, ReasonStalled, true},
		{"waiting for the canary", func(f *fixture) (db.Firmware, string) {
			fw := f.add("1.8.19")
			f.start(fw)
			return fw, otherID
		}, ReasonWaitingForCanary, true},
		{"offer cap reached", func(f *fixture) (db.Firmware, string) {
			fw := f.add("1.8.19")
			f.start(fw)
			f.setVersion(canaryID, "1.8.19")
			f.offered(otherID, fw, MaxOffers)
			return fw, otherID
		}, ReasonCapReached, true},
		{"file missing", func(f *fixture) (db.Firmware, string) {
			fw := f.add("1.8.19")
			f.start(fw)
			if err := os.Remove(Path(f.dir, fw.Sha256)); err != nil {
				f.t.Fatal(err)
			}
			return fw, canaryID
		}, ReasonFileMissing, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newFixture(t)
			f.device(canaryID, "AA:00:00:00:00:01", testModel, "1.8.17")
			f.device(otherID, "AA:00:00:00:00:02", testModel, "1.8.17")
			fw, target := tc.setup(f)

			rows, err := DeviceOffers(t.Context(), f.client.Read, f.dir, fw.ID)
			if err != nil {
				t.Fatal(err)
			}
			var got *DeviceOffer
			for i := range rows {
				if rows[i].FriendlyID == target {
					got = &rows[i]
				}
			}
			if got == nil {
				t.Fatalf("%s is not in %+v", target, rows)
			}
			if got.Reason != tc.want {
				t.Errorf("reason = %q (%s), want %q", got.Reason, got.Why, tc.want)
			}
			if (got.Why == "") != (got.Reason == "") {
				t.Errorf("reason %q has text %q", got.Reason, got.Why)
			}
			// A device that passes the device half could be the canary.
			if got.CanBeCanary != (tc.canary || tc.want == "") {
				t.Errorf("can be canary = %v, want %v", got.CanBeCanary, tc.canary || tc.want == "")
			}

			dev, err := device.FindDevice(t.Context(), f.client.Read, target)
			if err != nil {
				t.Fatal(err)
			}
			offer, err := Offer(t.Context(), f.client.Read, f.dir, &dev, dev.FwModel, dev.FwVersion)
			if err != nil {
				t.Fatal(err)
			}
			if (offer != nil) != (got.Reason == "") || (offer != nil && offer.ID != fw.ID) {
				t.Errorf("Offer = %v, but the reason is %q", offer, got.Reason)
			}
		})
	}
}

func TestDeviceOffersUnknownBuild(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	if _, err := DeviceOffers(t.Context(), f.client.Read, f.dir, 99); !errors.Is(err, ErrNoFirmware) {
		t.Errorf("err = %v, want ErrNoFirmware", err)
	}
}

// TestDeviceOffersMany checks the listing of many devices: the build's model
// only, in friendly ID order, with each device's own offer count. The
// listing's queries do not depend on the number of devices by construction.
func TestDeviceOffersMany(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.device(canaryID, "AA:00:00:00:01:00", testModel, "1.8.17")
	const n = 50
	for i := range n {
		f.device("D"+strconv.Itoa(100+i), "AA:00:00:00:02:"+strconv.FormatInt(int64(0x10+i), 16), testModel, "1.8.17")
	}
	f.device("OTHER1", "AA:00:00:00:03:00", "og", "1.8.17")
	fw := f.add("1.8.19")
	f.start(fw)
	f.setVersion(canaryID, "1.8.19")
	f.offered("D107", fw, 2)

	rows, err := DeviceOffers(t.Context(), f.client.Read, f.dir, fw.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != n+1 {
		t.Fatalf("%d rows, want %d", len(rows), n+1)
	}
	if rows[0].FriendlyID != canaryID || !rows[0].Canary || rows[0].Reason != ReasonNotNewer {
		t.Errorf("first row %+v", rows[0])
	}
	for i, r := range rows[1:] {
		want := "D" + strconv.Itoa(100+i)
		if r.FriendlyID != want || r.Reason != "" || r.Canary {
			t.Fatalf("row %d = %+v, want an offer to %s", i+1, r, want)
		}
		if wantOffers := map[bool]int64{true: 2, false: 0}[want == "D107"]; r.Offers != wantOffers {
			t.Errorf("%s offers = %d, want %d", want, r.Offers, wantOffers)
		}
	}
	if rows[8].LastOfferedAt != testNow.Unix() {
		t.Errorf("last offered at %d, want %d", rows[8].LastOfferedAt, testNow.Unix())
	}
}

// TestOfferableCaps checks that with two builds in active rollouts, offers
// are counted per build, and the newest build under the cap is offered.
func TestOfferableCaps(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.device(canaryID, "AA:00:00:00:00:01", testModel, "1.8.17")
	older, newer := f.add("1.8.18"), f.add("1.8.19")
	f.start(older)
	f.start(newer)
	dev, err := device.FindDevice(t.Context(), f.client.Read, canaryID)
	if err != nil {
		t.Fatal(err)
	}
	for _, step := range []struct {
		fw   db.Firmware
		n    int
		want int64
	}{
		{newer, 0, newer.ID},
		{newer, MaxOffers - 1, newer.ID},
		{newer, 1, older.ID},
		{older, MaxOffers - 1, older.ID},
		{older, 1, 0},
	} {
		f.offered(canaryID, step.fw, step.n)
		got, err := Offer(t.Context(), f.client.Read, f.dir, &dev, testModel, "1.8.17")
		if err != nil {
			t.Fatal(err)
		}
		if (got == nil && step.want != 0) || (got != nil && got.ID != step.want) {
			t.Fatalf("after %d more offers of %s, offered %v, want build %d", step.n, step.fw.Version, got, step.want)
		}
	}
}

// TestDeviceOffersBuilds checks the listing of each of several builds of a
// model against Offer and OfferAndRecord, which send the device the newest
// build that qualifies and has its file, whichever build is listed.
func TestDeviceOffersBuilds(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		// setup adds 1.8.18 and 1.8.19, and returns them with the device to
		// look at.
		setup func(f *fixture) (older, newer db.Firmware, target string)
		// want is the reason for each build, and insteadNewer is whether the
		// older build's listing names the newer one.
		wantOlder, wantNewer Reason
		insteadNewer         bool
		// sent is the build that is offered: "older", "newer" or "".
		sent string
	}{
		{"both active", func(f *fixture) (db.Firmware, db.Firmware, string) {
			older, newer := f.add("1.8.18"), f.add("1.8.19")
			f.start(older)
			f.start(newer)
			return older, newer, canaryID
		}, ReasonNewerBuild, "", true, "newer"},
		{"newer file missing", func(f *fixture) (db.Firmware, db.Firmware, string) {
			older, newer := f.add("1.8.18"), f.add("1.8.19")
			f.start(older)
			f.start(newer)
			if err := os.Remove(Path(f.dir, newer.Sha256)); err != nil {
				f.t.Fatal(err)
			}
			return older, newer, canaryID
		}, "", ReasonFileMissing, false, "older"},
		{"offer cap reached on the newer", func(f *fixture) (db.Firmware, db.Firmware, string) {
			older, newer := f.add("1.8.18"), f.add("1.8.19")
			f.start(older)
			f.start(newer)
			f.setVersion(canaryID, "1.8.19")
			f.offered(otherID, newer, MaxOffers)
			return older, newer, otherID
		}, "", ReasonCapReached, false, "older"},
		{"newer canceled", func(f *fixture) (db.Firmware, db.Firmware, string) {
			older, newer := f.add("1.8.18"), f.add("1.8.19")
			f.start(older)
			f.start(newer)
			if err := CancelRollout(f.t.Context(), f.client, newer.ID); err != nil {
				f.t.Fatal(err)
			}
			return older, newer, canaryID
		}, "", ReasonCanceled, false, "older"},
		{"newer waits for its canary", func(f *fixture) (db.Firmware, db.Firmware, string) {
			older, newer := f.add("1.8.18"), f.add("1.8.19")
			if _, err := StartRollout(f.t.Context(), f.client, older.ID, otherID, testNow); err != nil {
				f.t.Fatal(err)
			}
			f.start(newer)
			return older, newer, otherID
		}, "", ReasonWaitingForCanary, false, "older"},
		{"newer never rolled out", func(f *fixture) (db.Firmware, db.Firmware, string) {
			older, newer := f.add("1.8.18"), f.add("1.8.19")
			f.start(older)
			return older, newer, canaryID
		}, "", ReasonNoRollout, false, "older"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newFixture(t)
			f.device(canaryID, "AA:00:00:00:00:01", testModel, "1.8.17")
			f.device(otherID, "AA:00:00:00:00:02", testModel, "1.8.17")
			older, newer, target := tc.setup(f)
			dev, err := device.FindDevice(t.Context(), f.client.Read, target)
			if err != nil {
				t.Fatal(err)
			}
			offer, err := Offer(t.Context(), f.client.Read, f.dir, &dev, dev.FwModel, dev.FwVersion)
			if err != nil {
				t.Fatal(err)
			}
			sent := map[string]*db.Firmware{"older": &older, "newer": &newer, "": nil}[tc.sent]
			if (offer == nil) != (sent == nil) || (offer != nil && offer.ID != sent.ID) {
				t.Fatalf("Offer = %v, want the %q build", offer, tc.sent)
			}

			for _, b := range []struct {
				fw      db.Firmware
				want    Reason
				instead int64
			}{{older, tc.wantOlder, map[bool]int64{true: newer.ID}[tc.insteadNewer]}, {newer, tc.wantNewer, 0}} {
				rows, err := DeviceOffers(t.Context(), f.client.Read, f.dir, b.fw.ID)
				if err != nil {
					t.Fatal(err)
				}
				var got *DeviceOffer
				for i := range rows {
					if rows[i].FriendlyID == target {
						got = &rows[i]
					}
				}
				if got == nil {
					t.Fatalf("%s is not listed for %s", target, b.fw.Version)
				}
				if got.Reason != b.want || got.Instead != b.instead {
					t.Errorf("%s: reason %q instead %d (%s), want %q and %d", b.fw.Version, got.Reason, got.Instead, got.Why, b.want, b.instead)
				}
				if offered := offer != nil && offer.ID == b.fw.ID; offered != (got.Reason == "") {
					t.Errorf("%s: Offer sends it = %v, but the reason is %q", b.fw.Version, offered, got.Reason)
				}
			}

			rec, err := OfferAndRecord(t.Context(), f.client, f.dir, &dev, dev.FwModel, dev.FwVersion, testNow)
			if err != nil {
				t.Fatal(err)
			}
			if (rec == nil) != (offer == nil) || (rec != nil && rec.ID != offer.ID) {
				t.Errorf("OfferAndRecord = %v, Offer = %v", rec, offer)
			}
		})
	}
}
