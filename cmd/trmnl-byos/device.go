package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/die-net/trmnl-byos-go/internal/db"
	"github.com/die-net/trmnl-byos-go/internal/device"
)

func devices(ctx context.Context, args []string, out io.Writer) error {
	return subcommand(ctx, "device", args, out, map[string]func(context.Context, []string, io.Writer) error{
		"list":    deviceList,
		"delete":  deviceDelete,
		"block":   deviceBlock,
		"unblock": deviceUnblock,
		"blocked": deviceBlocked,
		"set":     deviceSet,
	})
}

// deviceSet changes a device's name, profile, palette, time zone, locale,
// sleep window, button command and double-click function. A flag left out
// keeps its setting, and none clears a profile, palette, time zone, locale or
// sleep window. A button command or double-click function is cleared by
// default instead, since none is a double-click function.
func deviceSet(ctx context.Context, args []string, out io.Writer) error {
	fs := flag.NewFlagSet("device set", flag.ExitOnError)
	data := dataFlag(fs)
	name := fs.String("name", "", "the device's name; empty for none")
	prof := fs.String("profile", "", "the registry model the device's panel is, such as og_bwry, or none for the one its firmware reports")
	palette := fs.String("palette", "", "the palette to render with when a plugin instance asks for none, one of the model's, or none for the model's default")
	zone := fs.String("zone", "", "the IANA time zone the device's schedules are read in and its renders are made for, such as America/Los_Angeles, or none for the server's")
	locale := fs.String("locale", "", "the locale the device's renders are made for, such as de or pt-BR, or none for the server's locale setting")
	sleep := fs.String("sleep", "", "the window in which the device is shown no playlist item, as HH:MM-HH:MM in its time zone, such as 23:00-06:30, or none for always awake")
	button := fs.String("button", "", "what a single button press does, one of "+strings.Join(device.ButtonCommands, ", ")+", or default for next")
	double := fs.String("double-click", "", "what a double-click does, one of "+strings.Join(device.SpecialFunctions, ", ")+", or default for rewind")
	rest := parseInterspersed(fs, args)
	if len(rest) != 1 {
		return errors.New("usage: trmnl-byos device set [-data dir] [-name name] [-profile model|none] [-palette id|none] [-zone name|none] [-locale code|none] [-sleep HH:MM-HH:MM|none] [-button next|previous|first|refresh|default] [-double-click none|identify|restart_playlist|rewind|sleep|add_wifi|default] <friendly ID or MAC>")
	}
	var c device.Change
	noneIsEmpty := func(v *string) *string {
		if *v == "none" {
			*v = ""
		}
		return v
	}
	defaultIsEmpty := func(v *string) *string {
		if *v == "default" {
			*v = ""
		}
		return v
	}
	fs.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "name":
			c.Name = name
		case "profile":
			c.Profile = noneIsEmpty(prof)
		case "palette":
			c.Palette = noneIsEmpty(palette)
		case "zone":
			c.TimeZone = noneIsEmpty(zone)
		case "locale":
			c.Locale = noneIsEmpty(locale)
		case "sleep":
			c.Sleep = noneIsEmpty(sleep)
		case "button":
			c.ButtonCommand = defaultIsEmpty(button)
		case "double-click":
			c.SpecialFunction = defaultIsEmpty(double)
		}
	})
	if c.Name == nil && c.Profile == nil && c.Palette == nil && c.TimeZone == nil && c.Locale == nil && c.Sleep == nil && c.ButtonCommand == nil && c.SpecialFunction == nil {
		return errors.New("device set: give -name, -profile, -palette, -zone, -locale, -sleep, -button or -double-click")
	}
	client, err := openDB(ctx, *data)
	if err != nil {
		return err
	}
	defer client.Close()
	dev, err := device.Update(ctx, client, rest[0], c)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(out, "%s: name %s, profile %s, palette %s, zone %s, locale %s, sleep %s, button %s, double-click %s.\n", dev.FriendlyID,
		orNone(dev.Name), orNone(dev.ProfileOverride), orNone(dev.PaletteOverride), orNone(dev.TimeZone), orNone(dev.Locale),
		orNone(device.FormatSleep(dev.SleepStart, dev.SleepEnd)), device.ButtonCommandOf(dev.ButtonCommand), device.SpecialFunctionOf(dev.SpecialFunction))
	return err
}

