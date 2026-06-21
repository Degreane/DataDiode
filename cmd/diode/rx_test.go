package main

import (
	"strings"
	"testing"

	"github.com/degreane/datadiode/internal/framing"
)

func TestParseRxFlags_Defaults(t *testing.T) {
	c, err := parseRxFlags([]string{"--listen=:9999", "--files-to=/tmp/out"})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if c.listen != ":9999" {
		t.Fatalf("listen: %q", c.listen)
	}
	if c.spoolMode != "sparse" {
		t.Fatalf("spoolMode default: %q", c.spoolMode)
	}
	if c.bufferLen < framing.MaxFrameLen {
		t.Fatalf("bufferLen default: %d", c.bufferLen)
	}
}

func TestParseRxFlags_RequiresListen(t *testing.T) {
	_, err := parseRxFlags([]string{"--files-to=/tmp/x"})
	if err == nil || !strings.Contains(err.Error(), "--listen") {
		t.Fatalf("err: got %v, want --listen error", err)
	}
}

func TestParseRxFlags_RequiresSink(t *testing.T) {
	_, err := parseRxFlags([]string{"--listen=:9"})
	if err == nil || !strings.Contains(err.Error(), "--files-to") {
		t.Fatalf("err: got %v, want sink error", err)
	}
}

func TestParseRxFlags_MutuallyExclusiveSinks(t *testing.T) {
	_, err := parseRxFlags([]string{"--listen=:9", "--files-to=/tmp/x", "--out=-"})
	if err == nil || !strings.Contains(err.Error(), "mutually exclusive") {
		t.Fatalf("err: got %v, want mutually exclusive error", err)
	}
}

func TestParseRxFlags_RejectsTinyBuffer(t *testing.T) {
	_, err := parseRxFlags([]string{"--listen=:9", "--files-to=/tmp/x", "--buffer-len=8"})
	if err == nil || !strings.Contains(err.Error(), "MaxFrameLen") {
		t.Fatalf("err: got %v, want MaxFrameLen error", err)
	}
}
