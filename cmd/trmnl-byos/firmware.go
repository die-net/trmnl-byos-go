package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/die-net/trmnl-byos-go/internal/firmware"
)

// firmwareDir is where firmware files are stored under the data directory.
func firmwareDir(data string) string {
	return filepath.Join(data, "firmware")
}

func firmwares(ctx context.Context, args []string, out io.Writer) error {
	return subcommand(ctx, "firmware", args, out, map[string]func(context.Context, []string, io.Writer) error{
		"add":     firmwareAdd,
		"list":    firmwareList,
		"rollout": firmwareRollout,
		"cancel":  firmwareCancel,
		"device":  firmwareDevice,
	})
}

func firmwareAdd(ctx context.Context, args []string, out io.Writer) error {
	fs := flag.NewFlagSet("firmware add", flag.ExitOnError)
	data := dataFlag(fs)
	model := fs.String("model", "", "the firmware model this build is for, the Model header its devices send, such as xiao_epaper_display")
	version := fs.String("version", "", "the build's version, such as 1.8.19; needed unless the file's descriptor holds a plain one")
	_ = fs.Parse(args) // ExitOnError exits on a parse error.
	if *model == "" || fs.NArg() != 1 {
		return errors.New("usage: trmnl-byos firmware add [-data dir] -model model [-version version] <file>")
	}
	f, err := os.Open(fs.Arg(0))
	if err != nil {
		return err
	}
	defer f.Close()
	client, err := openDB(ctx, *data)
	if err != nil {
		return err
	}
	defer client.Close()
	fw, err := firmware.Add(ctx, client, firmwareDir(*data), *model, *version, f, time.Now())
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(out, "Added firmware %d: %s %s for %s, %d bytes, SHA-256 %s.\nIt is offered to no device until you start a rollout.\n",
		fw.ID, fw.FwModel, fw.Version, fw.Chip, fw.Bytes, fw.Sha256)
	return err
}

func firmwareList(ctx context.Context, args []string, out io.Writer) error {
	fs := flag.NewFlagSet("firmware list", flag.ExitOnError)
	data := dataFlag(fs)
	_ = fs.Parse(args) // ExitOnError exits on a parse error.
	client, err := openDB(ctx, *data)
	if err != nil {
		return err
	}
	defer client.Close()
	rows, err := firmware.List(ctx, client.Read)
	if err != nil {
		return err
	}
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	if _, err := fmt.Fprintln(tw, "ID\tMODEL\tVERSION\tCHIP\tBYTES\tROLLOUT\tCANARY\tCANARY REPORTS\tLAST OFFERED\tSHA-256"); err != nil {
		return err
	}
	for i := range rows {
		r := &rows[i]
		if _, err := fmt.Fprintf(tw, "%d\t%s\t%s\t%s\t%d\t%s\t%s\t%s\t%s\t%s\n",
			r.ID, r.FwModel, r.Version, r.Chip, r.Bytes, orNone(r.Stage), orNone(r.Canary), orNone(r.CanaryVersion), offered(r.CanaryLastOfferedAt), r.Sha256); err != nil {
			return err
		}
	}
	return tw.Flush()
}

// offered formats a Unix time a build was last offered, or "-" for never.
func offered(sec int64) string {
	if sec == 0 {
		return "-"
	}
	return time.Unix(sec, 0).Format(time.DateTime)
}

func orNone(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// splitID takes the first argument as an ID when it is not a flag, so a
// command reads "rollout 3 -canary ABC123" as well as "rollout -canary
// ABC123 3". It returns the ID and the arguments left to parse as flags.
func splitID(args []string) (id string, rest []string) {
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		return args[0], args[1:]
	}
	return "", args
}

