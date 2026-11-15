package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"golang.org/x/term"

	"github.com/die-net/trmnl-byos-go/internal/adminauth"
	"github.com/die-net/trmnl-byos-go/internal/settings"
)

// password sets the admin password for the web UI. At a terminal it asks
// twice, without echo. Otherwise it reads one line from stdin, for a script.
// It never prints the password.
func password(ctx context.Context, args []string, in *os.File, out io.Writer) error {
	fs := flag.NewFlagSet("password", flag.ExitOnError)
	data := dataFlag(fs)
	_ = fs.Parse(args) // ExitOnError exits on a parse error.
	if fs.NArg() != 0 {
		return errors.New("usage: trmnl-byos password [-data dir]")
	}
	pw, err := readPassword(in, out)
	if err != nil {
		return err
	}
	client, err := openDB(ctx, *data)
	if err != nil {
		return err
	}
	defer client.Close()
	if err := adminauth.SetPassword(ctx, client, pw, time.Now()); err != nil {
		return err
	}
	_, err = fmt.Fprintln(out, "Admin password set. All sessions were signed out.")
	return err
}

// maxPasswordLine bounds the line read from a pipe: a password, and room for
// a line ending, with some slack so an over-long one is refused as too long
// rather than cut to a valid one.
const maxPasswordLine = settings.MaxPasswordLen + 16

// readPassword reads a new password from in. For a terminal it prompts on out
// and asks twice. For anything else it takes the first line, without its
// line ending.
func readPassword(in *os.File, out io.Writer) (string, error) {
	fd := int(in.Fd())
	if !term.IsTerminal(fd) {
		line, err := bufio.NewReader(io.LimitReader(in, maxPasswordLine)).ReadString('\n')
		if err != nil && (!errors.Is(err, io.EOF) || line == "") {
			return "", fmt.Errorf("read password from stdin: %w", err)
		}
		return strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\r"), nil
	}
	ask := func(prompt string) (string, error) {
		if _, err := fmt.Fprint(out, prompt); err != nil {
			return "", err
		}
		b, err := term.ReadPassword(fd)
		// The terminal did not echo the Enter.
		_, _ = fmt.Fprintln(out)
		if err != nil {
			return "", fmt.Errorf("read password: %w", err)
		}
		return string(b), nil
	}
	first, err := ask("New password: ")
	if err != nil {
		return "", err
	}
	again, err := ask("Again: ")
	if err != nil {
		return "", err
	}
	if first != again {
		return "", errors.New("the two passwords differ")
	}
	return first, nil
}