func deviceList(ctx context.Context, args []string, out io.Writer) error {
	fs := flag.NewFlagSet("device list", flag.ExitOnError)
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
	if _, err := fmt.Fprintln(tw, "ID\tMAC\tMODEL\tFIRMWARE\tLAST SEEN\tBATTERY\tRSSI\tWAKE\tBLOCKED"); err != nil {
		return err
	}
	for i := range rows {
		r, d := &rows[i], &rows[i].Device
		blocked := ""
		if r.Blocked != 0 {
			blocked = "yes"
		}
		_, err := fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			d.FriendlyID, d.Mac, d.FwModel, d.FwVersion,
			lastSeen(r.LastSeen), orDash(r.BatteryV, "%.2f V"), orDash(r.Rssi, "%d dBm"), orDash(r.WakeTimeMs, "%d ms"), blocked)
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

// deviceArg runs a device subcommand that takes the -data flag and one
// device, a friendly ID or MAC. It opens the database and calls act with the
// device, which prints its result to out.
func deviceArg(ctx context.Context, args []string, out io.Writer, name string,
	act func(ctx context.Context, client *db.Client, dev string, out io.Writer) error,
) error {
	fs := flag.NewFlagSet(name, flag.ExitOnError)
	data := dataFlag(fs)
	_ = fs.Parse(args) // ExitOnError exits on a parse error.
	if fs.NArg() != 1 {
		return fmt.Errorf("usage: trmnl-byos %s [-data dir] <friendly ID or MAC>", name)
	}
	client, err := openDB(ctx, *data)
	if err != nil {
		return err
	}
	defer client.Close()
	return act(ctx, client, fs.Arg(0), out)
}

func deviceDelete(ctx context.Context, args []string, out io.Writer) error {
	return deviceArg(ctx, args, out, "device delete", func(ctx context.Context, client *db.Client, dev string, out io.Writer) error {
		d, err := device.Delete(ctx, client, dev, nil, time.Now())
		if err != nil {
			return err
		}
		_, err = fmt.Fprintf(out, "Deleted %s (%s). Its next request resets it into setup.\n", d.FriendlyID, d.Mac)
		return err
	})
}

func deviceBlock(ctx context.Context, args []string, out io.Writer) error {
	return deviceArg(ctx, args, out, "device block", func(ctx context.Context, client *db.Client, dev string, out io.Writer) error {
		b, err := device.Block(ctx, client, dev, nil, time.Now())
		if err != nil {
			return err
		}
		_, err = fmt.Fprintf(out, "Blocked %s.\n", b.Mac)
		return err
	})
}

func deviceUnblock(ctx context.Context, args []string, out io.Writer) error {
	return deviceArg(ctx, args, out, "device unblock", func(ctx context.Context, client *db.Client, dev string, out io.Writer) error {
		mac, err := device.Unblock(ctx, client, dev, nil)
		if err != nil {
			return err
		}
		_, err = fmt.Fprintf(out, "Unblocked %s.\n", mac)
		return err
	})
}

func deviceBlocked(ctx context.Context, args []string, out io.Writer) error {
	fs := flag.NewFlagSet("device blocked", flag.ExitOnError)
	data := dataFlag(fs)
	_ = fs.Parse(args) // ExitOnError exits on a parse error.
	if fs.NArg() != 0 {
		return errors.New("usage: trmnl-byos device blocked [-data dir]")
	}
	client, err := openDB(ctx, *data)
	if err != nil {
		return err
	}
	defer client.Close()
	rows, err := device.ListBlocked(ctx, client.Read)
	if err != nil {
		return err
	}
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	if _, err := fmt.Fprintln(tw, "MAC\tDEVICE\tNAME\tBLOCKED"); err != nil {
		return err
	}
	for i := range rows {
		r := &rows[i]
		if _, err := fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", r.Mac, orDash(r.FriendlyID, "%s"), orDash(r.Name, "%s"), time.Unix(r.BlockedAt, 0).Format(time.DateTime)); err != nil {
			return err
		}
	}
	return tw.Flush()
}
