// Package scheduler renders, in the background, the plugin instances that
// devices' playlists show, so a device's request never waits on a render.
//
// Renders follow data, not wakes: an instance is rendered again when its
// recipe's refresh interval has passed, for each size, palette, time zone and
// locale a device showing it needs, and every device of that size, palette,
// time zone and locale shares the render. A build starts shortly before the wake that would otherwise
// show a render older than the interval, and when data pushed to the
// instance changes. A recipe that reads trmnl.device is rendered once per device
// instead, with that device's values.
package scheduler

import (
	"archive/tar"
	"cmp"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"index/suffixarray"
	"io"
	"log/slog"
	"maps"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/die-net/trmnl-byos-go/internal/db"
	"github.com/die-net/trmnl-byos-go/internal/device"
	"github.com/die-net/trmnl-byos-go/internal/imagestore"
	"github.com/die-net/trmnl-byos-go/internal/playlist"
	"github.com/die-net/trmnl-byos-go/internal/profile"
	"github.com/die-net/trmnl-byos-go/internal/recipe"
	"github.com/die-net/trmnl-byos-go/internal/render"
	"github.com/die-net/trmnl-byos-go/internal/secrets"
	"github.com/die-net/trmnl-byos-go/internal/settings"
)

// maxRetryAfter is the longest wait between builds of a target that keeps
// failing.
const maxRetryAfter = time.Hour

// retryDelay is how long to wait after the failures-th failed build in a row
// before trying again: db.RenderRetryAfter after the first, doubling after each
// further one, up to maxRetryAfter. A row from before failures were counted
// has 0 and counts as one.
func retryDelay(failures int64) time.Duration {
	d := db.RenderRetryAfter
	for i := int64(1); i < failures && d < maxRetryAfter; i++ {
		d *= 2
	}
	return min(d, maxRetryAfter)
}

// buildTimeout is the longest one build may take, from the request to the
// end of the response. The renderer stops a build at its own limit, 2
// minutes by default, and then sweeps up after it, for at most 40 seconds,
// before its response ends (render.Shim.ResponseTimeout), so this allows
// for that and the transfer. Past it the renderer is taken to be stuck, and
// the build fails like any other, to be retried after a backoff.
const buildTimeout = 5 * time.Minute

// checkEvery is how often the scheduler looks for renders that are due.
const checkEvery = 30 * time.Second

// unavailableWait is how long Run waits after the renderer did not take a
// build before it scans again, doubling while it stays unavailable, up to
// checkEvery (see unavailableBackoff).
const unavailableWait = 2 * time.Second

// unavailableBackoff is how long Run waits after the nth pass in a row, from
// 1, whose renderer did not take the build.
func unavailableBackoff(n int) time.Duration {
	d := unavailableWait
	for i := 1; i < n && d < checkEvery; i++ {
		d *= 2
	}
	return min(d, checkEvery)
}

// Config is how a scheduler is set up.
type Config struct {
	DB       *db.Client
	Images   *imagestore.Store
	Renderer Renderer
	// RecipesDir holds the installed recipes.
	RecipesDir string
	// Secrets opens instances' secret field values.
	Secrets *secrets.Key
	// Zone is the server's time zone, which a device with none of its own
	// has. Its schedules are read in its zone, and recipes render in it. Nil
	// is UTC.
	Zone *time.Location
	// AssetHosts are the hosts whose assets the renderer's proxy caches.
	AssetHosts []string
	// Wake makes Run scan at once rather than at its next check. A change
	// that builds read, such as an instance's settings, is sent on it by
	// Waker.Wake. Nil never wakes it.
	Wake Waker
}

// Renderer runs builds: a *render.Client, which sends them to a shim, or
// a *render.Sandbox, which runs each in a sandbox on this host. Build's
// errors are as render.Client.Build documents them.
type Renderer interface {
	Build(ctx context.Context, job render.Job, writeProject func(*tar.Writer) error,
		png func(layout string, r io.Reader) error) (string, error)
}

// Waker carries wake-ups to a scheduler. It holds at most one, since a
// scan after any number of changes finds them all.
type Waker chan struct{}

// NewWaker returns a Waker for Config.Wake.
func NewWaker() Waker { return make(Waker, 1) }

// Wake asks the scheduler to scan now. It never blocks: a wake-up already
// waiting covers this one. A nil Waker does nothing.
func (w Waker) Wake() {
	select {
	case w <- struct{}{}:
	default:
	}
}

// Scheduler renders what is due.
type Scheduler struct {
	cfg Config
	now func() time.Time
	// buildTimeout is how long one build may take: the buildTimeout constant.
	buildTimeout time.Duration

	// perDevice remembers by recipe ID whether a revision of the recipe's
	// files reads trmnl.device. A revision never changes, so it is scanned
	// once.
	mu        sync.Mutex
	perDevice map[int64]scanned
}

// scanned is whether one revision of a recipe's files reads trmnl.device.
type scanned struct {
	revision int64
	reads    bool
}

// New returns a scheduler.
func New(cfg Config) *Scheduler {
	cfg.Zone = cmp.Or(cfg.Zone, time.UTC)
	return &Scheduler{cfg: cfg, now: time.Now, buildTimeout: buildTimeout, perDevice: map[int64]scanned{}}
}

