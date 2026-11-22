// Command trmnl-byos is a self-hosted server for TRMNL e-ink displays.
//
//	trmnl-byos serve      run the server
//	trmnl-byos proxy      run the renderer's proxy apart from the server
//	trmnl-byos join       let new devices register for a while
//	trmnl-byos device     list, delete and block devices
//	trmnl-byos calibrate  show a device the gray calibration screen
//	trmnl-byos recipe     install, replace and list recipes
//	trmnl-byos instance   create, list, change and remove plugin instances
//	trmnl-byos playlist   edit what devices show
//	trmnl-byos token      create and remove API tokens
//	trmnl-byos password   set the admin password for the web UI
//	trmnl-byos setting    list and change settings, such as the refresh interval
//	trmnl-byos firmware   add firmware builds and roll them out
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/die-net/trmnl-byos-go/internal/adminapi"
	"github.com/die-net/trmnl-byos-go/internal/adminui"
	"github.com/die-net/trmnl-byos-go/internal/cache"
	"github.com/die-net/trmnl-byos-go/internal/db"
	"github.com/die-net/trmnl-byos-go/internal/device"
	"github.com/die-net/trmnl-byos-go/internal/deviceapi"
	"github.com/die-net/trmnl-byos-go/internal/errkind"
	"github.com/die-net/trmnl-byos-go/internal/guard"
	"github.com/die-net/trmnl-byos-go/internal/imagestore"
	"github.com/die-net/trmnl-byos-go/internal/recipe"
	"github.com/die-net/trmnl-byos-go/internal/render"
	"github.com/die-net/trmnl-byos-go/internal/renderproxy"
	"github.com/die-net/trmnl-byos-go/internal/scheduler"
	"github.com/die-net/trmnl-byos-go/internal/secrets"
)

const usage = `usage: trmnl-byos <command> [flags]

commands:
  serve      run the server
  proxy      run the renderer's proxy apart from the server
  join       let new devices register for a while
  device     list, delete and block devices: list, delete, block, unblock, blocked
  calibrate  show a device the gray calibration screen
  recipe     install, replace and list recipes: add, list, replace
  instance   create, list, change and remove plugin instances: add, list, set, remove
  playlist   edit what devices show: add, remove, list, move, set, schedule, create, rename, delete, link, unlink, order
  token      create and remove API tokens: add, list, remove
  password   set the admin password for the web UI
  setting    list and change settings: list, set, reset
  firmware   add firmware builds and roll them out: add, list, rollout, cancel, device

Run "trmnl-byos <command> -h" for a command's flags.
`

func main() {
	os.Exit(run())
}

// run runs the command named in os.Args and returns the exit code.
func run() int {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		return 2
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var err error
	switch cmd, args := os.Args[1], os.Args[2:]; cmd {
	case "serve":
		err = serve(ctx, args)
	case "proxy":
		err = proxy(ctx, args)
	case "join":
		err = join(ctx, args, os.Stdout)
	case "device":
		err = devices(ctx, args, os.Stdout)
	case "calibrate":
		err = calibrate(ctx, args, os.Stdout)
	case "recipe":
		err = recipes(ctx, args, os.Stdout)
	case "instance":
		err = instances(ctx, args, os.Stdout)
	case "playlist":
		err = playlists(ctx, args, os.Stdout)
	case "token":
		err = tokens(ctx, args, os.Stdout)
	case "password":
		err = password(ctx, args, os.Stdin, os.Stdout)
	case "setting":
		err = setting(ctx, args, os.Stdout)
	case "firmware":
		err = firmwares(ctx, args, os.Stdout)
	default:
		fmt.Fprint(os.Stderr, usage)
		return 2
	}
	return exitCode(ctx, err)
}

// exitCode returns the exit code for a command that returned err, and logs
// err. A command that a signal stopped did not finish, so its code is 1, but
// that is not a failure to log as an error.
func exitCode(ctx context.Context, err error) int {
	switch {
	case err == nil:
		return 0
	case errkind.Ended(ctx, err):
		slog.InfoContext(ctx, "command stopped", "err", err)
	default:
		slog.ErrorContext(ctx, "command failed", "err", err)
	}
	return 1
}

// serverZone returns the server's time zone: TZ, the standard way to give a
// process its zone, or UTC if TZ is unset. Schedules are read in it, times
// are shown in it, and recipes render in it. An unknown name is an error,
// so a typo does not quietly become UTC.
func serverZone() (*time.Location, error) {
	loc, err := time.LoadLocation(os.Getenv("TZ"))
	if err != nil {
		return nil, fmt.Errorf("TZ: %w", err)
	}
	return loc, nil
}

