package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/floris3456/cliproxyapi-quota-handoff/internal/history"
	"github.com/floris3456/cliproxyapi-quota-handoff/internal/historyreports"
)

func prepareReports(args []string, out io.Writer) error {
	f := flag.NewFlagSet("prepare-reports", flag.ContinueOnError)
	f.SetOutput(io.Discard)
	archive := f.String("archive", "", "existing published Pacer history archive")
	dir := f.String("output-dir", "", "existing private directory for the owned calculation artifact")
	interval := f.Int64("poll-interval-seconds", 300, "frozen source's poll interval; this command does not poll")
	if f.Parse(args) != nil || f.NArg() != 0 || *archive == "" || *dir == "" || *interval < 60 || *interval > 86400 {
		return errors.New("prepare-reports requires an existing archive, output directory and interval between 60 and 86400 seconds")
	}
	info, err := os.Lstat(*archive)
	if err != nil || !info.Mode().IsRegular() {
		return errors.New("report archive must already exist as a regular file")
	}
	info, err = os.Lstat(*dir)
	if err != nil || !info.IsDir() {
		return errors.New("report output directory must already exist")
	}
	store, err := history.Open(*archive)
	if err != nil {
		return errors.New("report archive is not a compatible Pacer archive")
	}
	defer store.Close()
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	reader, err := historyreports.Prepare(ctx, store, *dir, historyreports.PrepareOptions{PollIntervalSeconds: *interval})
	if err != nil {
		return errors.New("report preparation failed; verify published history, frozen evidence and resource ceilings")
	}
	defer reader.Close()
	return json.NewEncoder(out).Encode(map[string]any{"artifact_path": reader.ArtifactPath(), "default_pin": reader.DefaultPin(), "scope": "Frozen historical calculations only; does not activate routing or retire CPAMP"})
}
