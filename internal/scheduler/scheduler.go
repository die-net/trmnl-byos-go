// Package scheduler renders, in the background, the plugin instances that
// devices' playlists show, so a device's request never waits on a render.
//
// Renders follow data, not wakes: an instance is rendered again when its
// recipe's refresh interval has passed, for each size and palette a device
// showing it needs, and every device of that size and palette shares the
// render.
package scheduler

import (
	"cmp"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/die-net/trmnl-byos-go/internal/db"
	"github.com/die-net/trmnl-byos-go/internal/imagestore"
	"github.com/die-net/trmnl-byos-go/internal/profile"
	"github.com/die-net/trmnl-byos-go/internal/recipe"
	"github.com/die-net/trmnl-byos-go/internal/render"
	"github.com/die-net/trmnl-byos-go/internal/secrets"
)

// RetryAfter is how long after a failed build the next one is tried.
const RetryAfter = 5 * time.Minute

// checkEvery is how often the scheduler looks for renders that are due.
const checkEvery = 30 * time.Second

// Config is how a scheduler is set up.
type Config struct {
	DB       *db.Client
	Images   *imagestore.Store
	Renderer *render.Client
	// RecipesDir holds the installed recipes.
	RecipesDir string
	// Secrets opens instances' secret field values.
	Secrets *secrets.Key
	// TimeZone is the IANA time zone recipes render in, or "" for UTC.
	TimeZone string
	// AssetHosts are the hosts whose assets the renderer's proxy caches.
	AssetHosts []string
}

// Scheduler renders what is due.
type Scheduler struct {
	cfg Config
	now func() time.Time
}

// New returns a scheduler.
func New(cfg Config) *Scheduler {
	return &Scheduler{cfg: cfg, now: time.Now}
}

// Run renders what is due every checkEvery until ctx is done.
func (s *Scheduler) Run(ctx context.Context) {
	for {
		if err := s.RenderDue(ctx); err != nil && ctx.Err() == nil {
			slog.Error("scheduler", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(checkEvery):
		}
	}
}

// target is one render: an instance at a size and palette.
type target struct {
	instance      int64
	width, height int
	palette       string
	refresh       time.Duration
}

// RenderDue renders, one at a time, every target that is due. A failed
// render is recorded and does not stop the others.
func (s *Scheduler) RenderDue(ctx context.Context) error {
	targets, err := s.targets(ctx)
	if err != nil {
		return err
	}
	for _, t := range targets {
		due, err := s.due(ctx, t)
		if err != nil {
			return err
		}
		if !due {
			continue
		}
		if err := s.render(ctx, t); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			slog.Warn("render failed", "instance", t.instance, "width", t.width, "height", t.height, "palette", t.palette, "err", err)
		}
	}
	return nil
}

// targets lists what devices' playlists need rendered, each once, in a
// fixed order.
func (s *Scheduler) targets(ctx context.Context) ([]target, error) {
	rows, err := s.cfg.DB.Read.ListRenderTargets(ctx)
	if err != nil {
		return nil, fmt.Errorf("list render targets: %w", err)
	}
	seen := make(map[target]bool, len(rows))
	for i := range rows {
		r := &rows[i]
		p, err := profile.Resolve(r.FwModel, r.Width, r.Height)
		if err != nil {
			return nil, err
		}
		seen[target{
			instance: r.InstanceID,
			width:    p.Width,
			height:   p.Height,
			palette:  p.RenderPalette(r.Palette),
			refresh:  time.Duration(r.RefreshInterval) * time.Second,
		}] = true
	}
	return slices.SortedFunc(maps.Keys(seen), func(a, b target) int {
		return cmp.Or(cmp.Compare(a.instance, b.instance), cmp.Compare(a.width, b.width),
			cmp.Compare(a.height, b.height), strings.Compare(a.palette, b.palette))
	}), nil
}

