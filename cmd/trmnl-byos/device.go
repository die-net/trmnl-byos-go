package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
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
	})
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
