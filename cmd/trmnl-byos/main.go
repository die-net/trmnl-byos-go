// Command trmnl-byos is a self-hosted server for TRMNL e-ink displays.
//
//	trmnl-byos serve      run the server
//	trmnl-byos join       let new devices register for a while
//	trmnl-byos devices    list registered devices
//	trmnl-byos calibrate  show a device the gray calibration screen
//	trmnl-byos recipe     install and list recipes
//	trmnl-byos instance   create and list plugin instances
//	trmnl-byos playlist   edit what devices show
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/die-net/trmnl-byos-go/internal/cache"
	"github.com/die-net/trmnl-byos-go/internal/db"
	"github.com/die-net/trmnl-byos-go/internal/deviceapi"
	"github.com/die-net/trmnl-byos-go/internal/guard"
	"github.com/die-net/trmnl-byos-go/internal/imagestore"
	"github.com/die-net/trmnl-byos-go/internal/render"
	"github.com/die-net/trmnl-byos-go/internal/renderproxy"
	"github.com/die-net/trmnl-byos-go/internal/scheduler"
	"github.com/die-net/trmnl-byos-go/internal/secrets"
)

const usage = `usage: trmnl-byos <command> [flags]

commands:
  serve      run the server
  join       let new devices register for a while
  devices    list registered devices
  calibrate  show a device the gray calibration screen
  recipe     install and list recipes: add, list
  instance   create and list plugin instances: add, list
  playlist   edit what devices show: add, remove, list

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
	case "join":
		err = join(ctx, args, os.Stdout)
	case "devices":
		err = devices(ctx, args, os.Stdout)
	case "calibrate":
		err = calibrate(ctx, args, os.Stdout)
	case "recipe":
		err = recipes(ctx, args, os.Stdout)
	case "instance":
		err = instances(ctx, args, os.Stdout)
	case "playlist":
		err = playlists(ctx, args, os.Stdout)
	default:
		fmt.Fprint(os.Stderr, usage)
		return 2
	}
	if err != nil {
		slog.Error("command failed", "err", err)
		return 1
	}
	return 0
}

// dataFlag adds the -data flag every command takes.
func dataFlag(fs *flag.FlagSet) *string {
	return fs.String("data", "data", "directory holding the database and images")
}

func openDB(ctx context.Context, data string) (*db.Client, error) {
	return db.Open(ctx, filepath.Join(data, "trmnl.db"))
}

func serve(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	data := dataFlag(fs)
	listen := fs.String("listen", ":8080", "address to listen on")
	publicURL := fs.String("public-url", "", "base URL devices reach this server at, such as http://192.168.1.10:8080 (default: from each request)")
	refresh := fs.Duration("refresh", 15*time.Minute, "how long devices sleep between wakes")
	renderURL := fs.String("render-url", "", "URL of the renderer, such as http://renderer:8081 (default: recipes are not rendered)")
	proxyListen := fs.String("proxy-listen", ":3128", "address the renderer's proxy listens on, when -render-url is set; never publish it")
	cacheDir := fs.String("cache", "cache", "directory holding the renderer's asset cache")
	debug := fs.Bool("debug", false, "log at debug level, including device request headers")
	_ = fs.Parse(args) // ExitOnError exits on a parse error.

	if *debug {
		slog.SetLogLoggerLevel(slog.LevelDebug)
	}
	if *refresh < time.Second {
		return fmt.Errorf("-refresh %v is under a second", *refresh)
	}
	key, err := secrets.FromEnv()
	if err != nil {
		return err
	}

	client, err := openDB(ctx, *data)
	if err != nil {
		return err
	}
	defer client.Close()
	images, err := imagestore.New(filepath.Join(*data, "images"))
	if err != nil {
		return err
	}

	mux := http.NewServeMux()
	deviceapi.New(client, images, deviceapi.Config{PublicURL: *publicURL, RefreshRate: *refresh}).Register(mux)
	servers := []*http.Server{{
		Addr:              *listen,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       2 * time.Minute,
	}}

	go expireLoop(ctx, client, images)
	if *renderURL != "" {
		proxy, err := startRendering(ctx, client, images, key, *data, *renderURL, *cacheDir)
		if err != nil {
			return err
		}
		// No read or write timeout: a CONNECT tunnel lasts as long as the
		// build uses it, and the renderer kills a build that runs too long.
		servers = append(servers, &http.Server{Addr: *proxyListen, Handler: proxy, ReadHeaderTimeout: 10 * time.Second})
	} else {
		slog.Warn("no -render-url: playlists that show recipes will wait for renders")
	}

	errc := make(chan error, len(servers))
	for _, srv := range servers {
		go func() { errc <- srv.ListenAndServe() }()
	}
	slog.Info("serving", "listen", *listen, "data", *data, "render_url", *renderURL)
	select {
	case err := <-errc:
		return fmt.Errorf("listen: %w", err)
	case <-ctx.Done():
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var errs []error
	for _, srv := range servers {
		if err := srv.Shutdown(shutdown); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errs = append(errs, fmt.Errorf("shutdown %s: %w", srv.Addr, err))
		}
	}
	return errors.Join(errs...)
}

// Asset cache limits: entries unused for a month go, and the oldest go
// first past the size limit.
const (
	cacheSweepEvery = time.Hour
	cacheMaxAge     = 30 * 24 * time.Hour
	cacheMaxBytes   = 2 << 30
)

// startRendering starts the scheduler, which renders through the renderer
// at renderURL, and returns the proxy the renderer's builds reach the
// internet through.
func startRendering(ctx context.Context, client *db.Client, images *imagestore.Store, key *secrets.Key,
	data, renderURL, cacheDir string,
) (http.Handler, error) {
	assets, err := cache.New(cacheDir)
	if err != nil {
		return nil, err
	}
	go assets.Janitor(ctx, cacheSweepEvery, cacheMaxAge, cacheMaxBytes)
	sched := scheduler.New(scheduler.Config{
		DB:         client,
		Images:     images,
		Renderer:   &render.Client{URL: renderURL, HTTP: &http.Client{Timeout: 10 * time.Minute}},
		RecipesDir: recipesDir(data),
		Secrets:    key,
		// TZ is the standard way to give a process its time zone.
		TimeZone:   os.Getenv("TZ"),
		AssetHosts: renderproxy.AssetHosts,
	})
	go sched.Run(ctx)
	return renderproxy.New(renderproxy.Config{
		Dialer:      guard.NewDialer(10 * time.Second),
		Cache:       assets,
		CachedHosts: renderproxy.AssetHosts,
	}), nil
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
		if err != nil {
			slog.Error("expire images", "err", err)
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
// image first, which refreshes its time, so an image named after the read
// is too new to delete.
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
	if err := deviceapi.SetJoinWindow(ctx, client, until); err != nil {
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
	window := fs.Duration("for", 30*time.Minute, "how long the device shows the calibration screen")
	dot := fs.Int("dot", 1, "dither dot size in pixels: 1 or 2")
	_ = fs.Parse(args) // ExitOnError exits on a parse error.
	override := map[int]string{1: deviceapi.ScreenCalibrate, 2: deviceapi.ScreenCalibrate2x2}[*dot]
	if fs.NArg() != 1 || *window <= 0 || override == "" {
		return errors.New("usage: trmnl-byos calibrate [-data dir] [-for duration] [-dot 1|2] <friendly ID or MAC>")
	}

	client, err := openDB(ctx, *data)
	if err != nil {
		return err
	}
	defer client.Close()
	until := time.Now().Add(*window)
	dev, err := deviceapi.ShowScreen(ctx, client, fs.Arg(0), override, until)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(out, "%s shows the calibration screen from its next wake until %s. Press its button to wake it now.\n",
		dev.FriendlyID, until.Format(time.Kitchen))
	return err
}

func devices(ctx context.Context, args []string, out io.Writer) error {
	fs := flag.NewFlagSet("devices", flag.ExitOnError)
	data := dataFlag(fs)
	_ = fs.Parse(args) // ExitOnError exits on a parse error.

	client, err := openDB(ctx, *data)
	if err != nil {
		return err
	}
	defer client.Close()
	rows, err := client.Read.ListDeviceStatus(ctx)
	if err != nil {
		return fmt.Errorf("list devices: %w", err)
	}
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	if _, err := fmt.Fprintln(tw, "ID\tMAC\tMODEL\tFIRMWARE\tLAST SEEN\tBATTERY\tRSSI\tWAKE"); err != nil {
		return err
	}
	for i := range rows {
		r, d := &rows[i], &rows[i].Device
		_, err := fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			d.FriendlyID, d.Mac, d.FwModel, d.FwVersion,
			lastSeen(r.LastSeen), orDash(r.BatteryV, "%.2f V"), orDash(r.Rssi, "%d dBm"), orDash(r.WakeTimeMs, "%d ms"))
		if err != nil {
			return err
		}
	}
	return tw.Flush()
}

func lastSeen(sec *int64) string {
	if sec == nil {
		return "never"
	}
	return time.Since(time.Unix(*sec, 0)).Truncate(time.Second).String() + " ago"
}

func orDash[T any](v *T, format string) string {
	if v == nil {
		return "-"
	}
	return fmt.Sprintf(format, *v)
}
