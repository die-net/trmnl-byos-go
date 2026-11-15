package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"text/tabwriter"
	"time"

	"github.com/die-net/trmnl-byos-go/internal/apitoken"
)

func tokens(ctx context.Context, args []string, out io.Writer) error {
	return subcommand(ctx, "token", args, out, map[string]func(context.Context, []string, io.Writer) error{
		"add":    tokenAdd,
		"list":   tokenList,
		"remove": tokenRemove,
	})
}

func tokenAdd(ctx context.Context, args []string, out io.Writer) error {
	fs := flag.NewFlagSet("token add", flag.ExitOnError)
	data := dataFlag(fs)
	name := fs.String("name", "", "name of the token, such as the script that uses it")
	_ = fs.Parse(args) // ExitOnError exits on a parse error.
	if *name == "" || fs.NArg() != 0 {
		return errors.New("usage: trmnl-byos token add [-data dir] -name name")
	}
	client, err := openDB(ctx, *data)
	if err != nil {
		return err
	}
	defer client.Close()
	token, err := apitoken.Create(ctx, client, *name, time.Now())
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(out, "Created token %q. It is shown only now:\n%s\n", *name, token)
	return err
}

func tokenList(ctx context.Context, args []string, out io.Writer) error {
	fs := flag.NewFlagSet("token list", flag.ExitOnError)
	data := dataFlag(fs)
	_ = fs.Parse(args) // ExitOnError exits on a parse error.
	client, err := openDB(ctx, *data)
	if err != nil {
		return err
	}
	defer client.Close()
	rows, err := client.Read.ListTokens(ctx)
	if err != nil {
		return fmt.Errorf("list tokens: %w", err)
	}
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	if _, err := fmt.Fprintln(tw, "NAME\tCREATED\tLAST USED"); err != nil {
		return err
	}
	for i := range rows {
		r := &rows[i]
		used := "never"
		if r.LastUsedAt > 0 {
			used = time.Unix(r.LastUsedAt, 0).Format(time.DateTime)
		}
		if _, err := fmt.Fprintf(tw, "%s\t%s\t%s\n", r.Name, time.Unix(r.CreatedAt, 0).Format(time.DateTime), used); err != nil {
			return err
		}
	}
	return tw.Flush()
}

func tokenRemove(ctx context.Context, args []string, out io.Writer) error {
	return oneArgCommand(ctx, args, out, "token remove", "name", apitoken.Delete, "Removed token %q.\n")
}