// due reports whether t has never been rendered, its refresh interval has
// passed since its last good render, or RetryAfter has passed since a
// failed one.
func (s *Scheduler) due(ctx context.Context, t target) (bool, error) {
	r, err := s.cfg.DB.Read.GetRender(ctx, db.GetRenderParams{
		InstanceID: t.instance, Width: int64(t.width), Height: int64(t.height), Palette: t.palette,
	})
	if errors.Is(err, sql.ErrNoRows) {
		return true, nil
	}
	if err != nil {
		return false, fmt.Errorf("get render: %w", err)
	}
	now := s.now()
	if r.Error != "" {
		return !now.Before(time.Unix(r.AttemptedAt, 0).Add(RetryAfter)), nil
	}
	return !now.Before(time.Unix(r.RenderedAt, 0).Add(t.refresh)), nil
}

// stored is an image a build put in the image store.
type stored struct {
	name string
	size int64
}

// render builds t and records the outcome.
func (s *Scheduler) render(ctx context.Context, t target) error {
	start := s.now()
	images, log, err := s.build(ctx, t)
	took := s.now().Sub(start)
	slog.Info("render", "instance", t.instance, "width", t.width, "height", t.height, "palette", t.palette,
		"seconds", took.Seconds(), "err", err)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		serr := s.cfg.DB.Write.SaveRenderFailure(ctx, db.SaveRenderFailureParams{
			InstanceID:  t.instance,
			Width:       int64(t.width),
			Height:      int64(t.height),
			Palette:     t.palette,
			AttemptedAt: start.Unix(),
			DurationMs:  took.Milliseconds(),
			Error:       describe(err, log),
			Log:         log,
		})
		if serr != nil {
			return errors.Join(err, fmt.Errorf("save render failure: %w", serr))
		}
		return err
	}
	return s.cfg.DB.InTx(ctx, func(q *db.Queries) error {
		id, err := q.SaveRenderSuccess(ctx, db.SaveRenderSuccessParams{
			InstanceID:  t.instance,
			Width:       int64(t.width),
			Height:      int64(t.height),
			Palette:     t.palette,
			RenderedAt:  s.now().Unix(),
			AttemptedAt: start.Unix(),
			DurationMs:  took.Milliseconds(),
			Log:         log,
		})
		if err != nil {
			return fmt.Errorf("save render: %w", err)
		}
		for layout, img := range images {
			err := q.SaveRenderImage(ctx, db.SaveRenderImageParams{RenderID: id, Layout: layout, Image: img.name, Bytes: img.size})
			if err != nil {
				return fmt.Errorf("save render image: %w", err)
			}
		}
		return nil
	})
}

// build sends t's project to the renderer and stores the images it returns.
func (s *Scheduler) build(ctx context.Context, t target) (map[string]stored, string, error) {
	inst, err := s.cfg.DB.Read.GetInstance(ctx, t.instance)
	if err != nil {
		return nil, "", fmt.Errorf("get instance: %w", err)
	}
	fields, err := recipe.InstanceFields(&inst, s.cfg.Secrets)
	if err != nil {
		return nil, "", err
	}
	proj := recipe.NewProject(recipe.Dir(s.cfg.RecipesDir, inst.RecipeID), fields, s.cfg.TimeZone, s.cfg.AssetHosts)
	images := make(map[string]stored, len(render.Layouts))
	log, err := s.cfg.Renderer.Build(ctx,
		render.Job{Width: t.width, Height: t.height, Palette: t.palette, Env: proj.Env},
		proj.Write,
		func(layout string, r io.Reader) error {
			name, size, err := s.cfg.Images.Put(r)
			if err != nil {
				return err
			}
			images[layout] = stored{name: name, size: size}
			return nil
		})
	if err != nil {
		return nil, log, err
	}
	return images, log, nil
}

// describe is the one line a device shows for a failed build: trmnlp's last
// line of output when trmnlp failed, since that is where it says why.
func describe(err error, log string) string {
	if errors.Is(err, render.ErrBuildFailed) {
		lines := strings.Split(strings.TrimSpace(log), "\n")
		if last := strings.TrimSpace(lines[len(lines)-1]); last != "" {
			return "trmnlp: " + last
		}
	}
	return err.Error()
}
