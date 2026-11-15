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
	"strconv"
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
		return db.Firmware{}, errkind.New(errkind.ErrInvalid, fmt.Sprintf("this server does not know the firmware model %q", model))
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
		return db.Firmware{}, errkind.New(ErrTooBig, fmt.Sprintf("the file is larger than the app partition of %s, which holds %s", model, ByteSize(m.MaxBytes)))
	}
	img, err := ParseImage(f, n)
	if err != nil {
		return db.Firmware{}, err
	}
	if img.ChipID != m.Chip.ID {
		return db.Firmware{}, imageError("the image is built for %s, and %s uses %s", chipName(img.ChipID), model, m.Chip.Name)
	}
	if v, ok := cleanDescriptorVersion(img.Version); ok {
		if declared == nil {
			declared = v
		} else if declared.Compare(v) != 0 {
			return db.Firmware{}, errkind.New(errkind.ErrInvalid, fmt.Sprintf("the image says it is version %s, and the version given is %s", v, declared))
		}
	}
	if declared == nil {
		return db.Firmware{}, errkind.New(errkind.ErrInvalid, fmt.Sprintf("the image does not say which firmware version it is (its app descriptor says %q), so the version must be given", img.Version))
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
		rows, err := q.ListFirmwareClashes(ctx, db.ListFirmwareClashesParams{Sha256: sha, FwModel: model})
		if err != nil {
			return fmt.Errorf("check catalog: %w", err)
		}
		for _, r := range rows {
			if r.Sha256 == sha {
				return errkind.New(ErrExists, fmt.Sprintf("this exact file is already in the catalog, as %s %s", r.FwModel, r.Version))
			}
			// A version that does not parse cannot equal this one.
			if v, err := ParseVersion(r.Version); err == nil && v.Compare(declared) == 0 {
				if r.Version == declared.String() {
					return errkind.New(ErrExists, fmt.Sprintf("the catalog already has %s %s, from another file", model, declared))
				}
				return errkind.New(ErrExists, fmt.Sprintf("the catalog already has %s %s, which is the same version as %s", model, r.Version, declared))
			}
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
		out[i] = newEntry(&rows[i])
	}
	return out, nil
}

