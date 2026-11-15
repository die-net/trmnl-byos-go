package main

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/die-net/trmnl-byos-go/internal/db"
	"github.com/die-net/trmnl-byos-go/internal/device"
	"github.com/die-net/trmnl-byos-go/internal/playlist"
	"github.com/die-net/trmnl-byos-go/internal/recipe"
	"github.com/die-net/trmnl-byos-go/internal/render"
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

// fieldFlag collects repeated name=value flags into custom field values. A
// name given twice or more is a list, for a multi-select.
type fieldFlag map[string]recipe.Value

func (fieldFlag) String() string { return "" }

func (f fieldFlag) Set(v string) error {
	name, value, ok := strings.Cut(v, "=")
	if !ok || name == "" {
		return fmt.Errorf("%q is not name=value", v)
	}
	switch cur, seen := f[name]; {
	case !seen:
		f[name] = recipe.Value{One: value}
	case cur.IsList():
		f[name] = recipe.Value{Many: append(cur.Many, value)}
	default:
		f[name] = recipe.Value{Many: []string{cur.One, value}}
	}
	return nil
}

// nameFlag collects the names of custom fields, as -clear, -secret and
// -plain take them.
type nameFlag []string

func (nameFlag) String() string { return "" }

func (c *nameFlag) Set(v string) error {
	if v == "" {
		return errors.New("a field name is needed")
	}
	*c = append(*c, v)
	return nil
}

func instances(ctx context.Context, args []string, out io.Writer) error {
	return subcommand(ctx, "instance", args, out, map[string]func(context.Context, []string, io.Writer) error{
		"add": func(ctx context.Context, args []string, out io.Writer) error {
			return instanceAdd(ctx, args, os.Stdin, out)
		},
		"list": instanceList,
		"set": func(ctx context.Context, args []string, out io.Writer) error {
			return instanceSet(ctx, args, os.Stdin, out)
		},
		"remove": instanceRemove,
	})
}

// fileFlag collects repeated name=path flags, each naming a file that holds
// a field's value.
type fileFlag []string

func (fileFlag) String() string { return "" }

func (f *fileFlag) Set(v string) error {
	if name, path, ok := strings.Cut(v, "="); !ok || name == "" || path == "" {
		return fmt.Errorf("%q is not name=path", v)
	}
	*f = append(*f, v)
	return nil
}

// addFiles reads each file of the flag into fields, as -field would give it
// the contents: from stdin for the path "-", which only one may be, and with
// one trailing newline dropped, since an editor or echo adds one. A value
// cannot be bigger than a render job takes, so a file is read to that and no
// further.
func (f fileFlag) addFiles(fields fieldFlag, stdin io.Reader) error {
	usedStdin := false
	for _, v := range f {
		name, path, _ := strings.Cut(v, "=")
		if path == "-" {
			if usedStdin {
				return errors.New("only one -field-file can read stdin")
			}
			usedStdin = true
		}
		b, err := readValue(path, stdin)
		if err != nil {
			return fmt.Errorf("field %q: %w", name, err)
		}
		b = bytes.TrimSuffix(b, []byte("\n"))
		if err := fields.Set(name + "=" + string(b)); err != nil {
			return err
		}
	}
	return nil
}

