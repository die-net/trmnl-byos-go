// Command trmnl-byos is a self-hosted server for TRMNL e-ink displays.
//
//	trmnl-byos serve    run the server
//	trmnl-byos join     let new devices register for a while
//	trmnl-byos devices  list registered devices
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

	"github.com/die-net/trmnl-byos-go/internal/db"
	"github.com/die-net/trmnl-byos-go/internal/deviceapi"
	"github.com/die-net/trmnl-byos-go/internal/imagestore"
)

const usage = `usage: trmnl-byos <command> [flags]

commands:
  serve    run the server
  join     let new devices register for a while
  devices  list registered devices

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
	debug := fs.Bool("debug", false, "log at debug level, including device request headers")
	_ = fs.Parse(args) // ExitOnError exits on a parse error.

	if *debug {
		slog.SetLogLoggerLevel(slog.LevelDebug)
	}
	if *refresh < time.Second {
		return fmt.Errorf("-refresh %v is under a second", *refresh)
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
	srv := &http.Server{
		Addr:              *listen,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       2 * time.Minute,
	}

	go expireLoop(ctx, client, images)
	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()
	slog.Info("serving", "listen", *listen, "data", *data)
	select {
	case err := <-errc:
		return fmt.Errorf("listen: %w", err)
	case <-ctx.Done():
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdown); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("shutdown: %w", err)
	}
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
