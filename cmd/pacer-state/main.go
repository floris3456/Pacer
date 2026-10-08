// pacer-state performs an explicit first-installation operation. Normal plugin
// startup never calls Create and never treats absent policy state as an empty chat.
package main

import (
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/floris3456/cliproxyapi-quota-handoff/internal/cachepolicy"
	_ "github.com/mattn/go-sqlite3"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 || args[0] != "init" {
		return errors.New("usage: pacer-state init --state PATH --routes claude-main[,other-route]")
	}
	flags := flag.NewFlagSet("init", flag.ContinueOnError)
	path := flags.String("state", "", "Pacer SQLite state path")
	rawRoutes := flags.String("routes", "", "explicit supported provider-route IDs, comma separated")
	if err := flags.Parse(args[1:]); err != nil {
		return err
	}
	if flags.NArg() != 0 || *path == "" || *rawRoutes == "" {
		return errors.New("state and explicit routes are required")
	}
	routes := []cachepolicy.Route{}
	for _, route := range strings.Split(*rawRoutes, ",") {
		if route == "" || strings.TrimSpace(route) != route {
			return errors.New("invalid provider-route ID")
		}
		routes = append(routes, cachepolicy.Route{ProviderRoute: route, TTLs: []cachepolicy.TTL{cachepolicy.TTL5m, cachepolicy.TTL1h}})
	}
	if err := os.MkdirAll(filepath.Dir(*path), 0700); err != nil {
		return err
	}
	file, err := os.OpenFile(*path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	if err = file.Close(); err != nil {
		return err
	}
	db, err := sql.Open("sqlite3", *path+"?_journal_mode=WAL&_synchronous=FULL&_busy_timeout=5000")
	if err != nil {
		return err
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	_, err = cachepolicy.Create(context.Background(), db, cachepolicy.Options{Clock: time.Now, Routes: routes})
	return err
}
