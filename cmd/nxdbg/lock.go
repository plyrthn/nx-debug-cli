package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/plyrthn/nx-debug-cli/internal/edevlock"
)

// The lock group formalizes a convention agents were already following by
// hand when two sessions share one devkit: before an exclusive action
// (serve, reboot, install, USB open, gdb/debug attach), check the lock file
// at %TEMP%\edev_<serial>.lock; if another session wrote it recently, hold
// off; otherwise write your own and leave a message the other session can
// read later. This just gives that file a real reader/writer instead of
// hand-editing it.

var lockGroup = group{"lock", []subcommand{
	{"status", func(ctx context.Context, r []string) error {
		return cmdLockStatus(r[1])
	}},
	{"acquire", func(ctx context.Context, r []string) error {
		return cmdLockAcquire(r[1], r[2:])
	}},
	{"release", func(ctx context.Context, r []string) error {
		return cmdLockRelease(r[1], r[2:])
	}},
}}

func cmdLockStatus(serial string) error {
	l, err := edevlock.Read(serial)
	if err != nil {
		return err
	}
	if l.Session == "" {
		fmt.Printf("no lock held on %s\n", serial)
		return nil
	}
	age := time.Since(l.Written)
	state := "held"
	if edevlock.Stale(l, time.Now()) {
		state = "stale"
	}
	fmt.Printf("%s: %s by %s (%s ago, written %s)\n",
		serial, state, l.Session, age.Round(time.Second), l.Written.Format(time.RFC3339))
	if l.Message != "" {
		fmt.Println(l.Message)
	}
	return nil
}

// cmdLockAcquire writes the lock, refusing to overwrite an unexpired lock
// held by a different session unless --force is given.
func cmdLockAcquire(serial string, rest []string) error {
	session, message, force, err := parseLockArgs(rest, "usage: nxdbg lock acquire <serial> <session> [message] [--force]")
	if err != nil {
		return err
	}
	existing, err := edevlock.Read(serial)
	if err != nil {
		return err
	}
	if !force && existing.Session != "" && existing.Session != session && !edevlock.Stale(existing, time.Now()) {
		return fmt.Errorf("%s is held by %s as of %s ago (use --force to override): %s",
			serial, existing.Session, time.Since(existing.Written).Round(time.Second), existing.Message)
	}
	// A stale lock from a different session is fine to take, but its message
	// is about to be overwritten and gone for good, so it's worth surfacing
	// on the way past rather than silently discarding whatever the previous
	// session left behind for whoever came next.
	if existing.Session != "" && existing.Session != session && existing.Message != "" {
		fmt.Printf("overwriting %s's message from %s ago: %s\n",
			existing.Session, time.Since(existing.Written).Round(time.Second), existing.Message)
	}
	l := edevlock.Lock{Session: session, Written: time.Now(), Message: message}
	if err := edevlock.Write(serial, l); err != nil {
		return err
	}
	fmt.Printf("✓ %s locked by %s\n", serial, session)
	return nil
}

// cmdLockRelease clears the lock, refusing to clear a different session's
// unless --force is given.
func cmdLockRelease(serial string, rest []string) error {
	session, _, force, err := parseLockArgs(rest, "usage: nxdbg lock release <serial> <session> [--force]")
	if err != nil {
		return err
	}
	existing, err := edevlock.Read(serial)
	if err != nil {
		return err
	}
	if existing.Session == "" {
		fmt.Printf("%s was not locked\n", serial)
		return nil
	}
	if !force && existing.Session != session {
		return fmt.Errorf("%s is held by %s, not %s (use --force to override)", serial, existing.Session, session)
	}
	if err := edevlock.Clear(serial); err != nil {
		return err
	}
	fmt.Printf("✓ %s released\n", serial)
	return nil
}

// parseLockArgs splits a --force flag out of the remaining words, leaving the
// session name and whatever's left joined back into one message.
func parseLockArgs(rest []string, usage string) (session, message string, force bool, err error) {
	var words []string
	for _, w := range rest {
		if w == "--force" {
			force = true
			continue
		}
		words = append(words, w)
	}
	if len(words) == 0 {
		return "", "", false, errors.New(usage)
	}
	session = words[0]
	message = strings.Join(words[1:], " ")
	return session, message, force, nil
}
