// Package firmware keeps the catalog of firmware builds and decides who is
// offered one. The server is the only thing that stops a wrong-board
// update, since the device firmware does not check the board it installs
// onto, so every rule here refuses when in doubt.
package firmware

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/die-net/trmnl-byos-go/internal/db"
	"github.com/die-net/trmnl-byos-go/internal/device"
	"github.com/die-net/trmnl-byos-go/internal/errkind"
)

// Errors from the catalog, each of an errkind kind and wrapped with detail.
var (
	// ErrImage means a file is not an acceptable firmware image.
	ErrImage = errkind.New(errkind.ErrInvalid, "bad firmware image")
	// ErrTooBig means a file is larger than its model's app partition.
	ErrTooBig = errkind.New(errkind.ErrTooBig, "firmware file too large")
	// ErrNoFirmware means the catalog has no such build.
	ErrNoFirmware = errkind.New(errkind.ErrNotFound, "no such firmware")
	// ErrExists means the catalog has the file, or a build of that model at
	// that version.
	ErrExists = errkind.New(errkind.ErrConflict, "firmware exists")
)

// SourceUpload is the source of a build added from a file.
const SourceUpload = "upload"

// MaxOffers is how many times a build is offered to one device: one try and
// two retries, for a download that drops. A build that is still not
// installed after that either fails to install or is not the version it
// was added as, and offering it on every wake would re-flash the device
// forever. After this many offers the device is offered that build no more,
// and if it is the canary the rollout is stalled.
const MaxOffers = 3

// stateActive is the state of a rollout that is being offered. A rollout
// that was canceled has state "canceled".
const stateActive = "active"

// Path returns where the build with the given SHA-256 is stored under dir.
func Path(dir, sha string) string {
	return filepath.Join(dir, FileName(sha))
}

// ValidSHA reports whether s is 64 lowercase hex digits, which is all a
// stored file's name can be.
func ValidSHA(s string) bool {
	if len(s) != 2*sha256.Size {
		return false
	}
	for i := range len(s) {
		if c := s[i]; (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// Add adds the firmware file read from r to the catalog, stored in dir, as
// a build for the firmware model. version may be empty when the file's app
// descriptor holds a plain X.Y.Z version. r is read once, streamed to a
// temporary file in dir while it is hashed, and checked there. The file is
// moved into place only when every check passes, and a refused upload
// leaves nothing behind.
//
// The checks are that the model is in Models, that the file is no larger
// than the model's app partition, that its image headers are well formed,
// that its chip is the model's chip, and that its version is dotted
// numbers and agrees with the app descriptor. The version recorded is the
// one given, or the descriptor's when none is.
//
// The app descriptor's version is the firmware project's version, which
// the TRMNL build does not set, so it is usually a git description or "1".
// The firmware reports a number compiled in from config.h instead. So the
// descriptor can confirm the version only when it is plain X.Y.Z. Otherwise
// the caller must say which version the file is, from its release name.
func Add(ctx context.Context, client *db.Client, dir, model, version string, r io.Reader, now time.Time) (db.Firmware, error) {
	m, ok := Models[model]
	if !ok {
		return db.Firmware{}, fmt.Errorf("%w: model %q is not one this server knows the chip of", errkind.ErrInvalid, model)
	}
	var declared Version
	if version != "" {
		var err error
		if declared, err = ParseVersion(version); err != nil {
			return db.Firmware{}, err
		}
	}
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return db.Firmware{}, fmt.Errorf("create firmware dir: %w", err)
	}
	// A hidden name, which is never a stored file's name.
	f, err := os.CreateTemp(dir, ".upload-*.bin")
	if err != nil {
		return db.Firmware{}, fmt.Errorf("create upload file: %w", err)
	}
	// Once renamed into place, the name is gone and this does nothing.
	defer func() { _ = os.Remove(f.Name()) }()
	defer f.Close()

	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, h), io.LimitReader(r, m.MaxBytes+1))
	if err != nil {
		return db.Firmware{}, fmt.Errorf("save upload: %w", err)
	}
	if n > m.MaxBytes {
		return db.Firmware{}, fmt.Errorf("%w: %s's app partition is %d bytes", ErrTooBig, model, m.MaxBytes)
	}
	img, err := ParseImage(f, n)
	if err != nil {
		return db.Firmware{}, err
	}
	if img.ChipID != m.Chip.ID {
		return db.Firmware{}, fmt.Errorf("%w: the image is for %s, and %s is %s", ErrImage, chipName(img.ChipID), model, m.Chip.Name)
	}
	if v, ok := cleanDescriptorVersion(img.Version); ok {
		if declared == nil {
			declared = v
		} else if declared.Compare(v) != 0 {
			return db.Firmware{}, fmt.Errorf("%w: the image says version %s, not %s", errkind.ErrInvalid, v, declared)
		}
	}
	if declared == nil {
		return db.Firmware{}, fmt.Errorf("%w: the image's version is %q, which says nothing about the firmware's own, so give the version", errkind.ErrInvalid, img.Version)
	}
	if err := f.Sync(); err != nil {
		return db.Firmware{}, fmt.Errorf("save upload: %w", err)
	}
	if err := f.Close(); err != nil {
		return db.Firmware{}, fmt.Errorf("save upload: %w", err)
	}

	sha := hex.EncodeToString(h.Sum(nil))
	var fw db.Firmware
	err = client.InTx(ctx, func(q *db.Queries) error {
		exists, err := q.FirmwareExists(ctx, db.FirmwareExistsParams{Sha256: sha, FwModel: model, Version: declared.String()})
		if err != nil {
			return fmt.Errorf("check catalog: %w", err)
		}
		if exists != 0 {
			return fmt.Errorf("%w: %s %s or this file is in the catalog already", ErrExists, model, declared)
		}
		fw, err = q.CreateFirmware(ctx, db.CreateFirmwareParams{
			FwModel: model, Version: declared.String(), Sha256: sha, Chip: m.Chip.Name,
			IdfVersion: img.IDFVersion, Bytes: n, Source: SourceUpload, UploadedAt: now.Unix(),
		})
		if err != nil {
			return fmt.Errorf("save firmware: %w", err)
		}
		if err := os.Rename(f.Name(), Path(dir, sha)); err != nil {
			return fmt.Errorf("store firmware: %w", err)
		}
		return nil
	})
	return fw, err
}