// Run renders what is due until ctx is done. Each pass scans for due
// targets and builds the most urgent one. If it built one, it scans again
// at once, since more may be due and one may have become more urgent while
// it built. A pass that built nothing waits, for checkEvery or until
// Config.Wake. A pass that could not reach the renderer waits
// unavailableWait, doubling while the renderer stays unreachable, up to
// checkEvery, without a wake cutting it short, so an outage costs one
// target's build at a time, not every due target's. Before its first scan
// it records each build the server stopped during (see RecoverAttempts).
func (s *Scheduler) Run(ctx context.Context) {
	if n, err := s.RecoverAttempts(ctx); err != nil && ctx.Err() == nil {
		slog.Error("scheduler: record builds the server stopped during", "err", err)
	} else if n > 0 {
		slog.Warn("scheduler: the server stopped during builds, recorded as failures", "builds", n)
	}
	unavailable := 0
	for {
		more, err := s.RenderNext(ctx)
		if errors.Is(err, render.ErrUnavailable) && ctx.Err() == nil {
			unavailable++
			wait := unavailableBackoff(unavailable)
			slog.Info("scheduler: waiting for the renderer", "seconds", wait.Seconds())
			select {
			case <-ctx.Done():
				return
			case <-time.After(wait):
			}
			continue
		}
		unavailable = 0
		if err != nil && ctx.Err() == nil {
			slog.Error("scheduler", "err", err)
		}
		if more && ctx.Err() == nil {
			continue
		}
		select {
		case <-ctx.Done():
			return
		case <-s.cfg.Wake:
		case <-time.After(checkEvery):
		}
	}
}

// target is one render: an instance at a size, palette, time zone and
// locale, for one device when the recipe reads trmnl.device.
type target struct {
	instance int64
	// device is the device's ID, or 0 for a render every device shares.
	device        int64
	width, height int
	palette       string
	// zone and locale are the IANA time zone name and the locale the render
	// is made for, which are the ones in effect for the devices that show
	// it, never empty.
	zone, locale string
	refresh      time.Duration
	// broken is why the target cannot be built, empty if it can. A broken
	// target is not sent to the renderer. It gets a render failure with
	// this reason, so its devices show it and it is retried after
	// RenderRetryAfter like any failed build.
	broken string
}

// RenderNext scans for due targets and builds the one wanted soonest. A
// failed build is recorded and is not an error here, except when the
// renderer did not take it: that is not recorded, and is returned, an
// error matching render.ErrUnavailable, so Run waits before it tries
// another target. It returns whether it recorded an outcome, success or
// failure, which is whether another scan may find more to do. It returns
// false when nothing was due, when the renderer could not be reached, and
// when the scan or the write of the outcome failed, so that a persistent
// failure waits for the next check and does not spin.
func (s *Scheduler) RenderNext(ctx context.Context) (built bool, err error) {
	targets, err := s.dueTargets(ctx)
	if err != nil || len(targets) == 0 {
		return false, err
	}
	t := targets[0]
	recorded, err := s.render(ctx, t.target, t.seen)
	if err != nil {
		if ctx.Err() != nil {
			return false, ctx.Err()
		}
		slog.Warn("render failed", "instance", t.instance, "device", t.device, "width", t.width, "height", t.height, "palette", t.palette, "zone", t.zone, "locale", t.locale, "err", err)
		if errors.Is(err, render.ErrUnavailable) {
			return false, err
		}
	}
	return recorded, nil
}

// candidate is a target and what is known about who will want it.
type candidate struct {
	target
	// woken is whether any device whose playlist has the target has woken.
	woken bool
	// read is what the instance has now that a build reads.
	read version
	// wanted is when the devices that will show the target at their next
	// wake expect to wake.
	wanted []time.Time
}

// job is a target that is due, with what the scan saw of its instance.
type job struct {
	target
	// seen is the instance's version when the scan found the target due. A
	// failed build records it for what the build did not get to read, so a
	// target whose build fails early is not due again by the same change.
	seen version
	// by is when the target is first wanted, the zero time if no wake
	// waits for it.
	by time.Time
}

// version is what a build reads of an instance that can change under it,
// each as a time that rises with every change.
type version struct {
	// data is the updated_at of the instance's pushed data, 0 if it has
	// none.
	data int64
	// settings is the instance's settings_at.
	settings int64
}

// dueTargets returns the targets that are due now. Those wanted by a wake
// come first, soonest first. The rest keep their fixed order. It reads every
// render row once, not one row per candidate.
func (s *Scheduler) dueTargets(ctx context.Context) ([]job, error) {
	cands, err := s.candidates(ctx)
	if err != nil {
		return nil, err
	}
	if err := s.pruneRenders(ctx, cands); err != nil {
		// Housekeeping, so it does not stop the builds.
		slog.Warn("prune renders", "err", err)
	}
	rows, err := s.cfg.DB.Read.ListRenderRows(ctx)
	if err != nil {
		return nil, fmt.Errorf("list render rows: %w", err)
	}
	renders := make(map[renderKey]*db.Render, len(rows))
	for i := range rows {
		r := &rows[i]
		// Only what due and lead read. The build log is not loaded.
		renders[renderKey{r.InstanceID, r.DeviceID, r.Width, r.Height, r.Palette, r.TimeZone, r.Locale}] = &db.Render{
			RenderedAt: r.RenderedAt, AttemptedAt: r.AttemptedAt, DurationMs: r.DurationMs, Error: r.Error, DataAt: r.DataAt, SettingsAt: r.SettingsAt, Failures: r.Failures,
		}
	}
	now := s.now()
	var list []job
	for _, c := range cands {
		if ok, by := due(c.target, renders[c.key()], c.wanted, c.woken, c.read, now); ok {
			list = append(list, job{target: c.target, seen: c.read, by: by})
		}
	}
	slices.SortStableFunc(list, func(a, b job) int {
		switch {
		case a.by.IsZero() && b.by.IsZero():
			return 0
		case a.by.IsZero():
			return 1
		case b.by.IsZero():
			return -1
		}
		return a.by.Compare(b.by)
	})
	return list, nil
}