// newEntry returns the build of a catalog row with the stage of its rollout.
func newEntry(r *db.ListFirmwareRow) Entry {
	e := Entry{Firmware: r.Firmware}
	if r.RolloutState == nil {
		return e
	}
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
	return e
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

// Reason is why a device is not offered a build, as a stable code. The
// zero value means it is offered.
type Reason string

// The reasons a device is not offered a build. The first six come from the
// device and the build, and the rest from the build's rollout and file.
const (
	// ReasonUpdatesOff means updates are off for the device.
	ReasonUpdatesOff Reason = "updates_off"
	// ReasonPinned means the device is pinned to a version.
	ReasonPinned Reason = "pinned"
	// ReasonWrongModel means the build is not for the device's model: the
	// model is not in Models, differs from the build's, has another chip,
	// or the build is over its app partition.
	ReasonWrongModel Reason = "wrong_model"
	// ReasonBadVersion means the device's version is not dotted numbers.
	ReasonBadVersion Reason = "bad_version"
	// ReasonOldFirmware means the device's firmware is older than the first
	// whose Model header is trusted.
	ReasonOldFirmware Reason = "old_firmware"
	// ReasonNotNewer means the device is on the build's version or a newer
	// one, or the build's version does not parse.
	ReasonNotNewer Reason = "not_newer"
	// ReasonBadBuild means the build's SHA-256 is not 64 lowercase hex
	// digits, so it names no file.
	ReasonBadBuild Reason = "bad_build"
	// ReasonNoRollout means the build was never rolled out.
	ReasonNoRollout Reason = "no_rollout"
	// ReasonCanceled means the build's rollout was canceled.
	ReasonCanceled Reason = "canceled"
	// ReasonStalled means the canary was offered the build MaxOffers times
	// and does not report its version, so no device is offered it.
	ReasonStalled Reason = "stalled"
	// ReasonWaitingForCanary means the canary has not reported the build's
	// version yet, and the device is not the canary.
	ReasonWaitingForCanary Reason = "waiting_for_canary"
	// ReasonCapReached means the device was offered the build MaxOffers
	// times.
	ReasonCapReached Reason = "cap_reached"
	// ReasonNewerBuild means the device is offered another build, a newer
	// one that qualifies, and not this one.
	ReasonNewerBuild Reason = "newer_build"
	// ReasonFileMissing means the build's file is not in the firmware
	// directory at the recorded size.
	ReasonFileMissing Reason = "file_missing"
)

// notEligible returns why a device of the given model and version, with
// the given policy, is not offered the build, and a sentence saying so, or
// "" and "" when it is. It is the device half of an offer, and what
// StartRollout checks of a canary so that a rollout is never started that
// its canary can never take.
func notEligible(dev *db.Device, model, version string, fw *db.Firmware) (Reason, string) {
	if reason, why := notUpdatable(dev, model); reason != "" {
		return reason, why
	}
	if m := Models[model]; model != fw.FwModel || m.Chip.Name != fw.Chip || fw.Bytes > m.MaxBytes {
		return ReasonWrongModel, "the build is not for the device's model, " + model
	}
	have, err := ParseVersion(version)
	if err != nil {
		return ReasonBadVersion, "the device's firmware version " + version + " is not dotted numbers"
	}
	if have.Compare(minReportedVersion) < 0 {
		return ReasonOldFirmware, "the device's firmware " + version + " is older than " + minReportedVersion.String() + ", whose Model header may name another board"
	}
	want, err := ParseVersion(fw.Version)
	if err != nil || want.Compare(have) <= 0 {
		return ReasonNotNewer, "the build's version " + fw.Version + " is not newer than the device's " + version
	}
	return "", ""
}

// notUpdatable returns why a device of the given model is offered no build
// at all, and a sentence saying so, or "" and "" when some build could be
// offered to it. It is the part of notEligible that does not depend on the
// build, which offerable checks before it reads any.
func notUpdatable(dev *db.Device, model string) (Reason, string) {
	if dev.UpdatesEnabled == 0 {
		return ReasonUpdatesOff, "updates are off for the device"
	}
	if dev.FwPin != "" {
		return ReasonPinned, "the device is pinned to " + dev.FwPin
	}
	if _, ok := Models[model]; !ok {
		return ReasonWrongModel, "the build is not for the device's model, " + model
	}
	return "", ""
}

// rollout is what the rollout half of an offer decides from: the state of
// a build's rollout and its canary.
type rollout struct {
	// state is "" for no rollout, stateActive, or "canceled".
	state string
	// canaryID is the canary device's ID, and canaryModel and
	// canaryVersion are what it last reported.
	canaryID      int64
	canaryModel   string
	canaryVersion string
	canaryOffers  int64
}

// rolloutOf returns the rollout of a catalog row.
func rolloutOf(r *db.ListFirmwareRow) rollout {
	return rollout{
		state: deref(r.RolloutState), canaryID: derefInt(r.CanaryID), canaryModel: deref(r.CanaryModel),
		canaryVersion: deref(r.CanaryVersion), canaryOffers: r.CanaryOffers,
	}
}

// notOpen returns why the build, which a device has been offered the given
// number of times, is not offered to the device by its rollout, and a
// sentence saying so, or "" and "" when it is. It is the rollout half of an
// offer, which Offer and the per-device listing share with the device half.
func (r *rollout) notOpen(deviceID int64, fw *db.Firmware, offers int64) (Reason, string) {
	if !ValidSHA(fw.Sha256) {
		return ReasonBadBuild, "the build's SHA-256 is not 64 lowercase hex digits"
	}
	switch r.state {
	case "":
		return ReasonNoRollout, "the build was never rolled out"
	case stateActive:
	default:
		return ReasonCanceled, "the build's rollout was canceled"
	}
	if !canaryReported(fw, r.canaryModel, r.canaryVersion) {
		if r.canaryOffers >= MaxOffers {
			return ReasonStalled, "the canary was offered the build " + strconv.Itoa(MaxOffers) + " times and does not report its version"
		}
		if deviceID != r.canaryID {
			return ReasonWaitingForCanary, "the canary has not reported the build's version yet"
		}
	}
	if offers >= MaxOffers {
		return ReasonCapReached, "the device was offered the build " + strconv.Itoa(MaxOffers) + " times"
	}
	return "", ""
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
		if reason, why := notEligible(&dev, dev.FwModel, dev.FwVersion, &fw); reason != "" {
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
	row, err := q.GetFirmwareEntry(ctx, id)
	if errors.Is(err, sql.ErrNoRows) {
		return Entry{}, fmt.Errorf("%w: %d", ErrNoFirmware, id)
	}
	if err != nil {
		return Entry{}, fmt.Errorf("get firmware: %w", err)
	}
	r := db.ListFirmwareRow(row)
	return newEntry(&r), nil
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
// must be eligible (see notEligible). It must also be open to the device
// (see rollout.notOpen): the device is the rollout's canary, or the canary
// has reported the build's version, and it was offered the build fewer than
// MaxOffers times. Of several builds that qualify and have their file the
// newest is offered. Offer counts nothing.
func Offer(ctx context.Context, q *db.Queries, dir string, dev *db.Device, model, version string) (*db.Firmware, error) {
	cands, err := offerable(ctx, q, dev, model, version)
	if err != nil {
		return nil, err
	}
	return pick(cands, func(fw *db.Firmware) bool { return onDisk(dir, fw) }), nil
}

// activeBuild is a build with what decide needs to know of it for one
// device: its rollout, and how many times the device was offered it.
type activeBuild struct {
	fw     *db.Firmware
	ro     rollout
	offers int64
}

// refusal is why a build is not offered to a device.
type refusal struct {
	reason Reason
	why    string
	// byDevice is whether the device half of the decision refused it, so
	// that the device could not be the build's canary either.
	byDevice bool
}

// decide is the one rule for which builds a device would be offered, from
// what the database says and no file. A build is a candidate when the device
// half (notEligible) and the rollout half (rollout.notOpen) both pass, and
// the candidates are returned newest first. If refused is not nil it gets
// the refusal of every other build, by ID. Offer, OfferAndRecord and
// DeviceOffers all decide here, so that they cannot disagree, and since it
// checks no file it can run inside a write transaction.
func decide(dev *db.Device, model, version string, builds []activeBuild, refused map[int64]refusal) []*db.Firmware {
	type candidate struct {
		fw *db.Firmware
		v  Version
	}
	var cands []candidate
	for i := range builds {
		b := &builds[i]
		r := refusal{byDevice: true}
		if r.reason, r.why = notEligible(dev, model, version, b.fw); r.reason == "" {
			r.byDevice = false
			r.reason, r.why = b.ro.notOpen(dev.ID, b.fw, b.offers)
		}
		if r.reason != "" {
			if refused != nil {
				refused[b.fw.ID] = r
			}
			continue
		}
		// notEligible has parsed the version already.
		if v, err := ParseVersion(b.fw.Version); err == nil {
			cands = append(cands, candidate{b.fw, v})
		}
	}
	slices.SortStableFunc(cands, func(a, b candidate) int { return b.v.Compare(a.v) })
	out := make([]*db.Firmware, len(cands))
	for i, c := range cands {
		out[i] = c.fw
	}
	return out
}

// pick returns the first of the candidates, newest first, that ok accepts,
// or nil. ok is the file check, which the caller chooses.
func pick(cands []*db.Firmware, ok func(*db.Firmware) bool) *db.Firmware {
	for _, fw := range cands {
		if ok(fw) {
			return fw
		}
	}
	return nil
}

// offerable returns the builds that could be offered to the device by what
// the database says, newest first, without looking at any file. Offer and
// OfferAndRecord share it, so that a decision made inside a write
// transaction makes no filesystem calls.
func offerable(ctx context.Context, q *db.Queries, dev *db.Device, model, version string) ([]*db.Firmware, error) {
	// Most devices have updates off, so most wakes stop here without a query.
	if reason, _ := notUpdatable(dev, model); reason != "" {
		return nil, nil
	}
	rows, err := q.ListActiveFirmware(ctx, db.ListActiveFirmwareParams{DeviceID: dev.ID, FwModel: model})
	if err != nil {
		return nil, fmt.Errorf("list active firmware: %w", err)
	}
	builds := make([]activeBuild, len(rows))
	for i := range rows {
		r := &rows[i]
		builds[i] = activeBuild{
			fw:     &r.Firmware,
			ro:     rollout{state: stateActive, canaryID: r.CanaryDeviceID, canaryModel: r.CanaryModel, canaryVersion: r.CanaryVersion, canaryOffers: r.CanaryOffers},
			offers: r.Offers,
		}
	}
	return decide(dev, model, version, builds, nil), nil
}

// onDisk reports whether the build's file is in dir, at the recorded size.
func onDisk(dir string, fw *db.Firmware) bool {
	if !ValidSHA(fw.Sha256) {
		return false
	}
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
		fw := pick(cands, func(fw *db.Firmware) bool { return good[fw.ID] })
		if fw == nil {
			return nil
		}
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

// DeviceOffer is one device's standing for one build.
type DeviceOffer struct {
	DeviceID   int64
	FriendlyID string
	Name       string
	// Model and Version are what the device last reported.
	Model   string
	Version string
	// Canary is whether the device is this build's rollout canary.
	Canary bool
	// Offers is how many times the device has been offered the build.
	Offers int64
	// LastOfferedAt is when the device was last offered the build, in Unix
	// seconds, or 0 for never.
	LastOfferedAt int64
	// Reason is why /api/display would not send the device this build now,
	// or "" when it would.
	Reason Reason
	// Instead is the ID of the newer build it would send instead, when
	// Reason is ReasonNewerBuild, and 0 otherwise.
	Instead int64
	// Why is the sentence for Reason, or "" when the device is offered the
	// build.
	Why string
	// CanBeCanary is whether StartRollout would accept the device as the
	// build's canary now.
	CanBeCanary bool
}

// DeviceOffers returns, for each device whose last reported model is the
// build's, ordered by friendly ID, whether /api/display would send it this
// build now. Reason "" means it would: on a wake with the device's stored
// model and version, Offer returns this build. ReasonNewerBuild means it
// would send another, newer build (Instead). Any other reason is why this
// build is refused or cannot be sent. It decides as Offer does, with the
// same decide and pick over every active build of the model and this one,
// so the two cannot disagree. Offer uses the headers of the wake itself,
// which are the stored values unless a wake leaves one out: /api/display
// then keeps the stored value but offers nothing, so a device listed as
// offered is not offered on such a wake. The listing never changes what is
// offered. It takes one query for the build, one for the devices, one for
// the active builds with their offer counts, and one stat of each build's
// file, however many devices there are. Its queries agree only if q reads
// one snapshot, such as the Queries of a db.Client.ReadTx. It wraps
// ErrNoFirmware for an unknown build.
func DeviceOffers(ctx context.Context, q *db.Queries, dir string, firmwareID int64) ([]DeviceOffer, error) {
	row, err := q.GetFirmwareEntry(ctx, firmwareID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: %d", ErrNoFirmware, firmwareID)
	}
	if err != nil {
		return nil, fmt.Errorf("get firmware: %w", err)
	}
	entry := db.ListFirmwareRow(row)
	listed := &entry.Firmware
	devs, err := q.ListFirmwareDevices(ctx, db.ListFirmwareDevicesParams{FirmwareID: firmwareID, FwModel: listed.FwModel})
	if err != nil {
		return nil, fmt.Errorf("list firmware devices: %w", err)
	}
	active, err := q.ListActiveFirmwareOffers(ctx, listed.FwModel)
	if err != nil {
		return nil, fmt.Errorf("list active firmware: %w", err)
	}

	// Group the rows by build. counts[build][device] is how many times the
	// device was offered it, and a missing device was offered it never.
	var builds []activeBuild
	counts := map[int64]map[int64]int64{}
	for i := range active {
		r := &active[i]
		id := r.Firmware.ID
		if counts[id] == nil {
			counts[id] = map[int64]int64{}
			builds = append(builds, activeBuild{
				fw: &r.Firmware,
				ro: rollout{state: stateActive, canaryID: r.CanaryDeviceID, canaryModel: r.CanaryModel, canaryVersion: r.CanaryVersion, canaryOffers: r.CanaryOffers},
			})
		}
		if r.OfferedDeviceID != nil {
			counts[id][*r.OfferedDeviceID] = derefInt(r.OfferedOffers)
		}
	}
	// The build itself is decided with its own rollout, which is not active
	// when it has none or was canceled.
	if counts[listed.ID] == nil {
		builds = append(builds, activeBuild{fw: listed, ro: rolloutOf(&entry)})
	}
	counts[listed.ID] = make(map[int64]int64, len(devs))
	for i := range devs {
		counts[listed.ID][devs[i].Device.ID] = devs[i].Offers
	}
	fileOK := make(map[int64]bool, len(builds))
	for i := range builds {
		fileOK[builds[i].fw.ID] = onDisk(dir, builds[i].fw)
	}
	hasFile := func(fw *db.Firmware) bool { return fileOK[fw.ID] }

	canaryID := rolloutOf(&entry).canaryID
	refused := map[int64]refusal{}
	out := make([]DeviceOffer, len(devs))
	for i := range devs {
		d := &devs[i]
		for j := range builds {
			builds[j].offers = counts[builds[j].fw.ID][d.Device.ID]
		}
		clear(refused)
		cands := decide(&d.Device, d.Device.FwModel, d.Device.FwVersion, builds, refused)
		o := DeviceOffer{
			DeviceID: d.Device.ID, FriendlyID: d.Device.FriendlyID, Name: d.Device.Name,
			Model: d.Device.FwModel, Version: d.Device.FwVersion, Canary: d.Device.ID == canaryID,
			Offers: d.Offers, LastOfferedAt: d.LastOfferedAt,
		}
		r, isRefused := refused[listed.ID]
		o.CanBeCanary = !isRefused || !r.byDevice
		chosen := pick(cands, hasFile)
		switch {
		case isRefused:
			o.Reason, o.Why = r.reason, r.why
		case chosen != nil && chosen.ID == listed.ID:
			// Offered.
		case !fileOK[listed.ID]:
			// A candidate, skipped for its file as Offer skips it.
			o.Reason, o.Why = ReasonFileMissing, "the build's file is missing or is not the size that was added"
		default:
			// A candidate with its file, so a newer candidate with its file
			// was chosen.
			o.Reason, o.Instead = ReasonNewerBuild, chosen.ID
			o.Why = "the device is offered the newer version " + chosen.Version + " instead"
		}
		out[i] = o
	}
	return out, nil
}
