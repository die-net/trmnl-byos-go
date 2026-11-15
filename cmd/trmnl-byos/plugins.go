package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"maps"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/die-net/trmnl-byos-go/internal/db"
	"github.com/die-net/trmnl-byos-go/internal/playlist"
	"github.com/die-net/trmnl-byos-go/internal/recipe"
	"github.com/die-net/trmnl-byos-go/internal/secrets"
)

// recipesDir is where recipes are installed under the data directory.
func recipesDir(data string) string {
	return filepath.Join(data, "recipes")
}

// subcommand runs the named subcommand of a command, from cmds.
func subcommand(ctx context.Context, name string, args []string, out io.Writer,
	cmds map[string]func(context.Context, []string, io.Writer) error,
) error {
	if len(args) > 0 {
		if cmd, ok := cmds[args[0]]; ok {
			return cmd(ctx, args[1:], out)
		}
	}
	names := slices.Sorted(maps.Keys(cmds))
	return fmt.Errorf("usage: trmnl-byos %s <%s> [flags]", name, strings.Join(names, "|"))
}

func recipes(ctx context.Context, args []string, out io.Writer) error {
	return subcommand(ctx, "recipe", args, out, map[string]func(context.Context, []string, io.Writer) error{
		"add":  recipeAdd,
		"list": recipeList,
	})
}

func recipeAdd(ctx context.Context, args []string, out io.Writer) error {
	fs := flag.NewFlagSet("recipe add", flag.ExitOnError)
	data := dataFlag(fs)
	_ = fs.Parse(args) // ExitOnError exits on a parse error.
	if fs.NArg() != 1 {
		return errors.New("usage: trmnl-byos recipe add [-data dir] <recipe directory or zip>")
	}
	client, err := openDB(ctx, *data)
	if err != nil {
		return err
	}
	defer client.Close()
	src, err := filepath.Abs(fs.Arg(0))
	if err != nil {
		return err
	}
	rec, err := recipe.Install(ctx, client, recipesDir(*data), src, time.Now())
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(out, "Installed recipe %d, %q, rendered every %v.\n", rec.ID, rec.Name, time.Duration(rec.RefreshInterval)*time.Second)
	return err
}

func recipeList(ctx context.Context, args []string, out io.Writer) error {
	fs := flag.NewFlagSet("recipe list", flag.ExitOnError)
	data := dataFlag(fs)
	_ = fs.Parse(args) // ExitOnError exits on a parse error.
	client, err := openDB(ctx, *data)
	if err != nil {
		return err
	}
	defer client.Close()
	rows, err := client.Read.ListRecipes(ctx)
	if err != nil {
		return fmt.Errorf("list recipes: %w", err)
	}
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	if _, err := fmt.Fprintln(tw, "ID\tNAME\tREFRESH\tSOURCE"); err != nil {
		return err
	}
	for i := range rows {
		r := &rows[i]
		if _, err := fmt.Fprintf(tw, "%d\t%s\t%v\t%s\n", r.ID, r.Name, time.Duration(r.RefreshInterval)*time.Second, r.Source); err != nil {
			return err
		}
	}
	return tw.Flush()
}

// fieldFlag collects repeated name=value flags.
type fieldFlag map[string]string

func (fieldFlag) String() string { return "" }

func (f fieldFlag) Set(v string) error {
	name, value, ok := strings.Cut(v, "=")
	if !ok || name == "" {
		return fmt.Errorf("%q is not name=value", v)
	}
	f[name] = value
	return nil
}

func instances(ctx context.Context, args []string, out io.Writer) error {
	return subcommand(ctx, "instance", args, out, map[string]func(context.Context, []string, io.Writer) error{
		"add":    instanceAdd,
		"list":   instanceList,
		"remove": instanceRemove,
	})
}

