// history-import is a trusted offline maintenance command, never a plugin route.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/floris3456/cliproxyapi-quota-handoff/internal/history"
)

func run(ctx context.Context, args []string, out io.Writer) error {
	f := flag.NewFlagSet("history-import", flag.ContinueOnError)
	// Flag parser errors can echo secret-bearing arguments. Emit only our
	// constant errors; paths, source values and arbitrary arguments stay local.
	f.SetOutput(io.Discard)
	source := f.String("source", "", "frozen read-only backup/copy; no WAL/SHM/journal")
	dest := f.String("store", "", "dedicated Pacer history database")
	manifest := f.String("manifest", "", "trusted boundary JSON file")
	pins := f.String("report-pins", "", "optional frozen JSON report/evidence pins, digest-bound in manifest")
	batch := f.Int("batch", 250, "rows per transaction, 1..1000")
	batches := f.Int("max-batches", 0, "0 completes; positive returns resumable progress")
	frozen := f.Bool("frozen", false, "attest source is a consistent, offline immutable backup")
	if f.Parse(args) != nil || f.NArg() != 0 || !*frozen || *source == "" || *dest == "" || *manifest == "" {
		return history.ErrInput
	}
	file, e := os.Open(*manifest)
	if e != nil {
		return history.ErrInput
	}
	defer file.Close()
	decoder := json.NewDecoder(io.LimitReader(file, 64<<10))
	decoder.DisallowUnknownFields()
	var b history.Boundary
	if decoder.Decode(&b) != nil {
		return history.ErrInput
	}
	var extra any
	if decoder.Decode(&extra) != io.EOF {
		return history.ErrInput
	}
	s, e := history.Open(*dest)
	if e != nil {
		return e
	}
	defer s.Close()
	p, e := s.Import(ctx, history.ImportOptions{SourcePath: *source, ReportPinsPath: *pins, Boundary: b, BatchSize: *batch, MaxBatches: *batches})
	if e != nil {
		return e
	}
	if json.NewEncoder(out).Encode(p) != nil {
		return history.ErrStorage
	}
	return nil
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if e := run(ctx, os.Args[1:], os.Stdout); e != nil {
		fmt.Fprintln(os.Stderr, e)
		os.Exit(1)
	}
}
