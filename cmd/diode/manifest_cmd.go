package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/degreane/datadiode/internal/manifest"
)

func runManifest(_ context.Context, args []string) error {
	fs := flag.NewFlagSet("diode --mode=manifest", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, `usage: diode --mode=manifest [flags]

Print the sender's transfer history (records from <sender-state>/manifest.jsonl).

flags:`)
		fs.PrintDefaults()
	}
	state := fs.String("sender-state", defaultSenderStateDir(), "sender state dir")
	format := fs.String("format", "table", "output format: \"table\" (human), \"json\" (raw JSONL), \"tsv\" (tab-separated)")
	since := fs.String("since", "", "filter to records completed within this duration (e.g. \"24h\", \"7d\")")
	sid := fs.String("session-id", "", "filter to records matching this session_id")
	if err := fs.Parse(args); err != nil {
		return err
	}

	path := filepath.Join(*state, "manifest.jsonl")
	records, err := manifest.ReadAll(path)
	if err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}

	if *since != "" {
		cutoff, err := parseSince(*since)
		if err != nil {
			return err
		}
		records = manifest.FilterSince(records, cutoff)
	}
	if *sid != "" {
		var kept []manifest.Record
		for _, r := range records {
			if r.SessionID == *sid {
				kept = append(kept, r)
			}
		}
		records = kept
	}

	switch *format {
	case "table":
		fmt.Print(manifest.FormatTable(records))
		if len(records) == 0 {
			fmt.Println("(no records)")
		}
	case "json":
		enc := json.NewEncoder(os.Stdout)
		for _, r := range records {
			if err := enc.Encode(r); err != nil {
				return err
			}
		}
	case "tsv":
		fmt.Println("session_id\tcompleted_at\tfilename\tbytes\tdestination\ttx_status")
		for _, r := range records {
			fmt.Printf("%s\t%s\t%s\t%d\t%s\t%s\n",
				r.SessionID, r.CompletedAt.UTC().Format(time.RFC3339),
				r.Filename, r.TotalBytes, r.Destination, r.TxStatus)
		}
	default:
		return fmt.Errorf("--format must be table, json, or tsv; got %q", *format)
	}
	return nil
}

// parseSince accepts a Go duration ("24h", "1500ms") plus a couple of
// human shorthands: "d" = day, "w" = week. Returns the cutoff time
// (now - duration).
func parseSince(s string) (time.Time, error) {
	if s == "" {
		return time.Time{}, errors.New("empty duration")
	}
	// Try Go's parser first.
	if d, err := time.ParseDuration(s); err == nil {
		return time.Now().UTC().Add(-d), nil
	}
	// Fallback: NNd or NNw.
	if len(s) >= 2 {
		unit := s[len(s)-1]
		num := s[:len(s)-1]
		n, err := strconv.Atoi(num)
		if err == nil {
			switch unit {
			case 'd':
				return time.Now().UTC().Add(-time.Duration(n) * 24 * time.Hour), nil
			case 'w':
				return time.Now().UTC().Add(-time.Duration(n) * 7 * 24 * time.Hour), nil
			}
		}
	}
	return time.Time{}, fmt.Errorf("--since %q: not a duration like \"24h\", \"7d\", or \"2w\"", strings.TrimSpace(s))
}