func instanceAdd(ctx context.Context, args []string, out io.Writer) error {
	fs := flag.NewFlagSet("instance add", flag.ExitOnError)
	data := dataFlag(fs)
	recipeID := fs.Int64("recipe", 0, "ID of the recipe, from recipe list")
	name := fs.String("name", "", "name of the instance (default: the recipe's)")
	palette := fs.String("palette", "", "palette to render with when the device takes it, such as bw (default: the device's)")
	fields, secretFields := fieldFlag{}, fieldFlag{}
	fs.Var(fields, "field", "custom field value as keyname=value; repeat for each field")
	fs.Var(secretFields, "secret", "secret custom field value as keyname=value, stored encrypted with "+secrets.EnvVar+"; repeat for each")
	_ = fs.Parse(args) // ExitOnError exits on a parse error.
	if *recipeID == 0 || fs.NArg() != 0 {
		return errors.New("usage: trmnl-byos instance add [-data dir] -recipe ID [-name name] [-palette id] [-field k=v ...] [-secret k=v ...]")
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
	inst, err := recipe.CreateInstance(ctx, client, key, recipe.InstanceParams{
		RecipeID: *recipeID,
		Name:     *name,
		Palette:  *palette,
		Fields:   fields,
		Secrets:  secretFields,
	}, time.Now())
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(out, "Created instance %d, %q.\n", inst.ID, inst.Name)
	return err
}

func instanceList(ctx context.Context, args []string, out io.Writer) error {
	fs := flag.NewFlagSet("instance list", flag.ExitOnError)
	data := dataFlag(fs)
	_ = fs.Parse(args) // ExitOnError exits on a parse error.
	client, err := openDB(ctx, *data)
	if err != nil {
		return err
	}
	defer client.Close()
	rows, err := client.Read.ListInstances(ctx)
	if err != nil {
		return fmt.Errorf("list instances: %w", err)
	}
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	if _, err := fmt.Fprintln(tw, "ID\tNAME\tRECIPE\tPALETTE"); err != nil {
		return err
	}
	for i := range rows {
		r := &rows[i]
		pal := r.PluginInstance.Palette
		if pal == "" {
			pal = "device's"
		}
		if _, err := fmt.Fprintf(tw, "%d\t%s\t%d %s\t%s\n", r.PluginInstance.ID, r.PluginInstance.Name, r.PluginInstance.RecipeID, r.RecipeName, pal); err != nil {
			return err
		}
	}
	return tw.Flush()
}

func instanceRemove(ctx context.Context, args []string, out io.Writer) error {
	return removeByID(ctx, args, out, "instance remove", "instance", recipe.DeleteInstance)
}

// removeByID runs a remove command that takes the ID of what to remove.
// noun is what it removes, and remove does it.
func removeByID(ctx context.Context, args []string, out io.Writer, name, noun string,
	remove func(context.Context, *db.Client, int64) error,
) error {
	fs := flag.NewFlagSet(name, flag.ExitOnError)
	data := dataFlag(fs)
	_ = fs.Parse(args) // ExitOnError exits on a parse error.
	id, err := strconv.ParseInt(fs.Arg(0), 10, 64)
	if fs.NArg() != 1 || err != nil {
		return fmt.Errorf("usage: trmnl-byos %s [-data dir] <%s ID>", name, noun)
	}
	client, err := openDB(ctx, *data)
	if err != nil {
		return err
	}
	defer client.Close()
	if err := remove(ctx, client, id); err != nil {
		return err
	}
	_, err = fmt.Fprintf(out, "Removed %s %d.\n", noun, id)
	return err
}

func playlists(ctx context.Context, args []string, out io.Writer) error {
	return subcommand(ctx, "playlist", args, out, map[string]func(context.Context, []string, io.Writer) error{
		"add":      playlistAdd,
		"remove":   playlistRemove,
		"list":     playlistList,
		"move":     playlistMove,
		"set":      playlistSet,
		"schedule": playlistSchedule,
		"create":   playlistCreate,
		"rename":   playlistRename,
		"delete":   playlistDelete,
		"link":     playlistLink,
		"unlink":   playlistUnlink,
	})
}

func playlistAdd(ctx context.Context, args []string, out io.Writer) error {
	fs := flag.NewFlagSet("playlist add", flag.ExitOnError)
	data := dataFlag(fs)
	deviceID := fs.String("device", "", "friendly ID or MAC of a device to show the playlist on")
	name := fs.String("playlist", "", "playlist to add to, created if new (default: the device's friendly ID)")
	instanceID := fs.Int64("instance", 0, "ID of the instance to show, from instance list")
	duration := fs.Duration("duration", playlist.DefaultDuration, "how long the item shows; 0 shows it until a button press")
	_ = fs.Parse(args) // ExitOnError exits on a parse error.
	if *instanceID == 0 || (*deviceID == "" && *name == "") || fs.NArg() != 0 {
		return errors.New("usage: trmnl-byos playlist add [-data dir] -instance ID [-device ID] [-playlist name] [-duration d]")
	}
	client, err := openDB(ctx, *data)
	if err != nil {
		return err
	}
	defer client.Close()
	item, plName, err := playlist.AddItem(ctx, client, *name, *deviceID, *instanceID, *duration)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(out, "Added item %d to playlist %q.\n", item.ID, plName)
	return err
}

func playlistRemove(ctx context.Context, args []string, out io.Writer) error {
	return removeByID(ctx, args, out, "playlist remove", "item", playlist.DeleteItem)
}

// parseInterspersed parses args with fs and returns the arguments that are
// not flags. Unlike fs.Parse alone, it accepts flags after them too, so
// "playlist set 5 -hidden" works like "playlist set -hidden 5".
func parseInterspersed(fs *flag.FlagSet, args []string) []string {
	var rest []string
	for {
		_ = fs.Parse(args) // ExitOnError exits on a parse error.
		if fs.NArg() == 0 {
			return rest
		}
		rest = append(rest, fs.Arg(0))
		args = fs.Args()[1:]
	}
}

// scheduleFlags are the flags that set a schedule, shared by the commands
// that set one.
type scheduleFlags struct {
	fs                        *flag.FlagSet
	days, window, from, until *string
}

// addScheduleFlags defines the schedule flags on fs.
func addScheduleFlags(fs *flag.FlagSet) scheduleFlags {
	return scheduleFlags{
		fs:     fs,
		days:   fs.String("days", "", "weekdays it is active, such as mon,tue,wed or all"),
		window: fs.String("window", "", "hours it is active as HH:MM-HH:MM, which wraps past midnight if the end is first, or all for all day"),
		from:   fs.String("from", "", "first day it is active as YYYY-MM-DD in the server's time zone; empty for no start"),
		until:  fs.String("until", "", "last day it is active as YYYY-MM-DD in the server's time zone; empty for no end"),
	}
}

// edit returns the change that the schedule flags given on the command line
// ask for, or nil if none was given. Dates are days in loc.
func (f scheduleFlags) edit(loc *time.Location) (*playlist.ScheduleEdit, error) {
	var e playlist.ScheduleEdit
	var given bool
	var err error
	f.fs.Visit(func(fl *flag.Flag) {
		if err != nil {
			return
		}
		switch fl.Name {
		case "days":
			given = true
			e.Days = new(int64)
			*e.Days, err = playlist.ParseDays(*f.days)
		case "window":
			given = true
			e.WindowStart, e.WindowEnd = new(int64), new(int64)
			*e.WindowStart, *e.WindowEnd, err = playlist.ParseWindow(*f.window)
		case "from":
			given = true
			e.StartsAt = new(int64)
			*e.StartsAt, err = playlist.ParseDay(*f.from, loc)
		case "until":
			given = true
			e.EndsAt = new(int64)
			// The last day is included, so the schedule ends when the next begins.
			*e.EndsAt, err = playlist.ParseLastDay(*f.until, loc)
		}
	})
	if err != nil || !given {
		return nil, err
	}
	return &e, nil
}

func playlistMove(ctx context.Context, args []string, out io.Writer) error {
	fs := flag.NewFlagSet("playlist move", flag.ExitOnError)
	data := dataFlag(fs)
	rest := parseInterspersed(fs, args)
	usage := errors.New("usage: trmnl-byos playlist move [-data dir] <item ID> <index>")
	if len(rest) != 2 {
		return usage
	}
	id, err := strconv.ParseInt(rest[0], 10, 64)
	if err != nil {
		return usage
	}
	index, err := strconv.ParseInt(rest[1], 10, 64)
	if err != nil {
		return usage
	}
	client, err := openDB(ctx, *data)
	if err != nil {
		return err
	}
	defer client.Close()
	if err := playlist.MoveItem(ctx, client, id, index); err != nil {
		return err
	}
	_, err = fmt.Fprintf(out, "Moved item %d to index %d.\n", id, index)
	return err
}

func playlistSet(ctx context.Context, args []string, out io.Writer) error {
	fs := flag.NewFlagSet("playlist set", flag.ExitOnError)
	data := dataFlag(fs)
	duration := fs.Duration("duration", 0, "how long the item shows; 0 shows it until a button press")
	hidden := fs.Bool("hidden", false, "never show the item, or -hidden=false to show it again")
	sched := addScheduleFlags(fs)
	rest := parseInterspersed(fs, args)
	usage := errors.New("usage: trmnl-byos playlist set [-data dir] <item ID> [-duration d] [-hidden=bool] [-days d,d] [-window HH:MM-HH:MM|all] [-from date] [-until date]")
	if len(rest) != 1 {
		return usage
	}
	id, err := strconv.ParseInt(rest[0], 10, 64)
	if err != nil {
		return usage
	}
	var u playlist.ItemUpdate
	fs.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "duration":
			u.Duration = duration
		case "hidden":
			u.Hidden = hidden
		}
	})
	zone, err := serverZone()
	if err != nil {
		return err
	}
	if u.Schedule, err = sched.edit(zone); err != nil {
		return err
	}
	if u.Duration == nil && u.Hidden == nil && u.Schedule == nil {
		return usage
	}
	client, err := openDB(ctx, *data)
	if err != nil {
		return err
	}
	defer client.Close()
	if err := playlist.UpdateItem(ctx, client, id, u); err != nil {
		return err
	}
	_, err = fmt.Fprintf(out, "Updated item %d.\n", id)
	return err
}