// renderKey is the columns that identify a render row, as a target has them.
type renderKey struct {
	instance, device, width, height int64
	palette, zone, locale           string
}

// key returns the render row the target is stored in.
func (t target) key() renderKey {
	return renderKey{t.instance, t.device, int64(t.width), int64(t.height), t.palette, t.zone, t.locale}
}

// keepSet is the renders pruning keeps: the exact targets that are wanted,
// and the instances whose renders are all kept.
type keepSet struct {
	keys      map[renderKey]bool
	instances map[int64]bool
}

// has reports whether the render of k is kept.
func (k keepSet) has(key renderKey) bool {
	return k.keys[key] || k.instances[key.instance]
}

// pruneRenders deletes the renders that match no candidate: an instance
// that left every playlist, or a size, palette, time zone or locale no device
// has now, such as after a device's zone changed or the server's locale did. Their
// rows would otherwise keep their images referenced, and so stored,
// forever. A candidate is every item of every playlist a device is linked
// to, at the size the device last reported, so no device can show a render
// that is deleted here. The scheduler is the only writer of render rows
// besides a device's deletion, and it builds only after this, so a row
// cannot appear between the read and the delete.
func (s *Scheduler) pruneRenders(ctx context.Context, cands []*candidate) error {
	keep := keepSet{keys: make(map[renderKey]bool, len(cands)), instances: map[int64]bool{}}
	for _, c := range cands {
		keep.keys[c.key()] = true
		if c.broken != "" {
			// Its recipe could not be scanned, so it is not known which of
			// its renders are per device. It says nothing about which are
			// needed, so all of the instance's are kept for this pass.
			keep.instances[c.instance] = true
		}
	}
	return s.cfg.DB.InTx(ctx, func(q *db.Queries) error {
		rows, err := q.ListRenderKeys(ctx)
		if err != nil {
			return fmt.Errorf("list renders: %w", err)
		}
		for _, r := range rows {
			if keep.has(renderKey{r.InstanceID, r.DeviceID, r.Width, r.Height, r.Palette, r.TimeZone, r.Locale}) {
				continue
			}
			if err := q.DeleteRender(ctx, r.ID); err != nil {
				return fmt.Errorf("delete render: %w", err)
			}
			slog.Info("deleted a render no device needs", "instance", r.InstanceID, "device", r.DeviceID, "width", r.Width, "height", r.Height, "palette", r.Palette, "zone", r.TimeZone, "locale", r.Locale)
		}
		return nil
	})
}

// candidates lists what devices' playlists need rendered, each once, in a
// fixed order, with when each is wanted. A device wants the item it will
// show at its next wake, which is playlist.Advance's answer, without a
// press, for last_seen plus the refresh_rate it was sent, from the playlist
// playlist.Choose picks at that wake plus playlist.WakeSlack, read in the
// device's time zone. A device whose wake is inside its sleep window is shown
// no item, so it wants nothing. A target that is per device is wanted by that device
// alone, and a shared one by every device that will show it.
func (s *Scheduler) candidates(ctx context.Context) ([]*candidate, error) {
	serverLocale, err := settings.Locale(ctx, s.cfg.DB.Read)
	if err != nil {
		return nil, fmt.Errorf("read the locale setting: %w", err)
	}
	rows, err := s.cfg.DB.Read.ListRenderTargets(ctx)
	if err != nil {
		return nil, fmt.Errorf("list render targets: %w", err)
	}
	byTarget := make(map[target]*candidate, len(rows))
	for i := range rows {
		r := &rows[i]
		zone, locale := device.ZoneOf(r.TimeZone, s.cfg.Zone).String(), device.LocaleOf(r.Locale, serverLocale)
		t := s.newTarget(r.InstanceID, r.RecipeID, r.RecipeRevision, r.DeviceID, r.FwModel, profile.Override{Model: r.ProfileOverride, Palette: r.PaletteOverride}, r.Width, r.Height, r.Palette, zone, locale, r.RefreshInterval)
		c := byTarget[t]
		if c == nil {
			c = &candidate{target: t}
			byTarget[t] = c
		}
		c.woken = c.woken || r.Woken
		c.read = version{data: r.DataAt, settings: r.SettingsAt}
	}

	wakes, err := s.cfg.DB.Read.ListDeviceWakes(ctx)
	if err != nil {
		return nil, fmt.Errorf("list device wakes: %w", err)
	}
	shown, err := s.cfg.DB.Read.ListShownItems(ctx)
	if err != nil {
		return nil, fmt.Errorf("list shown items: %w", err)
	}
	rowsOf := map[int64][]db.ListShownItemsRow{}
	for i := range shown {
		rowsOf[shown[i].DeviceID] = append(rowsOf[shown[i].DeviceID], shown[i])
	}
	for i := range wakes {
		w := &wakes[i]
		lists, groups := playlist.Group(rowsOf[w.DeviceID], func(r *db.ListShownItemsRow) (int64, playlist.Schedule, playlist.Entry) {
			return r.PlaylistID, playlist.Schedule{
				Days: r.PlaylistDays, WindowStart: r.PlaylistWindowStart, WindowEnd: r.PlaylistWindowEnd, StartsAt: r.PlaylistStartsAt, EndsAt: r.PlaylistEndsAt,
			}, playlist.Entry{
				ID: r.ID, Duration: time.Duration(r.Duration) * time.Second, Hidden: r.Hidden != 0,
				Schedule: playlist.Schedule{Days: r.ItemDays, WindowStart: r.ItemWindowStart, WindowEnd: r.ItemWindowEnd, StartsAt: r.ItemStartsAt, EndsAt: r.ItemEndsAt},
			}
		})
		// The device chooses at its wake plus the slack a wake may come
		// early, as /api/display does, in its own time zone.
		zone := device.ZoneOf(w.TimeZone, s.cfg.Zone)
		next := time.Unix(w.LastSeen+w.LastRefreshRateSent, 0).In(zone)
		at := next.Add(playlist.WakeSlack)
		// A device asleep at its wake is shown nothing, so it wants nothing.
		c := playlist.Choose(lists, playlist.Awake(w.SleepStart, w.SleepEnd), at)
		if c < 0 {
			continue
		}
		var cur *playlist.Cursor
		if w.ItemID != nil && w.ItemStartedAt != nil {
			cur = &playlist.Cursor{ItemID: *w.ItemID, Started: time.Unix(*w.ItemStartedAt, 0)}
		}
		j, _ := playlist.Advance(lists[c].Items(at), cur, next, "")
		if j < 0 {
			continue
		}
		it := &groups[c][j]
		t := s.newTarget(it.InstanceID, it.RecipeID, it.RecipeRevision, w.DeviceID, w.FwModel, profile.Override{Model: w.ProfileOverride, Palette: w.PaletteOverride}, w.Width, w.Height, it.Palette, zone.String(), device.LocaleOf(w.Locale, serverLocale), it.RefreshInterval)
		if c := byTarget[t]; c != nil {
			c.wanted = append(c.wanted, next)
		}
	}

	return slices.SortedFunc(maps.Values(byTarget), func(a, b *candidate) int {
		return cmp.Or(cmp.Compare(a.instance, b.instance), cmp.Compare(a.device, b.device), cmp.Compare(a.width, b.width),
			cmp.Compare(a.height, b.height), strings.Compare(a.palette, b.palette),
			strings.Compare(a.zone, b.zone), strings.Compare(a.locale, b.locale))
	}), nil
}