// dataFlag adds the -data flag every command takes.
func dataFlag(fs *flag.FlagSet) *string {
	return fs.String("data", "data", "directory holding the database and images")
}

// oneArgCommand runs a command that takes the -data flag and one argument,
// named argName in its usage. It opens the database, calls act with the
// argument, and prints done, a format with one verb for the argument.
func oneArgCommand(ctx context.Context, args []string, out io.Writer, name, argName string,
	act func(ctx context.Context, client *db.Client, arg string) error, done string,
) error {
	fs := flag.NewFlagSet(name, flag.ExitOnError)
	data := dataFlag(fs)
	_ = fs.Parse(args) // ExitOnError exits on a parse error.
	if fs.NArg() != 1 {
		return fmt.Errorf("usage: trmnl-byos %s [-data dir] <%s>", name, argName)
	}
	client, err := openDB(ctx, *data)
	if err != nil {
		return err
	}
	defer client.Close()
	if err := act(ctx, client, fs.Arg(0)); err != nil {
		return err
	}
	_, err = fmt.Fprintf(out, done, fs.Arg(0))
	return err
}

// dbPath is the database's path in the data directory.
func dbPath(data string) string {
	return filepath.Join(data, "trmnl.db")
}

// openDB opens the database every command but serve works on. It never
// creates one: a command run from the wrong directory, or without the
// server's -data, would otherwise change an empty database the server never
// reads.
func openDB(ctx context.Context, data string) (*db.Client, error) {
	client, err := db.OpenExisting(ctx, dbPath(data))
	if errors.Is(err, db.ErrNoDatabase) {
		return nil, fmt.Errorf("%w: pass the -data directory the server uses", err)
	}
	return client, err
}

// serve runs the server until ctx is done, which is a normal shutdown and
// returns nil, whichever step it was in. A step that fails with the canceled
// context while ctx is done failed because of the shutdown, so that error is
// not returned: a service stop during startup would otherwise exit with an
// error. Any other error is returned.
func serve(ctx context.Context, args []string) error {
	if err := runServer(ctx, args); !errkind.Ended(ctx, err) {
		return err
	}
	return nil
}

// runServer is serve without the shutdown handling.
func runServer(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	data := dataFlag(fs)
	listen := fs.String("listen", ":8080", "address to listen on")
	publicURL := fs.String("public-url", "", "base URL devices reach this server at, such as http://192.168.1.10:8080 (default: from each request)")
	renderURL := fs.String("render-url", "", "URL of the renderer, such as http://renderer:8081 (default: recipes are not rendered)")
	proxyListen := fs.String("proxy-listen", "", "address the renderer's proxy listens on in this process, such as :3128; never publish it (default: no proxy here, run trmnl-byos proxy)")
	cacheDir := fs.String("cache", "cache", "directory holding the renderer's asset cache, with -proxy-listen")
	debug := fs.Bool("debug", false, "log at debug level, including device request headers")
	_ = fs.Parse(args) // ExitOnError exits on a parse error.

	if *debug {
		slog.SetLogLoggerLevel(slog.LevelDebug)
	}
	key, err := secrets.FromEnv()
	if err != nil {
		return err
	}
	zone, err := serverZone()
	if err != nil {
		return err
	}

	client, err := db.Open(ctx, dbPath(*data))
	if err != nil {
		return err
	}
	defer client.Close()
	images, err := imagestore.New(filepath.Join(*data, "images"))
	if err != nil {
		return err
	}

	// Before anything that installs a recipe or starts the scheduler: clear
	// what a crash during an install or an upload left behind.
	if err := recipe.Sweep(ctx, client, recipesDir(*data), firmwareDir(*data), time.Now()); err != nil {
		return fmt.Errorf("sweep leftovers: %w", err)
	}

	// A change that may make a render due wakes the scheduler, so what it
	// makes due is built now, whichever front end made it. The CLI is
	// another process, and its changes wait for the scheduler's next check.
	wake := scheduler.NewWaker()
	client.OnChange = wake.Wake
	mux := http.NewServeMux()
	deviceapi.New(client, images, deviceapi.Config{PublicURL: *publicURL, FirmwareDir: firmwareDir(*data), Zone: zone}).Register(mux)
	adminapi.New(client, adminapi.Config{Key: key, RecipesDir: recipesDir(*data), FirmwareDir: firmwareDir(*data), Zone: zone}).Register(mux)
	adminui.New(client, adminui.Config{PublicURL: *publicURL, Zone: zone, RecipesDir: recipesDir(*data), FirmwareDir: firmwareDir(*data), Secrets: key}).Register(mux)
	srv := &http.Server{
		Addr:              *listen,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       2 * time.Minute,
	}

	// Everything that runs for the life of the process is in one group, so
	// that when any part fails, such as a server that cannot listen, the
	// rest stop. The database is closed by the deferred Close above, which
	// runs only after g.Wait has returned, so no loop is left writing to it.
	g, ctx := errgroup.WithContext(ctx)
	// The proxy fails before it starts anything, so returning here leaves
	// nothing running.
	if *proxyListen != "" {
		ln, err := listenOn(ctx, *proxyListen)
		if err != nil {
			return err
		}
		if err := startProxy(ctx, g, ln, *cacheDir); err != nil {
			return err
		}
	}
	switch {
	case *renderURL == "":
		slog.Warn("no -render-url: playlists that show recipes will wait for renders")
	case *proxyListen == "":
		// Not a fallback: builds that fetch data or assets fail until a
		// proxy the renderer can reach is running.
		slog.Info("no -proxy-listen: the renderer's builds reach the internet only through a proxy run apart, such as trmnl-byos proxy")
	}
	if *renderURL != "" {
		startScheduler(ctx, g, client, images, key, zone, *data, *renderURL, wake)
	}
	g.Go(func() error { return listenAndServe(ctx, srv, shutdownGrace) })
	g.Go(func() error {
		expireLoop(ctx, client, images)
		return nil
	})
	slog.Info("serving", "listen", *listen, "data", *data, "render_url", *renderURL, "proxy_listen", *proxyListen)
	return g.Wait()
}

