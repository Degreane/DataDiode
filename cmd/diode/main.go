// Command diode is the DataDiode CLI. A single binary selects its role
// at startup via --mode:
//
//	diode --mode=tx [flags]   one-way sender   (stdin → frames → UDP)
//	diode --mode=rx [flags]   one-way receiver (UDP → verify → stdout/file)
//
// One binary keeps distribution simple (one file to push into both LXC
// containers). The diode discipline — that the receiver never sends —
// is enforced inside the rx code path (via udp.Receiver, which exposes
// no Write methods) and at the network layer by the host firewall (see
// scripts/).
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
)

// version, commit, buildDate are injected at link time by the Makefile's
// -ldflags="-X main.version=... -X main.commit=... -X main.buildDate=...".
// They MUST be vars (not consts) for -X to work.
var (
	version   = "0.0.0-dev"
	commit    = "unknown"
	buildDate = "unknown"
)

const (
	usage = `diode — software unidirectional gateway

usage:
  diode --mode=tx       [flags]   one-way sender   (file → SOH+chunks → UDP)
  diode --mode=rx       [flags]   one-way receiver (UDP → verify → file)
  diode --mode=manifest [flags]   print the sender's transfer history
  diode --mode=vacuum   [flags]   prune old spool / sent / manifest entries
  diode --version                 print version and exit
  diode --help                    show this help

Run "diode --mode=<mode> --help" for mode-specific flags.
`
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	mode, rest, err := extractMode(os.Args[1:])
	if err != nil {
		fmt.Fprintf(os.Stderr, "diode: %v\n\n%s", err, usage)
		os.Exit(2)
	}

	switch mode {
	case "tx":
		if err := runTx(ctx, rest); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				return // mode flag-set already printed usage
			}
			fmt.Fprintf(os.Stderr, "diode --mode=tx: %v\n", err)
			os.Exit(1)
		}
	case "rx":
		if err := runRx(ctx, rest); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				return
			}
			fmt.Fprintf(os.Stderr, "diode --mode=rx: %v\n", err)
			os.Exit(1)
		}
	case "manifest":
		if err := runManifest(ctx, rest); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				return
			}
			fmt.Fprintf(os.Stderr, "diode --mode=manifest: %v\n", err)
			os.Exit(1)
		}
	case "vacuum":
		if err := runVacuum(ctx, rest); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				return
			}
			fmt.Fprintf(os.Stderr, "diode --mode=vacuum: %v\n", err)
			os.Exit(1)
		}
	case "version":
		fmt.Printf("diode %s (commit %s, built %s)\n", version, commit, buildDate)
	case "help":
		fmt.Fprint(os.Stdout, usage)
	default:
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
}

// extractMode pre-parses argv for --mode / --version / --help and
// returns the selected mode plus the remaining args (which are then
// handed to the mode-specific flag parser).
//
// We pre-parse instead of using a top-level flag.FlagSet because the
// stdlib `flag` package errors on unknown flags, and mode-specific
// flags will be unknown at the top level.
func extractMode(args []string) (mode string, rest []string, err error) {
	rest = make([]string, 0, len(args))
	seen := false
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		// --help / --version are only intercepted at the top level.
		// Once --mode has been set, pass them through so the mode-
		// specific flag parser can show mode-specific usage.
		case !seen && (a == "--help" || a == "-h" || a == "help"):
			return "help", nil, nil
		case !seen && (a == "--version" || a == "-v" || a == "version"):
			return "version", nil, nil
		case strings.HasPrefix(a, "--mode=") || strings.HasPrefix(a, "-mode="):
			if seen {
				return "", nil, fmt.Errorf("--mode specified more than once")
			}
			seen = true
			mode = a[strings.IndexByte(a, '=')+1:]
		case a == "--mode" || a == "-mode":
			if seen {
				return "", nil, fmt.Errorf("--mode specified more than once")
			}
			if i+1 >= len(args) {
				return "", nil, fmt.Errorf("--mode requires a value (tx|rx)")
			}
			seen = true
			mode = args[i+1]
			i++
		default:
			rest = append(rest, a)
		}
	}
	if !seen {
		return "", nil, fmt.Errorf("--mode is required (tx|rx|manifest|vacuum)")
	}
	switch mode {
	case "tx", "rx", "manifest", "vacuum":
		// ok
	default:
		return "", nil, fmt.Errorf("--mode must be tx, rx, manifest, or vacuum; got %q", mode)
	}
	return mode, rest, nil
}
