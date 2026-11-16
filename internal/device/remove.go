package device

import (
	"context"
	"fmt"
	"time"

	"github.com/die-net/trmnl-byos-go/internal/db"
	"github.com/die-net/trmnl-byos-go/internal/errkind"
)

// ErrNotBlocked means an unblock named a MAC that is not blocked.
var ErrNotBlocked = errkind.New(errkind.ErrNotFound, "not blocked")

// Delete deletes the device named by its friendly ID or MAC, with its
// state, logs, playlist links, firmware offers and renders, and returns it.
// A firmware rollout whose canary it is goes too, so nothing is offered to
// the rest of its model until a new rollout names another canary. The
// device's MAC is kept as a tombstone in the same transaction, so the
// device's next request gets reset_firmware and goes back through setup.
// Its images stay until image expiry finds no row that names them.
func Delete(ctx context.Context, client *db.Client, device string, now time.Time) (db.Device, error) {
	var dev db.Device
	err := client.InTx(ctx, func(q *db.Queries) error {
		var err error
		if dev, err = FindDevice(ctx, q, device); err != nil {
			return err
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
// device keeps its row, so Unblock gives it back as it was.
func Block(ctx context.Context, client *db.Client, device string, now time.Time) (db.BlockedMac, error) {
	var out db.BlockedMac
	err := client.InTx(ctx, func(q *db.Queries) error {
		mac, err := blockMAC(ctx, q, device)
		if err != nil {
			return err
		}
		if out, err = q.BlockMAC(ctx, db.BlockMACParams{Mac: mac, BlockedAt: now.Unix()}); err != nil {
			return fmt.Errorf("block: %w", err)
		}
		return nil
	})
	return out, err
}

// Unblock lifts the block of a MAC, given as a MAC or as the friendly ID of
// a registered device, and returns the MAC. It is ErrNotBlocked if the MAC
// is not blocked. A deleted device's tombstone, if it has one, then resets
// it at its next request.
func Unblock(ctx context.Context, client *db.Client, device string) (string, error) {
	var mac string
	err := client.InTx(ctx, func(q *db.Queries) error {
		var err error
		if mac, err = blockMAC(ctx, q, device); err != nil {
			return err
		}
		n, err := q.UnblockMAC(ctx, mac)
		if err != nil {
			return fmt.Errorf("unblock: %w", err)
		}
		if n == 0 {
			return fmt.Errorf("%w: %s", ErrNotBlocked, mac)
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

// blockMAC returns the MAC a block names: device itself if it is a MAC,
// registered or not, and otherwise the MAC of the device with that friendly
// ID.
func blockMAC(ctx context.Context, q *db.Queries, device string) (string, error) {
	if mac, ok := ParseMAC(device); ok {
		return mac, nil
	}
	dev, err := FindDevice(ctx, q, device)
	if err != nil {
		return "", err
	}
	return dev.Mac, nil
}