// shutdownGrace is how long a server waits for requests in flight when the
// process is stopping.
const shutdownGrace = 10 * time.Second

// listenOn listens on addr, a TCP address such as :8080. It is the one place
// a listen address is read, so another kind, such as a Unix socket, is added
// here for every listener at once. An empty addr is refused: TCP would take
// it as a random port on every interface, which nothing could be pointed at.
func listenOn(ctx context.Context, addr string) (net.Listener, error) {
	if addr == "" {
		return nil, errors.New("listen: no address")
	}
	ln, err := (&net.ListenConfig{}).Listen(ctx, "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("listen %s: %w", addr, err)
	}
	return ln, nil
}

// listenAndServe runs srv on its Addr until ctx is done, as serveOn does. It
// returns at once with an error if srv cannot listen, so a caller that
// cancels ctx on error stops the rest.
func listenAndServe(ctx context.Context, srv *http.Server, grace time.Duration) error {
	ln, err := listenOn(ctx, srv.Addr)
	if err != nil {
		return err
	}
	return serveOn(ctx, srv, ln, grace)
}

// serveOn runs srv on ln until ctx is done, then shuts it down, waiting up
// to grace for requests in flight. It returns at once with an error if
// serving fails.
func serveOn(ctx context.Context, srv *http.Server, ln net.Listener, grace time.Duration) error {
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()
	select {
	case err := <-errc:
		return fmt.Errorf("serve %s: %w", ln.Addr(), err)
	case <-ctx.Done():
	}
	shutdown, cancel := context.WithTimeout(context.Background(), grace)
	defer cancel()
	if err := srv.Shutdown(shutdown); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("shutdown %s: %w", ln.Addr(), err)
	}
	return nil
}

// Asset cache limits: entries unused for a month go, and the oldest go
// first past the size limit.
const (
	cacheSweepEvery = time.Hour
	cacheMaxAge     = 30 * 24 * time.Hour
	cacheMaxBytes   = 2 << 30
)

// startScheduler starts the scheduler in g. It renders through the
// renderer at renderURL, and wake makes it scan at once.
func startScheduler(ctx context.Context, g *errgroup.Group, client *db.Client, images *imagestore.Store, key *secrets.Key,
	zone *time.Location, data, renderURL string, wake scheduler.Waker,
) {
	sched := scheduler.New(scheduler.Config{
		DB:         client,
		Images:     images,
		Renderer:   &render.Client{URL: renderURL, HTTP: &http.Client{}},
		RecipesDir: recipesDir(data),
		Secrets:    key,
		Zone:       zone,
		AssetHosts: renderproxy.AssetHosts,
		Wake:       wake,
	})
	g.Go(func() error {
		sched.Run(ctx)
		return nil
	})
}