func playlistSchedule(ctx context.Context, args []string, out io.Writer) error {
	fs := flag.NewFlagSet("playlist schedule", flag.ExitOnError)
	data := dataFlag(fs)
	sched := addScheduleFlags(fs)
	rest := parseInterspersed(fs, args)
	usage := errors.New("usage: trmnl-byos playlist schedule [-data dir] <playlist name> [-days d,d] [-window HH:MM-HH:MM|all] [-from date] [-until date]")
	zone, err := serverZone()
	if err != nil {
		return err
	}
	edit, err := sched.edit(zone)
	if err != nil {
		return err
	}
	if len(rest) != 1 || edit == nil {
		return usage
	}
	client, err := openDB(ctx, *data)
	if err != nil {
		return err
	}
	defer client.Close()
	if err := playlist.UpdatePlaylist(ctx, client, playlist.ByName(rest[0]), playlist.Update{Schedule: edit}); err != nil {
		return err
	}
	_, err = fmt.Fprintf(out, "Updated playlist %q.\n", rest[0])
	return err
}

func playlistCreate(ctx context.Context, args []string, out io.Writer) error {
	fs := flag.NewFlagSet("playlist create", flag.ExitOnError)
	data := dataFlag(fs)
	rest := parseInterspersed(fs, args)
	if len(rest) != 1 {
		return errors.New("usage: trmnl-byos playlist create [-data dir] <playlist name>")
	}
	client, err := openDB(ctx, *data)
	if err != nil {
		return err
	}
	defer client.Close()
	pl, err := playlist.CreatePlaylist(ctx, client, rest[0])
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(out, "Created playlist %q.\n", pl.Name)
	return err
}

