package main

import (
	"context"
	"database/sql"
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
	"github.com/die-net/trmnl-byos-go/internal/deviceapi"
	"github.com/die-net/trmnl-byos-go/internal/playlist"
	"github.com/die-net/trmnl-byos-go/internal/profile"
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
		"add":  instanceAdd,
		"list": instanceList,
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
	if *palette != "" {
		ok, err := profile.KnownPalette(*palette)
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("unknown palette %q", *palette)
		}
	}
	for k := range secretFields {
		if _, ok := fields[k]; ok {
			return fmt.Errorf("field %q is given as both -field and -secret", k)
		}
	}
	key, err := secrets.FromEnv()
	if err != nil {
		return err
	}
	if len(secretFields) > 0 && key == nil {
		return secrets.ErrNoKey
	}

	client, err := openDB(ctx, *data)
	if err != nil {
		return err
	}
	defer client.Close()
	rec, err := client.Read.GetRecipe(ctx, *recipeID)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("no recipe %d", *recipeID)
	}
	if err != nil {
		return fmt.Errorf("get recipe: %w", err)
	}
	if *name == "" {
		*name = rec.Name
	}
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
		pal := r.Palette
		if pal == "" {
			pal = "device's"
		}
		if _, err := fmt.Fprintf(tw, "%d\t%s\t%d %s\t%s\n", r.ID, r.Name, r.RecipeID, r.RecipeName, pal); err != nil {
			return err
		}
	}
	return tw.Flush()
}

func playlists(ctx context.Context, args []string, out io.Writer) error {
	return subcommand(ctx, "playlist", args, out, map[string]func(context.Context, []string, io.Writer) error{
		"add":    playlistAdd,
		"remove": playlistRemove,
		"list":   playlistList,
	})
}

func playlistAdd(ctx context.Context, args []string, out io.Writer) error {
	fs := flag.NewFlagSet("playlist add", flag.ExitOnError)
	data := dataFlag(fs)
	device := fs.String("device", "", "friendly ID or MAC of a device to show the playlist on")
	name := fs.String("playlist", "", "playlist to add to, created if new (default: the device's friendly ID)")
	instanceID := fs.Int64("instance", 0, "ID of the instance to show, from instance list")
	duration := fs.Duration("duration", 15*time.Minute, "how long the item shows; 0 shows it until a button press")
	_ = fs.Parse(args) // ExitOnError exits on a parse error.
	if *instanceID == 0 || (*device == "" && *name == "") || fs.NArg() != 0 {
		return errors.New("usage: trmnl-byos playlist add [-data dir] -instance ID [-device ID] [-playlist name] [-duration d]")
	}
	client, err := openDB(ctx, *data)
	if err != nil {
		return err
	}
	defer client.Close()
	var dev db.Device
	if *device != "" {
		if dev, err = deviceapi.FindDevice(ctx, client.Read, *device); err != nil {
			return err
		}
		if *name == "" {
			*name = dev.FriendlyID
		}
	}
	item, err := playlist.AddItem(ctx, client, *name, dev.ID, *instanceID, *duration)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(out, "Added item %d to playlist %q.\n", item.ID, *name)
	return err
}

func playlistRemove(ctx context.Context, args []string, out io.Writer) error {
	fs := flag.NewFlagSet("playlist remove", flag.ExitOnError)
	data := dataFlag(fs)
	_ = fs.Parse(args) // ExitOnError exits on a parse error.
	id, err := strconv.ParseInt(fs.Arg(0), 10, 64)
	if fs.NArg() != 1 || err != nil {
		return errors.New("usage: trmnl-byos playlist remove [-data dir] <item ID>")
	}
	client, err := openDB(ctx, *data)
	if err != nil {
		return err
	}
	defer client.Close()
	n, err := client.Write.DeletePlaylistItem(ctx, id)
	if err != nil {
		return fmt.Errorf("remove item: %w", err)
	}
	if n == 0 {
		return fmt.Errorf("no item %d", id)
	}
	_, err = fmt.Fprintf(out, "Removed item %d.\n", id)
	return err
}

func playlistList(ctx context.Context, args []string, out io.Writer) error {
	fs := flag.NewFlagSet("playlist list", flag.ExitOnError)
	data := dataFlag(fs)
	_ = fs.Parse(args) // ExitOnError exits on a parse error.
	client, err := openDB(ctx, *data)
	if err != nil {
		return err
	}
	defer client.Close()
	devs, err := client.Read.ListPlaylistDevices(ctx)
	if err != nil {
		return fmt.Errorf("list playlist devices: %w", err)
	}
	items, err := client.Read.ListPlaylistItems(ctx)
	if err != nil {
		return fmt.Errorf("list playlist items: %w", err)
	}
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	if _, err := fmt.Fprintln(tw, "PLAYLIST\tDEVICE\tPRIORITY"); err != nil {
		return err
	}
	for i := range devs {
		d := &devs[i]
		if _, err := fmt.Fprintf(tw, "%s\t%s\t%d\n", d.Playlist, d.FriendlyID, d.Priority); err != nil {
			return err
		}
	}
	if _, err := fmt.Fprintln(tw, "\nPLAYLIST\tITEM\tINSTANCE\tDURATION"); err != nil {
		return err
	}
	for i := range items {
		it := &items[i]
		dur := "until pressed"
		if it.Duration > 0 {
			dur = (time.Duration(it.Duration) * time.Second).String()
		}
		if _, err := fmt.Fprintf(tw, "%s\t%d\t%d %s\t%s\n", it.Playlist, it.ID, it.InstanceID, it.InstanceName, dur); err != nil {
			return err
		}
	}
	return tw.Flush()
}
