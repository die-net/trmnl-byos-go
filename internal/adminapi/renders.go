package adminapi

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/die-net/trmnl-byos-go/internal/errkind"
)

// errNoRender means no render target has the ID.
var errNoRender = errkind.New(errkind.ErrNotFound, "no such render")

// renderJSON is the status of one render target: an instance at one size
// and palette, for one device or shared by all.
type renderJSON struct {
	ID         int64 `json:"id"`
	InstanceID int64 `json:"instance_id"`
	// Device is the friendly ID of the device a per-device render is for,
	// and null for a render every device shares.
	Device  *string `json:"device"`
	Width   int64   `json:"width"`
	Height  int64   `json:"height"`
	Palette string  `json:"palette"`
	// RenderedAt is when the latest successful build finished, 0 if none
	// has.
	RenderedAt  int64 `json:"rendered_at"`
	AttemptedAt int64 `json:"attempted_at"`
	// DurationSeconds is how long the latest build took.
	DurationSeconds float64 `json:"duration_seconds"`
	// Error is why the latest build failed, empty if it succeeded.
	Error string `json:"error"`
}

// renderDetailJSON is a render target's status with its build log.
type renderDetailJSON struct {
	renderJSON
	Log string `json:"log"`
}

func (s *Server) listRenders(_ http.ResponseWriter, r *http.Request) ([]renderJSON, error) {
	rows, err := s.db.Read.ListRenders(r.Context())
	if err != nil {
		return nil, fmt.Errorf("list renders: %w", err)
	}
	out := make([]renderJSON, 0, len(rows))
	for _, x := range rows {
		out = append(out, renderJSON{
			ID: x.ID, InstanceID: x.InstanceID, Device: x.Device, Width: x.Width, Height: x.Height, Palette: x.Palette,
			RenderedAt: x.RenderedAt, AttemptedAt: x.AttemptedAt, DurationSeconds: float64(x.DurationMs) / 1000, Error: x.Error,
		})
	}
	return out, nil
}

func (s *Server) getRender(_ http.ResponseWriter, r *http.Request) (renderDetailJSON, error) {
	id, err := pathID(r, errNoRender)
	if err != nil {
		return renderDetailJSON{}, err
	}
	x, err := s.db.Read.GetRenderStatus(r.Context(), id)
	if errors.Is(err, sql.ErrNoRows) {
		return renderDetailJSON{}, fmt.Errorf("%w: %d", errNoRender, id)
	}
	if err != nil {
		return renderDetailJSON{}, fmt.Errorf("get render: %w", err)
	}
	return renderDetailJSON{
		renderJSON: renderJSON{
			ID: x.ID, InstanceID: x.InstanceID, Device: x.Device, Width: x.Width, Height: x.Height, Palette: x.Palette,
			RenderedAt: x.RenderedAt, AttemptedAt: x.AttemptedAt, DurationSeconds: float64(x.DurationMs) / 1000, Error: x.Error,
		},
		Log: x.Log,
	}, nil
}

// renderWaitLimit is how long waitRender holds a request open.
const renderWaitLimit = 30 * time.Second

// waitRender answers with a render target's status once a build newer than
// the after query parameter (Unix seconds) has been attempted, so a page
// can show a rebuild finish without polling. After renderWaitLimit it
// answers with the status as it is.
func (s *Server) waitRender(_ http.ResponseWriter, r *http.Request) (renderDetailJSON, error) {
	id, err := pathID(r, errNoRender)
	if err != nil {
		return renderDetailJSON{}, err
	}
	after, err := strconv.ParseInt(r.URL.Query().Get("after"), 10, 64)
	if err != nil {
		return renderDetailJSON{}, errkind.New(errkind.ErrInvalid, "after must be Unix seconds")
	}
	ctx, cancel := context.WithTimeout(r.Context(), renderWaitLimit)
	defer cancel()
	for {
		x, err := s.db.Read.GetRenderStatus(ctx, id)
		if errors.Is(err, sql.ErrNoRows) {
			return renderDetailJSON{}, fmt.Errorf("%w: %d", errNoRender, id)
		}
		if err != nil {
			return renderDetailJSON{}, fmt.Errorf("get render: %w", err)
		}
		if x.AttemptedAt > after {
			return renderDetailJSON{
				renderJSON: renderJSON{
					ID: x.ID, InstanceID: x.InstanceID, Device: x.Device, Width: x.Width, Height: x.Height, Palette: x.Palette,
					RenderedAt: x.RenderedAt, AttemptedAt: x.AttemptedAt, DurationSeconds: float64(x.DurationMs) / 1000, Error: x.Error,
				},
				Log: x.Log,
			}, nil
		}
		select {
		case <-ctx.Done():
			return renderDetailJSON{}, fmt.Errorf("wait for render: %w", ctx.Err())
		case <-time.After(time.Second):
		}
	}
}
