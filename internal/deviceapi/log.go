package deviceapi

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/die-net/trmnl-byos-go/internal/db"
)

const (
	// maxLogBody bounds an /api/log request body.
	maxLogBody = 1 << 20
	// keepLogs is how many log entries are kept per device.
	keepLogs = 1000
)

// logEntry is the part of a firmware log entry that gets its own columns.
// The whole entry is kept as sent.
type logEntry struct {
	CreatedAt int64  `json:"created_at"`
	Level     string `json:"level"`
	Message   string `json:"message"`
}

func (s *Server) log(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	debugRequest(r)
	c, err := s.identify(ctx, r)
	if requestEnded(r, "log: authenticate", err) {
		return
	}
	if err != nil {
		slog.ErrorContext(ctx, "log: authenticate", "err", err)
		http.Error(w, "lookup failed", http.StatusInternalServerError)
		return
	}
	if c.blocked() {
		http.Error(w, "device is blocked", http.StatusForbidden)
		return
	}
	if !c.ok {
		http.Error(w, "unknown device or bad token", http.StatusUnauthorized)
		return
	}
	dev := c.dev
	var body struct {
		Logs []json.RawMessage `json:"logs"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxLogBody)).Decode(&body); err != nil {
		http.Error(w, "bad log body", http.StatusBadRequest)
		return
	}
	now := s.now().Unix()
	err = s.db.InTx(ctx, func(q *db.Queries) error {
		for _, raw := range body.Logs {
			// An entry whose fields have unexpected types is still kept
			// whole in detail, with whatever columns did parse.
			var e logEntry
			_ = json.Unmarshal(raw, &e)
			err := q.InsertDeviceLog(ctx, db.InsertDeviceLogParams{
				DeviceID:   dev.ID,
				ReceivedAt: now,
				DeviceAt:   e.CreatedAt,
				Level:      e.Level,
				Message:    e.Message,
				Detail:     string(raw),
			})
			if err != nil {
				return fmt.Errorf("save log entry: %w", err)
			}
		}
		if err := q.TrimDeviceLog(ctx, db.TrimDeviceLogParams{DeviceID: dev.ID, Keep: keepLogs}); err != nil {
			return fmt.Errorf("trim log: %w", err)
		}
		return nil
	})
	if requestEnded(r, "log", err) {
		return
	}
	if err != nil {
		slog.ErrorContext(ctx, "log", "mac", dev.Mac, "err", err)
		http.Error(w, "save failed", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
