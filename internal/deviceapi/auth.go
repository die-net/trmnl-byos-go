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
// by the key's hash. Looking up a hash leaks nothing useful through timing,
// unlike looking up or comparing the key itself. The ID header is optional,
// since some non-TRMNL clients leave it out, but when present it must be
// the device's MAC. ok is false for an unknown key or a mismatched ID.
func (s *Server) authenticate(ctx context.Context, r *http.Request) (dev db.Device, ok bool, err error) {
	token := r.Header.Get("Access-Token")
	if token == "" {
		return db.Device{}, false, nil
	}
	dev, err = s.db.Read.GetDeviceByAPIKeyHash(ctx, hashKey(token))
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

// staleKey reports whether a request that failed authenticate comes from a
// registered MAC holding a key that is no longer its key, because the MAC
// was paired again since. Such a device would otherwise keep its old key
// and never run setup again, so it is told to reset, which erases its key,
// Wi-Fi settings and server URL and starts its setup portal. A request with
// no token, no ID, or an unknown MAC is not stale: it may come from a
// server that lost its database, and resetting then would wipe every
// display at once.
func (s *Server) staleKey(ctx context.Context, r *http.Request) (bool, error) {
	if r.Header.Get("Access-Token") == "" {
		return false, nil
	}
	mac, ok := device.ParseMAC(r.Header.Get("ID"))
	if !ok {
		return false, nil
	}
	_, err := s.db.Read.GetDeviceByMAC(ctx, mac)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("get device: %w", err)
	}
	return true, nil
}

// hashKey is how a device API key is stored and looked up. The key is 128
// random bits, so a fast hash is enough: there is nothing to brute-force.
func hashKey(key string) []byte {
	sum := sha256.Sum256([]byte(key))
	return sum[:]
}
