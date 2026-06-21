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

const (
	version = "0.0.0-dev"
	usage   = `diode — software unidirectional gateway

usage:
  diode --mode=tx [flags]   one-way sender   (stdin → frames → UDP)
  diode --mode=rx [flags]   one-way receiver (UDP → verify → stdout/file)
  diode --version           print version and exit
  diode --help              show this help

Run "diode --mode=tx --help" or "diode --mode=rx --help" for mode-specific flags.
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
	case "version":
		fmt.Println(version)
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
		return "", nil, fmt.Errorf("--mode is required (tx|rx)")
	}
	if mode != "tx" && mode != "rx" {
		return "", nil, fmt.Errorf("--mode must be tx or rx, got %q", mode)
	}
	return mode, rest, nil
}