// readValue reads a field's value from the file at path, or from stdin for
// "-". A value cannot be bigger than a render job takes, so it reads no
// further than that.
func readValue(path string, stdin io.Reader) ([]byte, error) {
	r := stdin
	if path != "-" {
		file, err := os.Open(path) //nolint:gosec // G304: the path is the command-line argument of the admin running this command, who may give any file they can read, as firmware add's FILE is.
		if err != nil {
			return nil, err
		}
		defer file.Close()
		r = file
	}
	b, err := io.ReadAll(io.LimitReader(r, render.MaxEnvBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	if len(b) > render.MaxEnvBytes {
		return nil, fmt.Errorf("%s is over the %d KB a value may come to", path, render.MaxEnvBytes/1024)
	}
	return b, nil
}

// fieldHelp is the help of -field.
const fieldHelp = "custom field value as keyname=value; repeat for each field, and for each choice of a multi-select. The value is in the command line, which ps and the shell history show, so give a value meant to be secret, such as a password or a key, with -field-file. A password field, and a field marked with -secret, is stored encrypted with " + secrets.EnvVar

// marks returns the marks of -secret and -plain as an update's Secret: true
// for each secret name and false for each plain one. A name in both is an
// error, so it is found before the database is opened.
func marks(secret, plain nameFlag) (map[string]bool, error) {
	if len(secret)+len(plain) == 0 {
		return nil, nil
	}
	out := make(map[string]bool, len(secret)+len(plain))
	for _, k := range secret {
		out[k] = true
	}
	for _, k := range plain {
		if out[k] {
			return nil, fmt.Errorf("field %q is given to both -secret and -plain", k)
		}
		out[k] = false
	}
	return out, nil
}

func instanceAdd(ctx context.Context, args []string, stdin io.Reader, out io.Writer) error {
	fs := flag.NewFlagSet("instance add", flag.ExitOnError)
	data := dataFlag(fs)
	recipeID := fs.Int64("recipe", 0, "ID of the recipe, from recipe list")
	name := fs.String("name", "", "name of the instance (default: the recipe's)")
	palette := fs.String("palette", "", "palette to render with when the device takes it, such as bw (default: the device's)")
	fields, files, secret := fieldFlag{}, fileFlag{}, nameFlag{}
	fs.Var(fields, "field", fieldHelp)
	fs.Var(&secret, "secret", "name of a text field to mark secret, so its value is stored encrypted and never shown back; repeat for each. A value shorter than 6 characters cannot be secret")
	fs.Var(&files, "field-file", "custom field value read from a file as keyname=path, or from stdin for -, which only one may be; one trailing newline is dropped. Repeat as for -field")
	_ = fs.Parse(args) // ExitOnError exits on a parse error.
	if *recipeID == 0 || fs.NArg() != 0 {
		return errors.New("usage: trmnl-byos instance add [-data dir] -recipe ID [-name name] [-palette id] [-field k=v ...] [-field-file k=path ...] [-secret k ...]")
	}
	marked, err := marks(secret, nil)
	if err != nil {
		return err
	}
	if err := files.addFiles(fields, stdin); err != nil {
		return err
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
	inst, err := recipe.CreateInstance(ctx, client, key, recipesDir(*data), recipe.InstanceParams{
		RecipeID: *recipeID,
		Name:     *name,
		Palette:  *palette,
		Values:   fields,
		Secret:   marked,
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

func instanceSet(ctx context.Context, args []string, stdin io.Reader, out io.Writer) error {
	fs := flag.NewFlagSet("instance set", flag.ExitOnError)
	data := dataFlag(fs)
	name := fs.String("name", "", "new name of the instance; empty for the recipe's")
	palette := fs.String("palette", "", "new palette to render with, such as bw; empty for the device's")
	fields, cleared, secret, plain := fieldFlag{}, nameFlag{}, nameFlag{}, nameFlag{}
	files := fileFlag{}
	fs.Var(fields, "field", fieldHelp)
	fs.Var(&files, "field-file", "custom field value read from a file as keyname=path, or from stdin for -, which only one may be; one trailing newline is dropped. Repeat as for -field")
	fs.Var(&cleared, "clear", "name of a custom field to clear, a password field included; repeat for each. A field left out is kept, and so is a secret given an empty value")
	fs.Var(&secret, "secret", "name of a text field to mark secret, which moves its value into the encrypted fields; repeat for each. A value shorter than 6 characters cannot be secret")
	fs.Var(&plain, "plain", "name of a field marked secret to unmark; repeat for each. A secret is never shown back, so a field with a value needs a new one with -field or -field-file, or -clear. A password field is always secret")
	rest := parseInterspersed(fs, args)
	usage := errors.New("usage: trmnl-byos instance set [-data dir] <instance ID> [-name name] [-palette id] [-field k=v ...] [-field-file k=path ...] [-clear k ...] [-secret k ...] [-plain k ...]")
	if len(rest) != 1 {
		return usage
	}
	id, err := strconv.ParseInt(rest[0], 10, 64)
	if err != nil {
		return usage
	}
	marked, err := marks(secret, plain)
	if err != nil {
		return err
	}
	var u recipe.InstanceUpdate
	fs.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "name":
			u.Name = name
		case "palette":
			u.Palette = palette
		}
	})
	if err := files.addFiles(fields, stdin); err != nil {
		return err
	}
	if u.Name == nil && u.Palette == nil && len(fields) == 0 && len(cleared) == 0 && len(marked) == 0 {
		return usage
	}
	u.Values, u.Clear, u.Secret = fields, cleared, marked
	key, err := secrets.FromEnv()
	if err != nil {
		return err
	}
	client, err := openDB(ctx, *data)
	if err != nil {
		return err
	}
	defer client.Close()
	inst, err := recipe.UpdateInstance(ctx, client, key, recipesDir(*data), id, u, time.Now())
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(out, "Updated instance %d, %q.\n", inst.ID, inst.Name)
	return err
}

func instanceRemove(ctx context.Context, args []string, out io.Writer) error {
	return removeByID(ctx, args, out, "instance remove", "instance", func(ctx context.Context, client *db.Client, id int64) error {
		err := recipe.DeleteInstance(ctx, client, id)
		if errors.Is(err, recipe.ErrInstanceInUse) {
			return fmt.Errorf("%w; see playlist list, and take them out with playlist remove first", err)
		}
		return err
	})
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
		"order":    playlistOrder,
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
	// The command creates the playlist if it is missing, so a first
	// "playlist add -device ID" works on a new install. AddItemCreating does
	// it in the add's transaction, so a refused add makes no playlist.
	plName := *name
	if plName == "" {
		dev, err := device.FindDevice(ctx, client.Read, *deviceID)
		if err != nil {
			return err
		}
		plName = dev.FriendlyID
	}
	item, err := playlist.AddItemCreating(ctx, client, plName, *deviceID, *instanceID, *duration)
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
// "playlist set 5 -hidden" works like "playlist set -hidden 5". An argument
// that is a negative whole number, such as the -1 of "playlist move 5 -1", is
// not a flag: no command has a flag named for a number, and the command
// refuses the number itself with a message about the range.
func parseInterspersed(fs *flag.FlagSet, args []string) []string {
	var rest []string
	for {
		if len(args) > 0 && isNegativeNumber(args[0]) {
			rest = append(rest, args[0])
			args = args[1:]
			continue
		}
		_ = fs.Parse(args) // ExitOnError exits on a parse error.
		if fs.NArg() == 0 {
			return rest
		}
		rest = append(rest, fs.Arg(0))
		args = fs.Args()[1:]
	}
}

// isNegativeNumber reports whether arg is a whole number below zero.
func isNegativeNumber(arg string) bool {
	n, err := strconv.ParseInt(arg, 10, 64)
	return err == nil && n < 0
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
	if err := playlist.DeletePlaylist(ctx, client, playlist.ByName(rest[0]), nil); err != nil {
		return err
	}
	_, err = fmt.Fprintf(out, "Deleted playlist %q.\n", rest[0])
	return err
}

// playlistDeviceCommand runs a command that takes <playlist name> <device>
// and changes the link between them with do, then prints done, which has a
// %s for the device and a %q for the playlist.
func playlistDeviceCommand(ctx context.Context, name string, args []string, out io.Writer, done string,
	do func(ctx context.Context, client *db.Client, pl playlist.Ref, deviceName string) error,
) error {
	fs := flag.NewFlagSet("playlist "+name, flag.ExitOnError)
	data := dataFlag(fs)
	rest := parseInterspersed(fs, args)
	if len(rest) != 2 {
		return fmt.Errorf("usage: trmnl-byos playlist %s [-data dir] <playlist name> <device>", name)
	}
	client, err := openDB(ctx, *data)
	if err != nil {
		return err
	}
	defer client.Close()
	if err := do(ctx, client, playlist.ByName(rest[0]), rest[1]); err != nil {
		return err
	}
	_, err = fmt.Fprintf(out, done, rest[1], rest[0])
	return err
}

func playlistLink(ctx context.Context, args []string, out io.Writer) error {
	return playlistDeviceCommand(ctx, "link", args, out, "Linked device %s to playlist %q.\n", playlist.LinkDevice)
}

func playlistOrder(ctx context.Context, args []string, out io.Writer) error {
	fs := flag.NewFlagSet("playlist order", flag.ExitOnError)
	data := dataFlag(fs)
	rest := parseInterspersed(fs, args)
	usage := errors.New("usage: trmnl-byos playlist order [-data dir] <playlist name> <device> <from> <to>")
	if len(rest) != 4 {
		return usage
	}
	from, err := strconv.ParseInt(rest[2], 10, 64)
	if err != nil {
		return usage
	}
	to, err := strconv.ParseInt(rest[3], 10, 64)
	if err != nil {
		return usage
	}
	client, err := openDB(ctx, *data)
	if err != nil {
		return err
	}
	defer client.Close()
	pl, shown, err := deviceOrder(ctx, client, rest[0], rest[1], from)
	if err != nil {
		return err
	}
	// A change to the device's order after it was read is refused by MoveLink.
	if err := playlist.MoveLink(ctx, client, playlist.ByID(pl), rest[1], shown, to); err != nil {
		return err
	}
	_, err = fmt.Fprintf(out, "Moved playlist %q from index %d to %d in device %s's order.\n", rest[0], from, to, rest[1])
	return err
}

// deviceOrder reads the IDs of the playlists the device, named by friendly ID
// or MAC, is linked to, in the order it tries them, and the ID of the playlist
// with the name. It returns an error wrapping playlist.ErrNotLinked if the
// device is not linked to it, and one wrapping playlist.ErrOrderChanged if the
// playlist is not at index from, which is where the caller saw it.
func deviceOrder(ctx context.Context, client *db.Client, playlistName, deviceName string, from int64) (playlistID int64, order []int64, err error) {
	err = client.ReadTx(ctx, func(q *db.Queries) error {
		pl, err := q.GetPlaylistByName(ctx, playlistName)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%w: %q", playlist.ErrNoPlaylist, playlistName)
		} else if err != nil {
			return fmt.Errorf("get playlist: %w", err)
		}
		dev, err := device.FindDevice(ctx, q, deviceName)
		if err != nil {
			return err
		}
		if order, err = q.ListDevicePlaylistIDs(ctx, dev.ID); err != nil {
			return fmt.Errorf("list links: %w", err)
		}
		playlistID = pl.ID
		at := slices.Index(order, pl.ID)
		switch {
		case at < 0:
			return fmt.Errorf("%w: playlist %q, device %s", playlist.ErrNotLinked, playlistName, dev.FriendlyID)
		case int64(at) != from:
			return fmt.Errorf("%w: the playlist is now at %d, not %d", playlist.ErrOrderChanged, at, from)
		}
		return nil
	})
	return playlistID, order, err
}

func playlistUnlink(ctx context.Context, args []string, out io.Writer) error {
	return playlistDeviceCommand(ctx, "unlink", args, out, "Unlinked device %s from playlist %q.\n", playlist.UnlinkDevice)
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