// Entry is a build and the state of its rollout.
type Entry struct {
	db.Firmware
	// Stage is "", for no rollout, "canary", "open", "stalled" or
	// "canceled". A stalled rollout's canary has been offered the build
	// MaxOffers times and still does not report its version, so nothing is
	// offered to anyone until the rollout is restarted or canceled.
	Stage string
	// CanaryVersion is the firmware version the canary last reported.
	CanaryVersion string
	// Canary is the canary device's friendly ID, or "" with no rollout.
	Canary string
	// StartedAt is when the rollout started, in Unix seconds, or 0.
	StartedAt int64
	// CanaryLastOfferedAt is when the canary was last told to install the
	// build, in Unix seconds, or 0 if it never was. It shows whether the
	// canary was offered the build and is not taking it.
	CanaryLastOfferedAt int64
}

// List returns every build in the catalog, oldest model first.
func List(ctx context.Context, q *db.Queries) ([]Entry, error) {
	rows, err := q.ListFirmware(ctx)
	if err != nil {
		return nil, fmt.Errorf("list firmware: %w", err)
	}
	out := make([]Entry, len(rows))
	for i := range rows {
		r := &rows[i]
		e := Entry{Firmware: r.Firmware}
		if r.RolloutState != nil {
			e.Stage = *r.RolloutState
			e.Canary = deref(r.Canary)
			e.StartedAt = derefInt(r.RolloutStartedAt)
			e.CanaryVersion = deref(r.CanaryVersion)
			e.CanaryLastOfferedAt = r.CanaryLastOfferedAt
			if e.Stage == stateActive {
				switch {
				case canaryReported(&r.Firmware, deref(r.CanaryModel), e.CanaryVersion):
					e.Stage = "open"
				case r.CanaryOffers >= MaxOffers:
					e.Stage = "stalled"
				default:
					e.Stage = "canary"
				}
			}
		}
		out[i] = e
	}
	return out, nil
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func derefInt(n *int64) int64 {
	if n == nil {
		return 0
	}
	return *n
}

// canaryReported reports whether the canary device is of the build's model
// and reports the build's version or a newer one, which opens the rollout to
// the rest. A canary flashed past the build has taken it as far as it can,
// and requiring an exact match would leave the rollout closed for good.
func canaryReported(fw *db.Firmware, canaryModel, canaryVersion string) bool {
	if canaryModel != fw.FwModel {
		return false
	}
	want, err := ParseVersion(fw.Version)
	if err != nil {
		return false
	}
	got, err := ParseVersion(canaryVersion)
	return err == nil && got.Compare(want) >= 0
}

// notEligible returns why a device of the given model and version, with
// the given policy, is not offered the build, or "" when it is. It is the
// device half of an offer, and what Rollout checks of a canary so that a
// rollout is never started that its canary can never take.
func notEligible(dev *db.Device, model, version string, fw *db.Firmware) string {
	if dev.UpdatesEnabled == 0 {
		return "updates are off for the device"
	}
	if dev.FwPin != "" {
		return "the device is pinned to " + dev.FwPin
	}
	m, ok := Models[model]
	if !ok || model != fw.FwModel || m.Chip.Name != fw.Chip || fw.Bytes > m.MaxBytes {
		return "the build is not for the device's model, " + model
	}
	have, err := ParseVersion(version)
	if err != nil {
		return "the device's firmware version " + version + " is not dotted numbers"
	}
	if have.Compare(minReportedVersion) < 0 {
		return "the device's firmware " + version + " is older than " + minReportedVersion.String() + ", whose Model header may name another board"
	}
	want, err := ParseVersion(fw.Version)
	if err != nil || want.Compare(have) <= 0 {
		return "the build's version " + fw.Version + " is not newer than the device's " + version
	}
	return ""
}

// StartRollout offers the build with the given ID to the device named by
// canary, and to every device of its model after that device reports the
// build's version. A rollout already under way, stalled or canceled restarts
// with the canary, and the canary's count of offers of the build starts over,
// which is how the admin retries. It refuses a canary that could not be offered the
// build now: of another model, with updates off or pinned, or already on
// that version or newer.
func StartRollout(ctx context.Context, client *db.Client, id int64, canary string, now time.Time) (Entry, error) {
	err := client.InTx(ctx, func(q *db.Queries) error {
		fw, err := getFirmware(ctx, q, id)
		if err != nil {
			return err
		}
		dev, err := device.FindDevice(ctx, q, canary)
		if err != nil {
			return err
		}
		if why := notEligible(&dev, dev.FwModel, dev.FwVersion, &fw); why != "" {
			return fmt.Errorf("%w: %s cannot be the canary: %s", errkind.ErrInvalid, dev.FriendlyID, why)
		}
		if err := q.ResetFirmwareOffers(ctx, db.ResetFirmwareOffersParams{DeviceID: dev.ID, FirmwareID: id}); err != nil {
			return fmt.Errorf("reset offers: %w", err)
		}
		if err := q.StartFirmwareRollout(ctx, db.StartFirmwareRolloutParams{FirmwareID: id, CanaryDeviceID: dev.ID, StartedAt: now.Unix()}); err != nil {
			return fmt.Errorf("start rollout: %w", err)
		}
		return nil
	})
	if err != nil {
		return Entry{}, err
	}
	return Get(ctx, client.Read, id)
}

// CancelRollout stops offering the build with the given ID. A device that
// is partway through a download may fail it, and keeps running what it has.
func CancelRollout(ctx context.Context, client *db.Client, id int64) error {
	return client.InTx(ctx, func(q *db.Queries) error {
		if _, err := getFirmware(ctx, q, id); err != nil {
			return err
		}
		n, err := q.CancelFirmwareRollout(ctx, id)
		if err != nil {
			return fmt.Errorf("cancel rollout: %w", err)
		}
		if n == 0 {
			return fmt.Errorf("%w: firmware %d has no active rollout", errkind.ErrInvalid, id)
		}
		return nil
	})
}

func getFirmware(ctx context.Context, q *db.Queries, id int64) (db.Firmware, error) {
	fw, err := q.GetFirmware(ctx, id)
	if errors.Is(err, sql.ErrNoRows) {
		return db.Firmware{}, fmt.Errorf("%w: %d", ErrNoFirmware, id)
	}
	if err != nil {
		return db.Firmware{}, fmt.Errorf("get firmware: %w", err)
	}
	return fw, nil
}

// Get returns the build with the given ID and its rollout.
func Get(ctx context.Context, q *db.Queries, id int64) (Entry, error) {
	all, err := List(ctx, q)
	if err != nil {
		return Entry{}, err
	}
	for i := range all {
		if all[i].ID == id {
			return all[i], nil
		}
	}
	return Entry{}, fmt.Errorf("%w: %d", ErrNoFirmware, id)
}

// Policy is a change to a device's firmware policy. A nil field is left as
// it is.
type Policy struct {
	// UpdatesEnabled turns on or off offering the device firmware.
	UpdatesEnabled *bool
	// Pin is a version the device stays on, or "" to clear the pin.
	Pin *string
}

// SetPolicy changes the policy of the device named by its friendly ID or
// MAC, and returns the device.
func SetPolicy(ctx context.Context, client *db.Client, name string, p Policy) (db.Device, error) {
	var pin string
	if p.Pin != nil && *p.Pin != "" {
		v, err := ParseVersion(*p.Pin)
		if err != nil {
			return db.Device{}, err
		}
		pin = v.String()
	}
	var dev db.Device
	err := client.InTx(ctx, func(q *db.Queries) error {
		var err error
		if dev, err = device.FindDevice(ctx, q, name); err != nil {
			return err
		}
		if p.UpdatesEnabled != nil {
			dev.UpdatesEnabled = 0
			if *p.UpdatesEnabled {
				dev.UpdatesEnabled = 1
			}
		}
		if p.Pin != nil {
			dev.FwPin = pin
		}
		err = q.SetDeviceFirmwarePolicy(ctx, db.SetDeviceFirmwarePolicyParams{UpdatesEnabled: dev.UpdatesEnabled, FwPin: dev.FwPin, ID: dev.ID})
		if err != nil {
			return fmt.Errorf("set firmware policy: %w", err)
		}
		return nil
	})
	return dev, err
}

// Offer returns the build the device should be told to install, or nil.
// model and version are what the device reported on this request. dev
// supplies the device's policy. The build must be for exactly model, in an
// active rollout, newer than version, with its file in dir, and the device
// must be eligible (see notEligible). It must also be open to the device:
// the device is the rollout's canary, or the canary has reported the
// build's version. Of several builds that qualify the newest is offered.
// Offer counts nothing.
func Offer(ctx context.Context, q *db.Queries, dir string, dev *db.Device, model, version string) (*db.Firmware, error) {
	cands, err := offerable(ctx, q, dev, model, version)
	if err != nil {
		return nil, err
	}
	for _, fw := range cands {
		if onDisk(dir, fw) {
			return fw, nil
		}
	}
	return nil, nil
}

// offerable returns the builds that could be offered to the device by what
// the database says, newest first, without looking at any file. Offer and
// OfferAndRecord share it, so that a decision made inside a write
// transaction makes no filesystem calls.
func offerable(ctx context.Context, q *db.Queries, dev *db.Device, model, version string) ([]*db.Firmware, error) {
	if _, ok := Models[model]; !ok || dev.UpdatesEnabled == 0 || dev.FwPin != "" {
		return nil, nil
	}
	rows, err := q.ListActiveFirmware(ctx, model)
	if err != nil {
		return nil, fmt.Errorf("list active firmware: %w", err)
	}
	type candidate struct {
		fw *db.Firmware
		v  Version
	}
	var cands []candidate
	for i := range rows {
		r := &rows[i]
		fw := &r.Firmware
		if notEligible(dev, model, version, fw) != "" {
			continue
		}
		offers, err := q.GetFirmwareOffers(ctx, db.GetFirmwareOffersParams{DeviceID: dev.ID, FirmwareID: fw.ID})
		if err != nil {
			return nil, fmt.Errorf("count offers: %w", err)
		}
		if offers >= MaxOffers {
			continue
		}
		if r.CanaryDeviceID != dev.ID && !canaryReported(fw, r.CanaryModel, r.CanaryVersion) {
			continue
		}
		v, err := ParseVersion(fw.Version)
		if err != nil || !ValidSHA(fw.Sha256) {
			continue
		}
		cands = append(cands, candidate{fw, v})
	}
	slices.SortStableFunc(cands, func(a, b candidate) int { return b.v.Compare(a.v) })
	out := make([]*db.Firmware, len(cands))
	for i, c := range cands {
		out[i] = c.fw
	}
	return out, nil
}

// onDisk reports whether the build's file is in dir, at the recorded size.
func onDisk(dir string, fw *db.Firmware) bool {
	fi, err := os.Stat(Path(dir, fw.Sha256))
	return err == nil && fi.Mode().IsRegular() && fi.Size() == fw.Bytes
}

// OfferAndRecord is Offer, and counts the offer it makes against the device
// and the build, in the same transaction as the decision. It decides on the
// read pool first, so a wake with nothing to offer, which is nearly all of
// them, takes no write, and checks the files there. Only an offer is decided
// again, from the database alone, inside the write transaction, since the
// state it read may have changed. A build that became a candidate in between
// is not offered until the next wake, as its file has not been checked.
func OfferAndRecord(ctx context.Context, client *db.Client, dir string, dev *db.Device, model, version string, now time.Time) (*db.Firmware, error) {
	cands, err := offerable(ctx, client.Read, dev, model, version)
	if err != nil {
		return nil, err
	}
	good := map[int64]bool{}
	for _, fw := range cands {
		if onDisk(dir, fw) {
			good[fw.ID] = true
		}
	}
	if len(good) == 0 {
		return nil, nil
	}
	var offered *db.Firmware
	err = client.InTx(ctx, func(q *db.Queries) error {
		cands, err := offerable(ctx, q, dev, model, version)
		if err != nil {
			return err
		}
		i := slices.IndexFunc(cands, func(fw *db.Firmware) bool { return good[fw.ID] })
		if i < 0 {
			return nil
		}
		fw := cands[i]
		if err := q.RecordFirmwareOffer(ctx, db.RecordFirmwareOfferParams{DeviceID: dev.ID, FirmwareID: fw.ID, LastOfferedAt: now.Unix()}); err != nil {
			return fmt.Errorf("record offer: %w", err)
		}
		offered = fw
		return nil
	})
	if err != nil {
		return nil, err
	}
	return offered, nil
}

// Open returns the stored file of the build whose SHA-256 is sha, when it
// is in an active rollout, with the build. The caller closes the file. A
// malformed hash, an unknown build, a build without an active rollout and
// a file that is missing or the wrong size all wrap ErrNoFirmware.
func Open(ctx context.Context, q *db.Queries, dir, sha string) (*os.File, db.Firmware, error) {
	// Check before the hash reaches the filesystem.
	if !ValidSHA(sha) {
		return nil, db.Firmware{}, fmt.Errorf("%w: not a SHA-256", ErrNoFirmware)
	}
	fw, err := q.GetActiveFirmwareBySHA(ctx, sha)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, db.Firmware{}, fmt.Errorf("%w: %s", ErrNoFirmware, sha)
	}
	if err != nil {
		return nil, db.Firmware{}, fmt.Errorf("get firmware: %w", err)
	}
	f, err := os.Open(Path(dir, sha))
	if err != nil {
		return nil, db.Firmware{}, fmt.Errorf("%w: open file: %w", ErrNoFirmware, err)
	}
	fi, err := f.Stat()
	if err != nil || !fi.Mode().IsRegular() || fi.Size() != fw.Bytes {
		_ = f.Close()
		return nil, db.Firmware{}, fmt.Errorf("%w: %s is not the file that was added", ErrNoFirmware, sha)
	}
	return f, fw, nil
}

// FileName returns the name a download URL ends in for the build with the
// given SHA-256.
func FileName(sha string) string { return sha + ".bin" }

// SHAFromName returns the SHA-256 in a download URL's file name, or false
// when the name is not a hash and ".bin".
func SHAFromName(name string) (string, bool) {
	sha, ok := strings.CutSuffix(name, ".bin")
	return sha, ok && ValidSHA(sha)
}