// newTarget returns the target for an instance shown on a device of the
// given model, override and size, in the given time zone and locale, which
// are the ones in effect for the device. A target that cannot be built, because its recipe
// cannot be scanned, is returned with broken set, so that it is recorded as
// a failed render and does not stop the others.
func (s *Scheduler) newTarget(instance, recipeID, recipeRevision, deviceID int64, fwModel string, o profile.Override, width, height *int64, palette, zone, locale string, refreshSeconds int64) target {
	t := target{instance: instance, device: deviceID, palette: palette, zone: zone, locale: locale, refresh: time.Duration(refreshSeconds) * time.Second}
	p := profile.Resolve(fwModel, width, height, o)
	t.width, t.height, t.palette = p.Width, p.Height, p.RenderPalette(palette)
	per, err := s.readsDevice(recipeID, recipeRevision)
	if err != nil {
		// Whether the recipe is per device is unknown. Its devices find a
		// shared failure, which is the fallback for a device's own render.
		t.device = 0
		t.broken = err.Error()
		return t
	}
	if !per {
		t.device = 0
	}
	return t
}

// readsDevice reports whether revision of the recipe's files reads
// trmnl.device, scanning them the first time it is asked. A revision's files
// never change, and a replace makes a new revision, which any process may
// do, so the answer for a revision holds. The answer for the recipe's other
// revisions is forgotten.
func (s *Scheduler) readsDevice(recipeID, revision int64) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if per, ok := s.perDevice[recipeID]; ok && per.revision == revision {
		return per.reads, nil
	}
	per, err := recipe.UsesDevice(recipe.Dir(s.cfg.RecipesDir, recipeID, revision))
	if err != nil {
		return false, fmt.Errorf("recipe %d: %w", recipeID, err)
	}
	s.perDevice[recipeID] = scanned{revision: revision, reads: per}
	return per, nil
}

// defaultBuildTime is the build duration assumed for a target that has none
// recorded yet.
const defaultBuildTime = 30 * time.Second

// lead is how long before a wake a build of a target should start, so it is
// done when the device may wake. Run builds one target per scan and scans
// again at once after a build, so a target that becomes due waits for the
// build that is running, if any, and is then found by the next scan. When
// nothing is running it is found by the next check, at most checkEvery
// later. A running build is not known, so it is taken to last as long as
// this target's last one. The lead is that build, this target's own build,
// the early wake the cursor allows, and the check interval. A target
// behind more urgent ones also waits for them, but those are wanted at
// earlier wakes and have leads of their own.
func lead(r *db.Render) time.Duration {
	build := defaultBuildTime
	if r != nil && r.DurationMs > 0 {
		build = time.Duration(r.DurationMs) * time.Millisecond
	}
	return 2*build + playlist.WakeSlack + checkEvery
}

