package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"text/tabwriter"
	"time"

	"github.com/die-net/trmnl-byos-go/internal/settings"
)

func setting(ctx context.Context, args []string, out io.Writer) error {
	return subcommand(ctx, "setting", args, out, map[string]func(context.Context, []string, io.Writer) error{
		"list":  settingList,
		"set":   settingSet,
		"reset": settingReset,
	})
}

func settingList(ctx context.Context, args []string, out io.Writer) error {
	fs := flag.NewFlagSet("setting list", flag.ExitOnError)
	data := dataFlag(fs)
	_ = fs.Parse(args) // ExitOnError exits on a parse error.
	if fs.NArg() != 0 {
		return errors.New("usage: trmnl-byos setting list [-data dir]")
	}
	client, err := openDB(ctx, *data)
	if err != nil {
		return err
	}
	defer client.Close()
	rows, err := settings.List(ctx, client.Read)
	if err != nil {
		return fmt.Errorf("list settings: %w", err)
	}
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	if _, err := fmt.Fprintln(tw, "KEY\tVALUE\tDEFAULT\tIS DEFAULT"); err != nil {
		return err
	}
	for _, r := range rows {
		isDefault := "yes"
		if r.Set {
			isDefault = "no"
		}
		if _, err := fmt.Fprintf(tw, "%s\t%d\t%d\t%s\n", r.Key, r.Value, r.Default, isDefault); err != nil {
			return err
		}
	}
	return tw.Flush()
}

func settingSet(ctx context.Context, args []string, out io.Writer) error {
	fs := flag.NewFlagSet("setting set", flag.ExitOnError)
	data := dataFlag(fs)
	_ = fs.Parse(args) // ExitOnError exits on a parse error.
	if fs.NArg() != 2 {
		return errors.New("usage: trmnl-byos setting set [-data dir] <key> <value>")
	}
	client, err := openDB(ctx, *data)
	if err != nil {
		return err
	}
	defer client.Close()
	key, value := fs.Arg(0), fs.Arg(1)
	if err := settings.Set(ctx, client, key, value, time.Now()); err != nil {
		return err
	}
	_, err = fmt.Fprintf(out, "Set %s to %s.\n", key, value)
	return err
}

func settingReset(ctx context.Context, args []string, out io.Writer) error {
	return oneArgCommand(ctx, args, out, "setting reset", "key", settings.Reset, "Reset %s to its default.\n")
}
