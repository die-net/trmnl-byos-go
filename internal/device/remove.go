package device

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/die-net/trmnl-byos-go/internal/db"
	"github.com/die-net/trmnl-byos-go/internal/errkind"
)

// ErrNotBlocked means an unblock named a MAC that is not blocked.
var ErrNotBlocked = errkind.New(errkind.ErrNotFound, "not blocked")

// ErrDeviceChanged means a delete, block or unblock was given the Standing
// a page showed, and the device or MAC no longer stands so. Nothing was
// changed.
var ErrDeviceChanged = errkind.New(errkind.ErrConflict, "the device has changed")

// Standing is what the server knows of a MAC, or of the device that has it:
// what a page shows before a delete, block or unblock, and what the change
// checks again inside its transaction.
type Standing struct {
	// MAC is the MAC, in the firmware's form.
	MAC string
	// DeviceID is the registered device that has the MAC, 0 for none. A
	// friendly ID can be issued again after a delete, so the ID is what
	// tells a page's device from a newer one.
	DeviceID int64
	// Blocked is whether the MAC is blocked.
	Blocked bool
	// Deleted is whether the MAC is a deleted device's tombstone, which an
	// unblock then lets reset the device into setup. A registered MAC has
	// no tombstone, and an unblock gives it back as it was.
	Deleted bool
	// Canary lists, in order, the firmware builds whose rollout has the
	// device as its canary, which a delete of the device deletes.
	Canary []int64
}

// equal reports whether s and o are the same standing. A nil and an empty
// Canary are the same.
func (s *Standing) equal(o *Standing) bool {
	return s.MAC == o.MAC && s.DeviceID == o.DeviceID && s.Blocked == o.Blocked && s.Deleted == o.Deleted && slices.Equal(s.Canary, o.Canary)
}

// ReadStanding returns the standing of a MAC, registered or not, or of the
// device with a friendly ID, read through q. An unknown friendly ID is
// ErrNoDevice. Delete, Block and Unblock check a page's standing against
// what this returns inside their transactions, so the two cannot disagree.
func ReadStanding(ctx context.Context, q *db.Queries, device string) (Standing, error) {
	mac, ok := ParseMAC(device)
	if !ok {
		dev, err := FindDevice(ctx, q, device)
		if err != nil {
			return Standing{}, err
		}
		mac = dev.Mac
	}
	row, err := q.GetMACStanding(ctx, mac)
	if err != nil {
		return Standing{}, fmt.Errorf("get mac standing: %w", err)
	}
	s := Standing{MAC: mac, Blocked: row.Blocked != 0, Deleted: row.Deleted != 0}
	if row.Registered == 0 {
		return s, nil
	}
	dev, err := q.GetDeviceByMAC(ctx, mac)
	if err != nil {
		return Standing{}, fmt.Errorf("get device: %w", err)
	}
	s.DeviceID = dev.ID
	if s.Canary, err = q.ListCanaryFirmware(ctx, dev.ID); err != nil {
		return Standing{}, fmt.Errorf("list canary firmware: %w", err)
	}
	return s, nil
}

// checkSeen returns ErrDeviceChanged if seen is not nil and is not now.
func checkSeen(seen, now *Standing) error {
	if seen != nil && !seen.equal(now) {
		return fmt.Errorf("%w: %s", ErrDeviceChanged, now.MAC)
	}
	return nil
}

// Delete deletes the device named by its friendly ID or MAC, with its
// state, logs, playlist links, firmware offers and renders, and returns it.
// A firmware rollout whose canary it is goes too, so nothing is offered to
// the rest of its model until a new rollout names another canary. The
// device's MAC is kept as a tombstone in the same transaction, so the
// device's next request gets reset_firmware and goes back through setup.
// Its images stay until image expiry finds no row that names them.
//
// seen is the Standing a page showed, or nil from the command line and the
// API. When it is not the device's standing now, as when the friendly ID
// now names a newer device, Delete is ErrDeviceChanged and changes nothing.
func Delete(ctx context.Context, client *db.Client, device string, seen *Standing, now time.Time) (db.Device, error) {
	var dev db.Device
	err := client.InTx(ctx, func(q *db.Queries) error {
		st, err := ReadStanding(ctx, q, device)
		if err != nil {
			return err
		}
		if st.DeviceID == 0 {
			return fmt.Errorf("%w: %s", ErrNoDevice, device)
		}
		if err := checkSeen(seen, &st); err != nil {
			return err
		}
		if dev, err = q.GetDevice(ctx, st.DeviceID); err != nil {
			return fmt.Errorf("get device: %w", err)
		}
		if err := q.DeleteDevice(ctx, dev.ID); err != nil {
			return fmt.Errorf("delete device: %w", err)
		}
		if err := q.AddDeviceTombstone(ctx, db.AddDeviceTombstoneParams{Mac: dev.Mac, DeletedAt: now.Unix()}); err != nil {
			return fmt.Errorf("add tombstone: %w", err)
		}
		return nil
	})
	return dev, err
}

// Block blocks a MAC, given as a MAC or as the friendly ID of a registered
// device, and returns the block. A MAC need not be registered, and a block
// of one already blocked keeps when it was first blocked. A registered
// device keeps its row, so Unblock gives it back as it was. seen is as for
// Delete.
func Block(ctx context.Context, client *db.Client, device string, seen *Standing, now time.Time) (db.BlockedMac, error) {
	var out db.BlockedMac
	err := client.InTx(ctx, func(q *db.Queries) error {
		st, err := ReadStanding(ctx, q, device)
		if err != nil {
			return err
		}
		if err := checkSeen(seen, &st); err != nil {
			return err
		}
		if out, err = q.BlockMAC(ctx, db.BlockMACParams{Mac: st.MAC, BlockedAt: now.Unix()}); err != nil {
			return fmt.Errorf("block: %w", err)
		}
		return nil
	})
	return out, err
}

// Unblock lifts the block of a MAC, given as a MAC or as the friendly ID of
// a registered device, and returns the MAC. It is ErrNotBlocked if the MAC
// is not blocked, whatever seen says. A deleted device's tombstone, if it
// has one, then resets it at its next request, and a registered device is
// back as it was. seen is as for Delete.
func Unblock(ctx context.Context, client *db.Client, device string, seen *Standing) (string, error) {
	var mac string
	err := client.InTx(ctx, func(q *db.Queries) error {
		st, err := ReadStanding(ctx, q, device)
		if err != nil {
			return err
		}
		mac = st.MAC
		if !st.Blocked {
			return fmt.Errorf("%w: %s", ErrNotBlocked, mac)
		}
		if err := checkSeen(seen, &st); err != nil {
			return err
		}
		if _, err := q.UnblockMAC(ctx, mac); err != nil {
			return fmt.Errorf("unblock: %w", err)
		}
		return nil
	})
	return mac, err
}

// ListBlocked returns every blocked MAC, ordered by MAC, with the friendly
// ID and name of the device that has it, if one does.
func ListBlocked(ctx context.Context, q *db.Queries) ([]db.ListBlockedMACsRow, error) {
	rows, err := q.ListBlockedMACs(ctx)
	if err != nil {
		return nil, fmt.Errorf("list blocked: %w", err)
	}
	return rows, nil
}