func playlistRename(ctx context.Context, args []string, out io.Writer) error {
	fs := flag.NewFlagSet("playlist rename", flag.ExitOnError)
	data := dataFlag(fs)
	rest := parseInterspersed(fs, args)
	if len(rest) != 2 {
		return errors.New("usage: trmnl-byos playlist rename [-data dir] <playlist name> <new name>")
	}
	client, err := openDB(ctx, *data)
	if err != nil {
		return err
	}
	defer client.Close()
	if err := playlist.UpdatePlaylist(ctx, client, playlist.ByName(rest[0]), playlist.Update{Name: &rest[1]}); err != nil {
		return err
	}
	_, err = fmt.Fprintf(out, "Renamed playlist %q to %q.\n", rest[0], strings.TrimSpace(rest[1]))
	return err
}

func playlistDelete(ctx context.Context, args []string, out io.Writer) error {
	fs := flag.NewFlagSet("playlist delete", flag.ExitOnError)
	data := dataFlag(fs)
	rest := parseInterspersed(fs, args)
	if len(rest) != 1 {
		return errors.New("usage: trmnl-byos playlist delete [-data dir] <playlist name>")
	}
	client, err := openDB(ctx, *data)
	if err != nil {
		return err
	}
	defer client.Close()
	if err := playlist.DeletePlaylist(ctx, client, playlist.ByName(rest[0])); err != nil {
		return err
	}
	_, err = fmt.Fprintf(out, "Deleted playlist %q.\n", rest[0])
	return err
}

