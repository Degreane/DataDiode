package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/degreane/datadiode/internal/manifest"
)

func runVacuum(_ context.Context, args []string) error {
	fs := flag.NewFlagSet("diode --mode=vacuum", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, `usage: diode --mode=vacuum --age=<minutes> (--spool <dir> | --sender-state <dir>) [flags]

Removes spool sessions, archived snapshots, and manifest entries older
than --age minutes. Designed to be run from cron.

flags:`)
		fs.PrintDefaults()
	}
	ageMin := fs.Int("age", 0, "max age in minutes; anything older than (now - age) is removed (required)")
	spool := fs.String("spool", "", "receiver spool dir to clean (skip if empty)")
	state := fs.String("sender-state", "", "sender state dir to clean (skip if empty)")
	dryRun := fs.Bool("dry-run", false, "print actions, perform nothing")
	verbose := fs.Bool("verbose", false, "print every action taken")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *ageMin <= 0 {
		return errors.New("--age must be > 0 (minutes)")
	}
	if *spool == "" && *state == "" {
		return errors.New("at least one of --spool or --sender-state must be set")
	}
	cutoff := time.Now().UTC().Add(-time.Duration(*ageMin) * time.Minute)
	fmt.Fprintf(os.Stderr, "diode vacuum: removing entries older than %v (cutoff=%s) dry-run=%v\n",
		time.Duration(*ageMin)*time.Minute, cutoff.Format(time.RFC3339), *dryRun)

	var totalRemoved int
	if *spool != "" {
		n, err := vacuumSpool(*spool, cutoff, *dryRun, *verbose)
		if err != nil {
			return fmt.Errorf("spool: %w", err)
		}
		totalRemoved += n
		fmt.Fprintf(os.Stderr, "diode vacuum: spool removed %d session dirs\n", n)
	}
	if *state != "" {
		n, err := vacuumSenderState(*state, cutoff, *dryRun, *verbose)
		if err != nil {
			return fmt.Errorf("sender-state: %w", err)
		}
		totalRemoved += n
		fmt.Fprintf(os.Stderr, "diode vacuum: sender-state removed %d archive files + pruned manifest\n", n)
	}
	fmt.Fprintf(os.Stderr, "diode vacuum: done. total removed = %d\n", totalRemoved)
	return nil
}

// vacuumSpool removes each session subdirectory whose newest file is
// older than cutoff. Returns the count of session dirs removed.
func vacuumSpool(dir string, cutoff time.Time, dryRun, verbose bool) (int, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	removed := 0
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		sd := filepath.Join(dir, e.Name())
		newest, err := newestMTime(sd)
		if err != nil {
			fmt.Fprintf(os.Stderr, "diode vacuum: skip %s: %v\n", sd, err)
			continue
		}
		if !newest.Before(cutoff) {
			if verbose {
				fmt.Fprintf(os.Stderr, "diode vacuum: keep %s (newest=%s)\n", sd, newest.Format(time.RFC3339))
			}
			continue
		}
		if verbose || dryRun {
			fmt.Fprintf(os.Stderr, "diode vacuum: remove session %s (newest=%s)\n", sd, newest.Format(time.RFC3339))
		}
		if !dryRun {
			if err := os.RemoveAll(sd); err != nil {
				fmt.Fprintf(os.Stderr, "diode vacuum: rm %s: %v\n", sd, err)
				continue
			}
		}
		removed++
	}
	return removed, nil
}

// vacuumSenderState removes archive snapshots and prunes the manifest.
func vacuumSenderState(dir string, cutoff time.Time, dryRun, verbose bool) (int, error) {
	archive := filepath.Join(dir, "sent")
	removed := 0
	entries, err := os.ReadDir(archive)
	if err == nil {
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			path := filepath.Join(archive, e.Name())
			st, err := os.Stat(path)
			if err != nil {
				continue
			}
			if !st.ModTime().Before(cutoff) {
				if verbose {
					fmt.Fprintf(os.Stderr, "diode vacuum: keep %s (mtime=%s)\n", path, st.ModTime().Format(time.RFC3339))
				}
				continue
			}
			if verbose || dryRun {
				fmt.Fprintf(os.Stderr, "diode vacuum: remove archive %s\n", path)
			}
			if !dryRun {
				if err := os.Remove(path); err != nil {
					fmt.Fprintf(os.Stderr, "diode vacuum: rm %s: %v\n", path, err)
					continue
				}
			}
			removed++
		}
	} else if !os.IsNotExist(err) {
		return removed, err
	}

	manifestPath := filepath.Join(dir, "manifest.jsonl")
	if _, err := os.Stat(manifestPath); err == nil {
		if dryRun {
			recs, _ := manifest.ReadAll(manifestPath)
			drop := 0
			for _, r := range recs {
				if r.CompletedAt.Before(cutoff) {
					drop++
				}
			}
			fmt.Fprintf(os.Stderr, "diode vacuum: manifest would drop %d records (dry-run)\n", drop)
		} else {
			dropped, err := manifest.PruneOlderThan(manifestPath, cutoff)
			if err != nil {
				return removed, fmt.Errorf("manifest prune: %w", err)
			}
			if dropped > 0 {
				fmt.Fprintf(os.Stderr, "diode vacuum: manifest dropped %d records\n", dropped)
			}
		}
	}
	return removed, nil
}

// newestMTime returns the most recent modification time among files in dir.
func newestMTime(dir string) (time.Time, error) {
	var newest time.Time
	err := filepath.Walk(dir, func(_ string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.ModTime().After(newest) {
			newest = info.ModTime()
		}
		return nil
	})
	return newest, err
}
