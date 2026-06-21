package main

import (
	"slices"
	"strings"
	"testing"
)

func TestExtractMode(t *testing.T) {
	cases := []struct {
		name      string
		args      []string
		wantMode  string
		wantRest  []string
		wantErr   string // substring; "" means no error
		earlyExit bool   // help/version don't return rest
	}{
		{name: "mode-eq-tx", args: []string{"--mode=tx", "--dst=:9"}, wantMode: "tx", wantRest: []string{"--dst=:9"}},
		{name: "mode-space-rx", args: []string{"--mode", "rx", "--listen", ":9"}, wantMode: "rx", wantRest: []string{"--listen", ":9"}},
		{name: "single-dash-eq", args: []string{"-mode=tx", "--dst=:9"}, wantMode: "tx", wantRest: []string{"--dst=:9"}},
		{name: "single-dash-space", args: []string{"-mode", "tx", "--dst=:9"}, wantMode: "tx", wantRest: []string{"--dst=:9"}},
		{name: "help", args: []string{"--help"}, wantMode: "help", earlyExit: true},
		{name: "version", args: []string{"--version"}, wantMode: "version", earlyExit: true},

		{name: "missing", args: []string{"--dst=:9"}, wantErr: "--mode is required"},
		{name: "bad-value", args: []string{"--mode=foo"}, wantErr: "must be tx, rx, manifest, or vacuum"},
		{name: "no-value", args: []string{"--mode"}, wantErr: "requires a value"},
		{name: "double", args: []string{"--mode=tx", "--mode=rx"}, wantErr: "more than once"},

		// --help passes through to the mode parser when --mode came first.
		{name: "mode-then-help", args: []string{"--mode=tx", "--help"}, wantMode: "tx", wantRest: []string{"--help"}},
		{name: "mode-then-dst-help", args: []string{"--mode=rx", "--listen", ":9", "--help"}, wantMode: "rx", wantRest: []string{"--listen", ":9", "--help"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mode, rest, err := extractMode(tc.args)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err: got %v, want substring %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected err: %v", err)
			}
			if mode != tc.wantMode {
				t.Fatalf("mode: got %q, want %q", mode, tc.wantMode)
			}
			if !tc.earlyExit && !slices.Equal(rest, tc.wantRest) {
				t.Fatalf("rest: got %v, want %v", rest, tc.wantRest)
			}
		})
	}
}
