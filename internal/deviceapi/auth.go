package deviceapi

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"net/http"

	"github.com/die-net/trmnl-byos-go/internal/db"
	"github.com/die-net/trmnl-byos-go/internal/device"
)

// authenticate finds the device whose API key is the Access-Token header,
// by the key's hash, through q. Looking up a hash leaks nothing useful
// through timing, unlike looking up or comparing the key itself. The ID
// header is optional, since some non-TRMNL clients leave it out, but when
// present it must be the device's MAC. ok is false for an unknown key or a
// mismatched ID.
func authenticate(ctx context.Context, q *db.Queries, r *http.Request) (dev db.Device, ok bool, err error) {
	token := r.Header.Get("Access-Token")
	if token == "" {
		return db.Device{}, false, nil
	}
	dev, err = q.GetDeviceByAPIKeyHash(ctx, hashKey(token))
	if errors.Is(err, sql.ErrNoRows) {
		return db.Device{}, false, nil
	}
	if err != nil {
		return db.Device{}, false, fmt.Errorf("get device: %w", err)
	}
	if id := r.Header.Get("ID"); id != "" {
		if mac, valid := device.ParseMAC(id); !valid || mac != dev.Mac {
			return db.Device{}, false, nil
		}
	}
	return dev, true, nil
}

// caller is who a device request comes from, read in one snapshot.
type caller struct {
	// dev is the device whose key the request holds, when ok is true.
	dev db.Device
	ok  bool
	// mac is the device's MAC when ok, and otherwise the ID header's, or ""
	// if there is none or it is not a MAC.
	mac string
	// standing is what the server knows of mac, all false when mac is "".
	standing db.GetMACStandingRow
}

// identify reads who a device request comes from: the device its key
// belongs to, and whether its MAC is blocked, deleted or registered. The
// reads share one snapshot, so a device deleted meanwhile is seen either
// whole or gone, never half of each.
func (s *Server) identify(ctx context.Context, r *http.Request) (caller, error) {
	var c caller
	err := s.db.ReadTx(ctx, func(q *db.Queries) error {
		c = caller{}
		var err error
		if c.dev, c.ok, err = authenticate(ctx, q, r); err != nil {
			return err
		}
		if c.ok {
			c.mac = c.dev.Mac
		} else {
			c.mac, _ = device.ParseMAC(r.Header.Get("ID"))
		}
		if c.mac == "" {
			return nil
		}
		if c.standing, err = q.GetMACStanding(ctx, c.mac); err != nil {
			return fmt.Errorf("get mac standing: %w", err)
		}
		return nil
	})
	return c, err
}

// blocked reports whether the request's MAC is blocked. A block wins over
// the request's key, a tombstone, and the join window.
func (c *caller) blocked() bool { return c.standing.Blocked != 0 }

// reset reports whether a request that holds no valid key is told to reset,
// which erases its key, Wi-Fi settings and server URL and starts its setup
// portal. That is for a request with a key from a MAC that is registered
// with another key, because it was paired again since, or that was
// deleted. Such a device would otherwise keep its old key and never run
// setup again. A blocked MAC is never reset. Neither is a request with no
// token, no ID, or an unknown MAC: it may come from a server that lost its
// database, and resetting then would wipe every display at once.
func (c *caller) reset(r *http.Request) bool {
	// With no ID, mac is "" and standing is all false.
	if c.ok || c.blocked() || r.Header.Get("Access-Token") == "" {
		return false
	}
	return c.standing.Registered != 0 || c.standing.Deleted != 0
}

// hashKey is how a device API key is stored and looked up. The key is 128
// random bits, so a fast hash is enough: there is nothing to brute-force.
func hashKey(key string) []byte {
	sum := sha256.Sum256([]byte(key))
	return sum[:]
}
