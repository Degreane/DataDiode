package main

import (
	"strings"
	"testing"

	"github.com/degreane/datadiode/internal/framing"
)

func TestParseTxFlags_Defaults(t *testing.T) {
	c, err := parseTxFlags([]string{"--dst=10.99.0.20:9999", "--send-file=/tmp/x"})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if c.dst != "10.99.0.20:9999" {
		t.Fatalf("dst: %q", c.dst)
	}
	if c.chunkSize != framing.MaxPayloadLen {
		t.Fatalf("chunkSize default: %d, want %d", c.chunkSize, framing.MaxPayloadLen)
	}
	if c.redundancy != 1 {
		t.Fatalf("redundancy default: %d", c.redundancy)
	}
	if c.sohRedundancy != 3 {
		t.Fatalf("sohRedundancy default: %d", c.sohRedundancy)
	}
}

func TestParseTxFlags_RequiresDst(t *testing.T) {
	_, err := parseTxFlags([]string{"--send-file=/tmp/x"})
	if err == nil || !strings.Contains(err.Error(), "--dst") {
		t.Fatalf("err: got %v, want --dst error", err)
	}
}

func TestParseTxFlags_RequiresSource(t *testing.T) {
	_, err := parseTxFlags([]string{"--dst=:9"})
	if err == nil || !strings.Contains(err.Error(), "--send-file") {
		t.Fatalf("err: got %v, want source error", err)
	}
}

func TestParseTxFlags_MutuallyExclusiveSources(t *testing.T) {
	_, err := parseTxFlags([]string{"--dst=:9", "--send-file=a", "--in=b"})
	if err == nil || !strings.Contains(err.Error(), "mutually exclusive") {
		t.Fatalf("err: got %v, want mutually exclusive error", err)
	}
}

func TestParseTxFlags_RejectsBadChunkSize(t *testing.T) {
	for _, bad := range []string{"--chunk-size=0", "--chunk-size=99999"} {
		t.Run(bad, func(t *testing.T) {
			_, err := parseTxFlags([]string{"--dst=:9", "--send-file=/tmp/x", bad})
			if err == nil {
				t.Fatalf("expected error for %s", bad)
			}
		})
	}
}

func TestParseTxFlags_RejectsBadRedundancy(t *testing.T) {
	_, err := parseTxFlags([]string{"--dst=:9", "--send-file=/tmp/x", "--redundancy=0"})
	if err == nil {
		t.Fatalf("expected error for --redundancy=0")
	}
	_, err = parseTxFlags([]string{"--dst=:9", "--send-file=/tmp/x", "--soh-redundancy=0"})
	if err == nil {
		t.Fatalf("expected error for --soh-redundancy=0")
	}
}
