package main

import (
	"strings"
	"testing"

	"github.com/degreane/datadiode/internal/framing"
)

func TestParseRxFlags_Defaults(t *testing.T) {
	c, err := parseRxFlags([]string{"--listen=:9999"})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if c.listen != ":9999" {
		t.Fatalf("listen: %q", c.listen)
	}
	if c.outPath != "-" {
		t.Fatalf("outPath default: %q", c.outPath)
	}
	if c.maxPending <= 0 || c.maxBytes <= 0 {
		t.Fatalf("caps must be > 0: %+v", c)
	}
}

func TestParseRxFlags_RequiresListen(t *testing.T) {
	_, err := parseRxFlags([]string{})
	if err == nil || !strings.Contains(err.Error(), "--listen") {
		t.Fatalf("err: got %v, want --listen error", err)
	}
}

func TestParseRxFlags_RejectsTinyBuffer(t *testing.T) {
	_, err := parseRxFlags([]string{"--listen=:9", "--buffer-len=8"})
	if err == nil || !strings.Contains(err.Error(), "MaxFrameLen") {
		t.Fatalf("err: got %v, want MaxFrameLen error", err)
	}
	if framing.MaxFrameLen <= 8 {
		t.Fatalf("test premise broken: MaxFrameLen is %d", framing.MaxFrameLen)
	}
}
