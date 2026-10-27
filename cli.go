package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"
)

const cliUsage = `usage:
  psbin [-addr A] [-db DSN] [-log text|json]   run the server
  psbin ls  [-n N] [-db DSN]    table of stored pastes (id, time, start of text)
  psbin cat [-db DSN] <id>      print one paste in full
  psbin help                              show this message

ls and cat read the store directly, no server needed. Pastes are encrypted; the key
is in .env next to the paste file (see README). -db / $DATABASE_URL is only for a
server you pointed at Postgres.`

// msgLen is how many characters of a paste the ls table shows.
const msgLen = 60

func runCLI(cmd string, args []string) {
	fs := flag.NewFlagSet(cmd, flag.ExitOnError)
	db := fs.String("db", os.Getenv("DATABASE_URL"), "Postgres connection string (only if the server uses Postgres)")
	n := fs.Int("n", 0, "how many pastes to list, newest first; 0 = all (ls)")
	fs.Parse(args)

	var err error
	switch cmd {
	case "ls":
		err = cliLs(*db, *n)
	case "cat":
		if fs.NArg() != 1 {
			fmt.Fprintln(os.Stderr, cliUsage)
			os.Exit(2)
		}
		err = cliCat(*db, fs.Arg(0))
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "psbin:", err)
		os.Exit(1)
	}
}

func cliLs(db string, n int) error {
	store, where, err := openStore(db, true)
	if err != nil {
		return err
	}
	defer store.Close()
	if n <= 0 {
		n = 1<<31 - 1
	}
	pastes, err := store.Recent(context.Background(), n)
	if err != nil {
		return err
	}
	// stderr, so piping the table elsewhere stays clean
	fmt.Fprintf(os.Stderr, "%d pastes in %s\n", len(pastes), where)
	if len(pastes) == 0 {
		return nil
	}

	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tCREATED\tMESSAGE")
	for _, p := range pastes {
		// collapse newlines/tabs so each paste stays on one row
		msg := []rune(strings.Join(strings.Fields(p.Content), " "))
		text := string(msg)
		if len(msg) > msgLen {
			text = string(msg[:msgLen]) + "…"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\n", p.ID, p.CreatedAt.Local().Format("02 Jan 15:04"), text)
	}
	return tw.Flush()
}

func cliCat(db, id string) error {
	store, where, err := openStore(db, true)
	if err != nil {
		return err
	}
	defer store.Close()
	p, err := store.Get(context.Background(), id)
	if errors.Is(err, errNotFound) {
		return fmt.Errorf("no paste %q in %s", id, where)
	}
	if err != nil {
		return err
	}
	_, err = io.WriteString(os.Stdout, p.Content)
	return err
}