// firmwareID parses a firmware ID from the first argument, or from what
// is left after the flags.
func firmwareID(first string, fs *flag.FlagSet) (int64, error) {
	if first == "" && fs.NArg() == 1 {
		first = fs.Arg(0)
	} else if first == "" || fs.NArg() != 0 {
		return 0, errors.New("want exactly one firmware ID, from firmware list")
	}
	id, err := strconv.ParseInt(first, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%q is not a firmware ID", first)
	}
	return id, nil
}

func firmwareRollout(ctx context.Context, args []string, out io.Writer) error {
	fs := flag.NewFlagSet("firmware rollout", flag.ExitOnError)
	data := dataFlag(fs)
	canary := fs.String("canary", "", "friendly ID or MAC of the device that gets the build first")
	first, rest := splitID(args)
	_ = fs.Parse(rest) // ExitOnError exits on a parse error.
	id, err := firmwareID(first, fs)
	if err != nil || *canary == "" {
		return errors.New("usage: trmnl-byos firmware rollout [-data dir] <id> -canary <friendly ID or MAC>")
	}
	client, err := openDB(ctx, *data)
	if err != nil {
		return err
	}
	defer client.Close()
	e, err := firmware.StartRollout(ctx, client, id, *canary, time.Now())
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(out, "Rolling out %s %s. Only %s is offered it, on its next wake, until it reports version %s.\n",
		e.FwModel, e.Version, e.Canary, e.Version)
	return err
}

func firmwareCancel(ctx context.Context, args []string, out io.Writer) error {
	fs := flag.NewFlagSet("firmware cancel", flag.ExitOnError)
	data := dataFlag(fs)
	first, rest := splitID(args)
	_ = fs.Parse(rest) // ExitOnError exits on a parse error.
	id, err := firmwareID(first, fs)
	if err != nil {
		return fmt.Errorf("usage: trmnl-byos firmware cancel [-data dir] <id>: %w", err)
	}
	client, err := openDB(ctx, *data)
	if err != nil {
		return err
	}
	defer client.Close()
	if err := firmware.CancelRollout(ctx, client, id); err != nil {
		return err
	}
	_, err = fmt.Fprintf(out, "Canceled the rollout of firmware %d. No device is offered it now.\n", id)
	return err
}

// firmwareDevice sets whether a device may be offered firmware, and the
// version it is pinned to. Both are part of the firmware command, since
// they only decide what firmware a device is offered.
func firmwareDevice(ctx context.Context, args []string, out io.Writer) error {
	fs := flag.NewFlagSet("firmware device", flag.ExitOnError)
	data := dataFlag(fs)
	updates := fs.String("updates", "", "on lets the device be offered firmware, off stops it; devices start off")
	pin := fs.String("pin", "", "a version for the device to stay on, or none to clear the pin")
	first, rest := splitID(args)
	_ = fs.Parse(rest) // ExitOnError exits on a parse error.
	device := first
	if device == "" && fs.NArg() == 1 {
		device = fs.Arg(0)
	} else if fs.NArg() != 0 {
		device = ""
	}
	var p firmware.Policy
	switch *updates {
	case "":
	case "on", "off":
		on := *updates == "on"
		p.UpdatesEnabled = &on
	default:
		return fmt.Errorf("-updates %q is not on or off", *updates)
	}
	if *pin != "" {
		v := *pin
		if v == "none" {
			v = ""
		}
		p.Pin = &v
	}
	if device == "" || (p.UpdatesEnabled == nil && p.Pin == nil) {
		return errors.New("usage: trmnl-byos firmware device [-data dir] [-updates on|off] [-pin version|none] <friendly ID or MAC>")
	}
	client, err := openDB(ctx, *data)
	if err != nil {
		return err
	}
	defer client.Close()
	dev, err := firmware.SetPolicy(ctx, client, device, p)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(out, "%s: updates %s, pin %s.\n", dev.FriendlyID, onOff(dev.UpdatesEnabled != 0), orNone(dev.FwPin))
	return err
}

func onOff(on bool) string {
	if on {
		return "on"
	}
	return "off"
}