func playlistLink(ctx context.Context, args []string, out io.Writer) error {
	fs := flag.NewFlagSet("playlist link", flag.ExitOnError)
	data := dataFlag(fs)
	priority := fs.Int64("priority", 0, "the playlist's index in the device's order, from 0, which is tried first; default: last for a new link, unchanged for an existing one")
	rest := parseInterspersed(fs, args)
	if len(rest) != 2 {
		return errors.New("usage: trmnl-byos playlist link [-data dir] <playlist name> <device> [-priority N]")
	}
	var prio *int64
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "priority" {
			prio = priority
		}
	})
	client, err := openDB(ctx, *data)
	if err != nil {
		return err
	}
	defer client.Close()
	if err := playlist.LinkDevice(ctx, client, playlist.ByName(rest[0]), rest[1], prio); err != nil {
		return err
	}
	_, err = fmt.Fprintf(out, "Linked device %s to playlist %q.\n", rest[1], rest[0])
	return err
}

func playlistUnlink(ctx context.Context, args []string, out io.Writer) error {
	fs := flag.NewFlagSet("playlist unlink", flag.ExitOnError)
	data := dataFlag(fs)
	rest := parseInterspersed(fs, args)
	if len(rest) != 2 {
		return errors.New("usage: trmnl-byos playlist unlink [-data dir] <playlist name> <device>")
	}
	client, err := openDB(ctx, *data)
	if err != nil {
		return err
	}
	defer client.Close()
	if err := playlist.UnlinkDevice(ctx, client, playlist.ByName(rest[0]), rest[1]); err != nil {
		return err
	}
	_, err = fmt.Fprintf(out, "Unlinked device %s from playlist %q.\n", rest[1], rest[0])
	return err
}

func playlistList(ctx context.Context, args []string, out io.Writer) error {
	fs := flag.NewFlagSet("playlist list", flag.ExitOnError)
	data := dataFlag(fs)
	_ = fs.Parse(args) // ExitOnError exits on a parse error.
	zone, err := serverZone()
	if err != nil {
		return err
	}
	client, err := openDB(ctx, *data)
	if err != nil {
		return err
	}
	defer client.Close()
	lists, err := client.Read.ListPlaylists(ctx)
	if err != nil {
		return fmt.Errorf("list playlists: %w", err)
	}
	devs, err := client.Read.ListPlaylistDevices(ctx)
	if err != nil {
		return fmt.Errorf("list playlist devices: %w", err)
	}
	items, err := client.Read.ListPlaylistItems(ctx)
	if err != nil {
		return fmt.Errorf("list playlist items: %w", err)
	}
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	if _, err := fmt.Fprintln(tw, "PLAYLIST\tSCHEDULE"); err != nil {
		return err
	}
	for i := range lists {
		l := &lists[i]
		sched := playlist.Schedule{Days: l.Days, WindowStart: l.WindowStart, WindowEnd: l.WindowEnd, StartsAt: l.StartsAt, EndsAt: l.EndsAt}
		if _, err := fmt.Fprintf(tw, "%s\t%s\n", l.Name, sched.Describe(zone)); err != nil {
			return err
		}
	}
	if _, err := fmt.Fprintln(tw, "\nPLAYLIST\tDEVICE\tPRIORITY"); err != nil {
		return err
	}
	for i := range devs {
		d := &devs[i]
		if _, err := fmt.Fprintf(tw, "%s\t%s\t%d\n", d.Playlist, d.FriendlyID, d.Priority); err != nil {
			return err
		}
	}
	if _, err := fmt.Fprintln(tw, "\nPLAYLIST\tITEM\tINDEX\tINSTANCE\tDURATION\tSCHEDULE\tHIDDEN"); err != nil {
		return err
	}
	for i := range items {
		it := &items[i]
		dur := "until pressed"
		if it.Duration > 0 {
			dur = (time.Duration(it.Duration) * time.Second).String()
		}
		sched := playlist.Schedule{Days: it.Days, WindowStart: it.WindowStart, WindowEnd: it.WindowEnd, StartsAt: it.StartsAt, EndsAt: it.EndsAt}
		hide := ""
		if it.Hidden != 0 {
			hide = "hidden"
		}
		if _, err := fmt.Fprintf(tw, "%s\t%d\t%d\t%d %s\t%s\t%s\t%s\n", it.Playlist, it.ID, it.Position, it.InstanceID, it.InstanceName, dur, sched.Describe(zone), hide); err != nil {
			return err
		}
	}
	return tw.Flush()
}
