package app

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"codeberg.org/chrberger/scimux/internal/backend"
)

const stopCmd = "stop"

func runStopMain(args []string, stdout, stderr io.Writer) int {
	home, err := os.UserHomeDir()
	if err != nil {
		fmt.Fprintln(stderr, "scimux stop:", err)
		return 1
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	return runStopCommand(ctx, args, home, stdout, stderr)
}

func runStopCommand(ctx context.Context, args []string, home string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("scimux stop", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		fmt.Fprintln(stderr, appSummary)
		fmt.Fprintln(stderr)
		fmt.Fprintln(stderr, "Usage: scimux stop [options]")
		fmt.Fprintln(stderr)
		fs.PrintDefaults()
	}
	data := fs.String("data", filepath.Join(home, ".scimux"), "data directory of the muxer to stop")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintf(stderr, "scimux stop: unexpected argument %q\n", fs.Arg(0))
		fs.Usage()
		return 2
	}
	link, err := backend.Discover(*data)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			fmt.Fprintf(stderr, "scimux stop: no running muxer for data directory %s\n", *data)
		} else {
			fmt.Fprintln(stderr, "scimux stop:", err)
		}
		return 1
	}
	client, err := backend.NewClient(link)
	if err != nil {
		fmt.Fprintln(stderr, "scimux stop:", err)
		return 1
	}
	defer client.Close()
	if err := client.RequestStop(ctx); err != nil {
		fmt.Fprintln(stderr, "scimux stop: request stop:", err)
		return 1
	}
	if err := waitForMuxerStop(ctx, *data, link); err != nil {
		fmt.Fprintln(stderr, "scimux stop: wait for muxer:", err)
		return 1
	}
	fmt.Fprintln(stdout, "scimux: stopped")
	return 0
}

func waitForMuxerStop(ctx context.Context, dataDir string, stopped backend.Link) error {
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		current, err := backend.Discover(dataDir)
		if errors.Is(err, os.ErrNotExist) || err == nil && current != stopped {
			return nil
		}
		if err != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}