// startProxy starts, in g, the forward proxy the renderer's builds reach the
// internet through, serving on ln, and the janitor of its asset cache in
// cacheDir. If it fails, it has started nothing and closed ln.
func startProxy(ctx context.Context, g *errgroup.Group, ln net.Listener, cacheDir string) error {
	assets, err := cache.New(cacheDir)
	if err != nil {
		_ = ln.Close()
		return err
	}
	g.Go(func() error {
		assets.Janitor(ctx, cacheSweepEvery, cacheMaxAge, cacheMaxBytes)
		return nil
	})
	handler := renderproxy.New(renderproxy.Config{
		Dialer:      guard.NewDialer(10 * time.Second),
		Cache:       assets,
		CachedHosts: renderproxy.AssetHosts,
	})
	// No read or write timeout: a CONNECT tunnel lasts as long as the build
	// uses it, and the renderer kills a build that runs too long.
	srv := &http.Server{Handler: handler, ReadHeaderTimeout: 10 * time.Second}
	g.Go(func() error { return serveOn(ctx, srv, ln, shutdownGrace) })
	return nil
}

const (
	// imageGrace is how long an image no row names is kept after it was
	// last stored.
	imageGrace = 24 * time.Hour
	// expireEvery is how often unused images are deleted.
	expireEvery = time.Hour
)

// expireLoop deletes unused images at startup and then every expireEvery,
// until ctx is done.
func expireLoop(ctx context.Context, client *db.Client, images *imagestore.Store) {
	tick := time.NewTicker(expireEvery)
	defer tick.Stop()
	for {
		n, err := expireImages(ctx, client, images, time.Now())
		if errkind.Ended(ctx, err) {
			return
		} else if err != nil {
			slog.ErrorContext(ctx, "expire images", "err", err)
		} else if n > 0 {
			slog.Info("expired images", "deleted", n)
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

// expireImages deletes images that no row names and that were last stored
// more than imageGrace before now. The rows are read before the files are
// listed. That is safe because whatever hands out an image name stores the
// image first, or refreshes it when the name comes from a row, so an image
// named after the read is too new to delete.
func expireImages(ctx context.Context, client *db.Client, images *imagestore.Store, now time.Time) (int, error) {
	names, err := client.Read.ListReferencedImages(ctx)
	if err != nil {
		return 0, fmt.Errorf("list referenced images: %w", err)
	}
	keep := make(map[string]bool, len(names))
	for _, n := range names {
		keep[n] = true
	}
	return images.Expire(keep, now.Add(-imageGrace))
}

func join(ctx context.Context, args []string, out io.Writer) error {
	fs := flag.NewFlagSet("join", flag.ExitOnError)
	data := dataFlag(fs)
	window := fs.Duration("for", 10*time.Minute, "how long the join window stays open; 0 closes it")
	_ = fs.Parse(args) // ExitOnError exits on a parse error.

	client, err := openDB(ctx, *data)
	if err != nil {
		return err
	}
	defer client.Close()
	until := time.Now().Add(*window)
	if err := device.SetJoinWindow(ctx, client, until); err != nil {
		return err
	}
	if *window <= 0 {
		_, err = fmt.Fprintln(out, "Join window closed.")
	} else {
		_, err = fmt.Fprintf(out, "New devices can register until %s.\n", until.Format(time.Kitchen))
	}
	return err
}

func calibrate(ctx context.Context, args []string, out io.Writer) error {
	fs := flag.NewFlagSet("calibrate", flag.ExitOnError)
	data := dataFlag(fs)
	window := fs.Duration("for", device.DefaultCalibration, "how long the device shows the calibration screen")
	dot := fs.Int("dot", 1, "dither dot size in pixels: 1 or 2")
	_ = fs.Parse(args) // ExitOnError exits on a parse error.
	if fs.NArg() != 1 {
		return errors.New("usage: trmnl-byos calibrate [-data dir] [-for duration] [-dot 1|2] <friendly ID or MAC>")
	}

	client, err := openDB(ctx, *data)
	if err != nil {
		return err
	}
	defer client.Close()
	dev, until, err := device.Calibrate(ctx, client, fs.Arg(0), *dot, *window, time.Now())
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(out, "%s shows the calibration screen from its next wake until %s. Press its button to wake it now.\n",
		dev.FriendlyID, until.Format(time.Kitchen))
	return err
}