// due reports whether t should be built now, given its render row (nil if
// it has none), the times devices that will show it expect to wake next,
// whether any device showing it has ever woken, what its instance has now
// that a build reads, and the time. It is due when any of these holds:
//
//   - it has never been rendered;
//   - its settings are newer than what its last build read, by settings_at,
//     which an update of the instance's field values or secrets makes rise.
//     This holds after a failed build too, since the change is new input
//     and is usually the fix, and the failed build recorded the
//     settings_at it read, so it holds once for each change;
//   - its last build failed, and retryDelay has passed since it, which
//     grows with each failure in a row (while a failure is the latest
//     outcome, nothing else but the settings rule makes it due, so a
//     failing recipe is not built on every check). A try the renderer did
//     not take is not recorded, so it changes none of this;
//   - its data is newer than what its last build read, by updated_at, which
//     the push path makes rise with every push. A failed build is retried
//     after retryDelay by the rule above, so a push waits for that;
//   - it is wanted at a wake that its last good render would be a refresh
//     interval old at, and that wake is within lead;
//   - its last good render is two refresh intervals old, wanted or not,
//     which bounds the age of what a button press can reach and keeps
//     renders younger than the device API treats as out of date;
//   - no device showing it has woken, and a refresh interval has passed.
//
// by is the earliest wake that makes it due, which orders builds, and is
// the zero time when only the data, settings and last two rules apply.
func due(t target, r *db.Render, wanted []time.Time, woken bool, read version, now time.Time) (ok bool, by time.Time) {
	if r == nil {
		return true, earliest(wanted, func(time.Time) bool { return true })
	}
	if r.Error != "" {
		// A change to the settings is new input the failed build did not
		// see, and fixing a field is the usual reason for one, so it does
		// not wait for the backoff. The failed build recorded the
		// settings_at it read, so this holds once for each change.
		if read.settings > r.SettingsAt {
			return true, earliest(wanted, func(time.Time) bool { return true })
		}
		ok = !now.Before(time.Unix(r.AttemptedAt, 0).Add(retryDelay(r.Failures)))
		return ok, earliest(wanted, func(time.Time) bool { return ok })
	}
	if read.data > r.DataAt || read.settings > r.SettingsAt {
		return true, earliest(wanted, func(time.Time) bool { return true })
	}
	rendered := time.Unix(r.RenderedAt, 0)
	// A device that is late wakes now, not at the time it was due.
	if by = earliest(wanted, func(w time.Time) bool {
		at := w
		if now.After(at) {
			at = now
		}
		return at.Sub(rendered) >= t.refresh && !now.Before(w.Add(-lead(r)))
	}); !by.IsZero() {
		return true, by
	}
	age := now.Sub(rendered)
	return age >= 2*t.refresh || (!woken && age >= t.refresh), time.Time{}
}

// earliest returns the earliest of times for which keep is true, or the
// zero time if there is none.
func earliest(times []time.Time, keep func(time.Time) bool) time.Time {
	var best time.Time
	for _, w := range times {
		if keep(w) && (best.IsZero() || w.Before(best)) {
			best = w
		}
	}
	return best
}

// stored is an image a build put in the image store.
type stored struct {
	name string
	size int64
}

