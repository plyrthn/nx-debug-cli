package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strconv"
	"time"

	"github.com/plyrthn/nx-debug-cli/internal/htc"
)

// cmdWatchLog prints the target's log as it arrives, with no daemon involved.
//
// The daemon's own log server reads this same stream; the difference is that it
// then republishes it on host-side ports for other tools, where this just
// decodes and prints. Run it under `nxdbg serve`.
func cmdWatchLog(ctx context.Context, serial string, rest []string) error {
	seconds := 0
	if len(rest) > 0 {
		n, err := strconv.Atoi(rest[0])
		if err != nil {
			return fmt.Errorf("invalid seconds %q: %w", rest[0], err)
		}
		seconds = n
	}
	if seconds > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, time.Duration(seconds)*time.Second)
		defer cancel()
	}

	reader, conn, err := htc.DialTargetLog(ctx, serial)
	if err != nil {
		return err
	}
	defer conn.Close()

	// Closing the connection is what unblocks the read, since the decoder is
	// sitting in a blocking ReadFull rather than polling.
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt)
	defer signal.Stop(sig)
	go func() {
		select {
		case <-sig:
		case <-ctx.Done():
		}
		conn.Close()
	}()

	if seconds > 0 {
		fmt.Printf("reading the target log for %ds (ctrl-c to stop)\n", seconds)
	} else {
		fmt.Println("reading the target log (ctrl-c to stop)")
	}

	records := 0
	for {
		rec, err := reader.Next()
		if err != nil {
			if records == 0 && errors.Is(err, io.EOF) {
				fmt.Println("the target closed the stream without sending anything")
				return nil
			}
			break
		}
		records++
		fmt.Println(rec)
	}
	fmt.Printf("\n%d records", records)
	if reader.Dropped > 0 {
		fmt.Printf(", %d packets skipped (the stream was joined mid-record)", reader.Dropped)
	}
	fmt.Println()
	return nil
}