// render builds t and records the outcome. seen is the instance's version
// when the scan found t due. recorded is whether a success or a failure was
// written, so that err is the build's failure when recorded, and a failure
// to write otherwise.
func (s *Scheduler) render(ctx context.Context, t target, seen version) (recorded bool, err error) {
	start := s.now()
	// A server that stops during the build records nothing of it, so the
	// build would run again first at the next start. This row survives,
	// and RecoverAttempts makes it a counted failure then, so the build
	// waits out its backoff. It is deleted in the same transaction that
	// saves the outcome, so a crash between the two cannot count a saved
	// build again, and when the renderer did not take the build. It stays
	// when the server is stopping: systemd stops a unit with SIGTERM when
	// the kernel killed one of its processes for memory, as it may a build's
	// Firefox, and the server cannot tell that from a planned stop, so a
	// planned restart during a build costs that build one retry delay.
	key := attemptKey(t)
	if err := s.cfg.DB.Write.StartRenderAttempt(ctx, db.StartRenderAttemptParams{
		InstanceID: key.InstanceID, DeviceID: key.DeviceID, Width: key.Width, Height: key.Height,
		Palette: key.Palette, TimeZone: key.TimeZone, Locale: key.Locale,
		StartedAt: start.Unix(), DataAt: seen.data, SettingsAt: seen.settings,
	}); err != nil {
		return false, fmt.Errorf("record the build's start: %w", err)
	}
	bctx, cancel := context.WithTimeout(ctx, s.buildTimeout)
	defer cancel()
	images, log, read, err := s.build(bctx, t, seen)
	took := s.now().Sub(start)
	slog.Info("render", "instance", t.instance, "device", t.device, "width", t.width, "height", t.height, "palette", t.palette,
		"zone", t.zone, "locale", t.locale, "seconds", took.Seconds(), "err", err)
	if err != nil {
		if ctx.Err() != nil {
			return false, ctx.Err()
		}
		// A renderer that did not take the build says nothing about the
		// target, so nothing is recorded: the row keeps its last real
		// outcome, and Run waits before it tries again. It is logged.
		if errors.Is(err, render.ErrUnavailable) {
			if derr := s.cfg.DB.Write.EndRenderAttempt(ctx, key); derr != nil {
				return false, errors.Join(err, fmt.Errorf("clear the build's start: %w", derr))
			}
			return false, err
		}
		serr := s.cfg.DB.InTx(ctx, func(q *db.Queries) error {
			if err := q.SaveRenderFailure(ctx, db.SaveRenderFailureParams{
				InstanceID:  t.instance,
				DeviceID:    t.device,
				Width:       int64(t.width),
				Height:      int64(t.height),
				Palette:     t.palette,
				TimeZone:    t.zone,
				Locale:      t.locale,
				AttemptedAt: start.Unix(),
				DurationMs:  took.Milliseconds(),
				Error:       log.line(err),
				Log:         log.stored(),
				DataAt:      read.data,
				SettingsAt:  read.settings,
				Failures:    1,
			}); err != nil {
				return err
			}
			return q.EndRenderAttempt(ctx, key)
		})
		if serr != nil {
			return false, errors.Join(err, fmt.Errorf("save render failure: %w", serr))
		}
		return true, err
	}
	err = s.cfg.DB.InTx(ctx, func(q *db.Queries) error {
		id, err := q.SaveRenderSuccess(ctx, db.SaveRenderSuccessParams{
			InstanceID:  t.instance,
			DeviceID:    t.device,
			Width:       int64(t.width),
			Height:      int64(t.height),
			Palette:     t.palette,
			TimeZone:    t.zone,
			Locale:      t.locale,
			RenderedAt:  s.now().Unix(),
			AttemptedAt: start.Unix(),
			DurationMs:  took.Milliseconds(),
			Log:         log.stored(),
			DataAt:      read.data,
			SettingsAt:  read.settings,
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
		if err := q.EndRenderAttempt(ctx, key); err != nil {
			return fmt.Errorf("clear the build's start: %w", err)
		}
		return nil
	})
	return err == nil, err
}

// attemptKey is t's key in render_attempt.
func attemptKey(t target) db.EndRenderAttemptParams {
	return db.EndRenderAttemptParams{
		InstanceID: t.instance, DeviceID: t.device, Width: int64(t.width), Height: int64(t.height),
		Palette: t.palette, TimeZone: t.zone, Locale: t.locale,
	}
}

// stoppedDuringBuild is the error a build gets that was running when the
// server stopped, which RecoverAttempts records at the next start.
const stoppedDuringBuild = "the server stopped during this build, which the build may have caused"

// RecoverAttempts records each build that started and saved no outcome,
// which means the server stopped during it, as a counted failure of its
// target, so it waits out the backoff, and the instance's page says why.
// Run calls it before its first scan. It does all of them in one
// transaction. A leftover whose instance or device was deleted went with
// it, by the instance's foreign key and the device's trigger, so every
// leftover still has its target's instance. It returns how many it
// recorded.
func (s *Scheduler) RecoverAttempts(ctx context.Context) (int, error) {
	n := 0
	err := s.cfg.DB.InTx(ctx, func(q *db.Queries) error {
		n = 0
		rows, err := q.ListRenderAttempts(ctx)
		if err != nil {
			return fmt.Errorf("list render attempts: %w", err)
		}
		for _, a := range rows {
			if err := q.SaveRenderFailure(ctx, db.SaveRenderFailureParams{
				InstanceID: a.InstanceID, DeviceID: a.DeviceID, Width: a.Width, Height: a.Height,
				Palette: a.Palette, TimeZone: a.TimeZone, Locale: a.Locale,
				AttemptedAt: a.StartedAt, Error: stoppedDuringBuild,
				DataAt: a.DataAt, SettingsAt: a.SettingsAt, Failures: 1,
			}); err != nil {
				return fmt.Errorf("record a build the server stopped during: %w", err)
			}
			if err := q.EndRenderAttempt(ctx, db.EndRenderAttemptParams{
				InstanceID: a.InstanceID, DeviceID: a.DeviceID, Width: a.Width, Height: a.Height,
				Palette: a.Palette, TimeZone: a.TimeZone, Locale: a.Locale,
			}); err != nil {
				return fmt.Errorf("clear a build the server stopped during: %w", err)
			}
			n++
		}
		return nil
	})
	return n, err
}

// build sends t's project to the renderer and stores the images it returns.
// It also returns what it read of the instance, which the render records. A
// build that fails before it reads the instance, or its data, returns what
// the scan saw, seen, for what it did not read. A failure then records the
// version it was due for, so a change makes a failing target due once, and
// it otherwise waits for its backoff, wherever the build failed. Recording 0
// instead would make a target whose settings ever changed due on every scan.
// log is what the renderer printed, with the field values the build read,
// which the stored log and the line of a failure hide.
func (s *Scheduler) build(ctx context.Context, t target, seen version) (images map[string]stored, log buildLog, read version, err error) {
	read = seen
	if t.broken != "" {
		return nil, log, read, errors.New(t.broken)
	}
	// The instance and its recipe are read in one snapshot, so the files
	// are the revision its settings_at goes with. A replace raises both in
	// one commit, and keeps the revision before for a build like this one
	// that read it.
	var inst db.PluginInstance
	var rec db.Recipe
	err = s.cfg.DB.ReadTx(ctx, func(q *db.Queries) error {
		var err error
		if inst, err = q.GetInstance(ctx, t.instance); err != nil {
			return fmt.Errorf("get instance: %w", err)
		}
		if rec, err = q.GetRecipe(ctx, inst.RecipeID); err != nil {
			return fmt.Errorf("get recipe: %w", err)
		}
		return nil
	})
	if err != nil {
		return nil, log, read, err
	}
	read.settings = inst.SettingsAt
	dir := recipe.FilesDir(s.cfg.RecipesDir, &rec)
	fields, secretValues, err := recipe.InstanceFields(&inst, s.cfg.Secrets)
	if err != nil {
		return nil, log, read, err
	}
	// The values this build read are the ones its log and line hide, not the
	// ones stored by the time it fails, which a save meanwhile may have
	// changed.
	log.secrets, log.hidden = secretValues, hiddenValues(fields)
	// A value left from a field the recipe no longer declares stays stored,
	// so it can be cleared or a replace back can use it, but a build gets
	// only the declared ones.
	rs, err := recipe.ReadSettings(dir)
	if err != nil {
		return nil, log, read, fmt.Errorf("recipe %d: %w", rec.ID, err)
	}
	declared := make(map[string]bool, len(rs.CustomFields))
	for i := range rs.CustomFields {
		declared[rs.CustomFields[i].Keyname] = true
	}
	maps.DeleteFunc(fields, func(k string, _ recipe.Value) bool { return !declared[k] })
	// What trmnlp would only warn about, and then draw no data for, fails
	// the build here, so it shows.
	if why := rs.BuildProblem(fields); why != "" {
		return nil, log, read, errors.New(why)
	}
	proj := recipe.NewProject(dir, fields, t.zone, t.locale, s.cfg.AssetHosts)
	data, err := s.cfg.DB.Read.GetInstanceData(ctx, t.instance)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		read.data = 0
	case err != nil:
		return nil, log, read, fmt.Errorf("get instance data: %w", err)
	default:
		read.data = data.UpdatedAt
		if proj, err = proj.WithData(data.Data); err != nil {
			return nil, log, read, fmt.Errorf("instance %d data: %w", t.instance, err)
		}
	}
	if t.device != 0 {
		row, err := s.cfg.DB.Read.GetRenderDevice(ctx, t.device)
		if err != nil {
			return nil, log, read, fmt.Errorf("get device: %w", err)
		}
		proj = proj.WithDevice(recipe.NewDevice(&row))
	}
	images = make(map[string]stored, len(render.Layouts))
	log.raw, err = s.cfg.Renderer.Build(ctx,
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
		return nil, log, read, err
	}
	return images, log, read, nil
}

// buildLog is what a build's renderer printed, as it printed it, and the
// field values the build read. What trmnlp prints is whatever a transform
// printed or a failed fetch quoted, which can be a field's value, secret or
// not. The raw log is never stored, logged or shown: it is read only through
// stored and line, which hide the values. A failure's line is made from the
// raw log, so a URL in it is cut before anything is hidden and a secret that
// holds a URL's scheme cannot stop the cut.
type buildLog struct {
	raw string
	// secrets are the build's secret field values, of any length.
	secrets []string
	// hidden are the build's field values a failure's line hides, from
	// hiddenValues.
	hidden []string
}

// stored returns the log as it is stored and shown to an admin: with every
// secret value hidden.
func (l buildLog) stored() string {
	return redact(l.raw, l.secrets)
}

// line returns the line a device shows for err, the build's failure, with
// every field value the build read hidden, and every secret one of any
// length, as the stored log hides it.
func (l buildLog) line(err error) string {
	return describe(err, l.raw, slices.Concat(l.hidden, l.secrets))
}

// redacted replaces a secret value in a build log.
const redacted = "[redacted]"

// redact returns log with every exact occurrence of each value replaced,
// and every occurrence of its URL-encoded forms: url.QueryEscape, which
// writes a space as +, and url.PathEscape, which writes it as %20. trmnlp
// prints the whole polling URL when a poll fails with a 3xx or 4xx answer
// or a malformed body, and a URL holds a value as the recipe wrote it,
// which is encoded when it used the url_encode filter. The longest forms go
// first, so a form that holds another is hidden whole. An empty value is
// skipped, since it matches everywhere.
func redact(log string, values []string) string {
	forms := redactForms(values)
	if len(forms) == 0 {
		return log
	}
	pairs := make([]string, 0, 2*len(forms))
	for _, f := range forms {
		pairs = append(pairs, f, redacted)
	}
	return strings.NewReplacer(pairs...).Replace(log)
}

// redactForms returns each value and its URL-encoded forms, as redact
// hides them, longest first.
func redactForms(values []string) []string {
	forms := map[string]bool{}
	for _, v := range values {
		if v != "" {
			forms[v], forms[url.QueryEscape(v)], forms[url.PathEscape(v)] = true, true, true
		}
	}
	return slices.SortedFunc(maps.Keys(forms), func(a, b string) int { return cmp.Or(cmp.Compare(len(b), len(a)), strings.Compare(a, b)) })
}

// urlScheme matches the start of a URL in any case: its scheme and ://.
var urlScheme = regexp.MustCompile(`(?i)\b[a-z][a-z0-9+.-]*://`)

// hostPrefix matches a host, an IP literal or a name, and an optional port,
// at the start of a string.
var hostPrefix = regexp.MustCompile(`^(?:\[[0-9A-Fa-f:.]+\]|[A-Za-z0-9.-]+)(?::\d+)?`)

// urlEnds are what end a URL in trmnlp's lines, which print it as it was
// interpolated, spaces and all: its separator before why, and the phrase
// after a refused one. The end of the line ends one too.
var urlEnds = []string{" — ", ": the url resolves"}

// span is a part of a line that describe shows, line[start:end], in lower
// case when lower is set.
type span struct {
	start, end int
	lower      bool
}

// urlCut returns the parts of line that are kept when each URL in it is cut
// to its scheme and host. trmnlp prints the whole URL of a fetch that
// failed, as interpolated, and a key can be in its path, its user info or
// its query, from a field that may not be secret, after a value with a space
// in it. So a URL runs from its scheme to the end of the line or to where
// trmnlp ends one, and its host is what follows the last @ in that: the name
// or IP literal there and its port, and nothing else. The scheme is kept in
// lower case. The rest goes. Showing too little is safe. The line goes on a
// device's screen and into the render's error, and the build log, which
// only an admin reads, keeps the whole line.
func urlCut(line string) []span {
	var kept []span
	at := 0
	for {
		loc := urlScheme.FindStringIndex(line[at:])
		if loc == nil {
			return append(kept, span{start: at, end: len(line)})
		}
		scheme, rest := at+loc[0], at+loc[1]
		kept = append(kept, span{start: at, end: scheme}, span{start: scheme, end: rest, lower: true})
		end := len(line)
		for _, e := range urlEnds {
			if i := strings.Index(line[rest:], e); i >= 0 && rest+i < end {
				end = rest + i
			}
		}
		host := rest
		if i := strings.LastIndexByte(line[rest:end], '@'); i >= 0 {
			host = rest + i + 1
		}
		// Only what can be a host and port stays, so an @ in the query
		// shows a wrong host at worst, never the query after it.
		kept = append(kept, span{start: host, end: host + len(hostPrefix.FindString(line[host:end]))})
		at = end
	}
}

// hiddenBytes reports, for each byte of line, whether it is in an
// occurrence of one of values in a form redact hides. Occurrences may
// overlap, and each one counts: "aba" in "ababa" hides all five bytes. One
// suffix array of the line finds every start of each form, and each byte is
// marked once per form, so the cost stays near linear in the line however
// often a form repeats in it.
func hiddenBytes(line string, values []string) []bool {
	hidden := make([]bool, len(line))
	forms := redactForms(values)
	if len(forms) == 0 {
		return hidden
	}
	index := suffixarray.New([]byte(line))
	for _, f := range forms {
		starts := index.Lookup([]byte(f), -1)
		slices.Sort(starts)
		marked := 0
		for _, at := range starts {
			for j := max(at, marked); j < at+len(f); j++ {
				hidden[j] = true
			}
			marked = max(marked, at+len(f))
		}
	}
	return hidden
}

// minHiddenLen is the fewest characters a field value has to have to be
// hidden on a device line. A shorter one, such as a unit, would hide every
// place its letters appear, and is not much of a secret.
const minHiddenLen = 4

// hiddenValues returns the field values a device line hides, of at least
// minHiddenLen characters, plain ones too, each item of a list on its own.
func hiddenValues(fields map[string]recipe.Value) []string {
	var out []string
	for _, v := range fields {
		for _, item := range append([]string{v.One}, v.Many...) {
			if utf8.RuneCountInString(item) >= minHiddenLen {
				out = append(out, item)
			}
		}
	}
	return out
}

// describe is the one line a device shows for a failed build: trmnlp's last
// line of output when trmnlp failed, since that is where it says why, and
// its last warning when a data fetch failed, since trmnlp then finishes and
// says why only in a warning. No character of hidden, the field values the
// build read, reaches the screen, whatever else is cut. Which bytes came
// from a value is found once, on the whole raw log, as redact finds them,
// before anything is picked, trimmed or cut. Everything describe shows is
// then a byte range of that log, read against that one mask, or fixed text:
// the "trmnlp: " or "data fetch failed: " prefix, or a scheme in lower case.
// No step builds a new string that is searched again. So a value with space
// at either end, one that runs across lines, and one that runs past where a
// URL is cut are all hidden in the part that shows. why picks the line as a
// range of the log, without the space at either end. Then, in a line from
// trmnlp, every URL is cut to its scheme and host, as ranges in that line.
// Of what is left, each run of bytes that came from a value shows as one
// [redacted]. So a host or base URL from a field is hidden whether the cut
// keeps all of it or only its host, and a URL the cut cannot see, with no
// scheme or a typo in it, keeps no value either.
// The server's own errors keep their URLs: the only URL in one is the
// renderer's, and the reason after it is what the admin needs.
func describe(err error, log string, hidden []string) string {
	prefix, src, start, end, fromTrmnlp := why(err, log)
	line, isHidden := src[start:end], hiddenBytes(src, hidden)[start:end]
	kept := []span{{start: 0, end: len(line)}}
	if fromTrmnlp {
		kept = urlCut(line)
	}
	var b strings.Builder
	b.WriteString(prefix)
	inHidden := false
	for _, k := range kept {
		for i := k.start; i < k.end; {
			if isHidden[i] {
				if !inHidden {
					b.WriteString(redacted)
				}
				inHidden = true
				i++
				continue
			}
			inHidden = false
			j := i
			for j < k.end && !isHidden[j] {
				j++
			}
			if k.lower {
				b.WriteString(strings.ToLower(line[i:j]))
			} else {
				b.WriteString(line[i:j])
			}
			i = j
		}
	}
	return b.String()
}

// why is describe's line before anything in it is hidden: prefix, then
// src[start:end], and whether it is trmnlp's output. src is the whole log
// when the line is from it, so describe finds the values in all of it, and
// the error's text otherwise.
func why(err error, log string) (prefix, src string, start, end int, fromTrmnlp bool) {
	fetch, build := errors.Is(err, render.ErrFetchFailed), errors.Is(err, render.ErrBuildFailed)
	if fetch || build {
		// Each line, from the last, without the space at either end.
		for lineEnd := len(log); lineEnd >= 0; {
			lineStart := strings.LastIndexByte(log[:lineEnd], '\n') + 1
			s, e := trimmed(log, lineStart, lineEnd)
			if build && s < e {
				return "trmnlp: ", log, s, e, true
			}
			if fetch {
				if i := strings.Index(log[s:e], "warning: "); i >= 0 {
					if ws, we := trimmed(log, s+i+len("warning: "), e); ws < we {
						return "data fetch failed: ", log, ws, we, true
					}
				}
			}
			lineEnd = lineStart - 1
		}
	}
	msg := err.Error()
	return "", msg, 0, len(msg), false
}

// trimmed returns the range of log[start:end] without the space at either
// end, as strings.TrimSpace would leave it.
func trimmed(log string, start, end int) (s, e int) {
	t := strings.TrimLeftFunc(log[start:end], unicode.IsSpace)
	s = end - len(t)
	return s, s + len(strings.TrimRightFunc(t, unicode.IsSpace))
}
