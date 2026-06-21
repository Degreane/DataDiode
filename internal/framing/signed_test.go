package framing

import (
	"bytes"
	"errors"
	"testing"
)

var testKey = []byte("0123456789abcdef0123456789abcdef") // 32 bytes
var otherKey = []byte("FFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFF")

func TestSigned_Roundtrip(t *testing.T) {
	payload := []byte("authenticated hello")
	h := Header{ChunkTotal: 1, Flags: FlagFinal, Seq: 1, MsgID: 99}

	buf, err := Encode(nil, h, payload, testKey)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	wantLen := HeaderLen + len(payload) + HashLen + HMACLen
	if len(buf) != wantLen {
		t.Fatalf("signed frame length: got %d, want %d", len(buf), wantLen)
	}

	gotHdr, gotPayload, err := Decode(buf, testKey)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if !gotHdr.IsSigned() {
		t.Fatalf("decoded header should report IsSigned()")
	}
	if !bytes.Equal(gotPayload, payload) {
		t.Fatalf("payload mismatch")
	}
}

func TestSigned_DecodeRejectsWrongKey(t *testing.T) {
	buf, err := Encode(nil, Header{ChunkTotal: 1, Flags: FlagFinal}, []byte("x"), testKey)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	_, _, err = Decode(buf, otherKey)
	if !errors.Is(err, ErrHMACMismatch) {
		t.Fatalf("err: got %v, want %v", err, ErrHMACMismatch)
	}
}

func TestSigned_DecodeRejectsTamperedPayload(t *testing.T) {
	buf, _ := Encode(nil, Header{ChunkTotal: 1, Flags: FlagFinal}, []byte("payload"), testKey)
	// Flip a byte in the payload.
	buf[HeaderLen] ^= 0xFF
	// Tampering changes the sha256 first; that fires before HMAC check.
	_, _, err := Decode(buf, testKey)
	if !errors.Is(err, ErrHash) {
		t.Fatalf("err: got %v, want %v", err, ErrHash)
	}
}

func TestSigned_DecodeRejectsTamperedHMAC(t *testing.T) {
	buf, _ := Encode(nil, Header{ChunkTotal: 1, Flags: FlagFinal}, []byte("payload"), testKey)
	// Flip a byte in the HMAC trailer.
	buf[len(buf)-1] ^= 0xFF
	_, _, err := Decode(buf, testKey)
	if !errors.Is(err, ErrHMACMismatch) {
		t.Fatalf("err: got %v, want %v", err, ErrHMACMismatch)
	}
}

// Receiver with no key MUST reject signed frames (otherwise an
// attacker could downgrade auth by simply having a key the receiver
// doesn't know about).
func TestSigned_KeylessReceiverRejectsSigned(t *testing.T) {
	buf, _ := Encode(nil, Header{ChunkTotal: 1, Flags: FlagFinal}, []byte("x"), testKey)
	_, _, err := Decode(buf, nil)
	if !errors.Is(err, ErrUnexpectedSign) {
		t.Fatalf("err: got %v, want %v", err, ErrUnexpectedSign)
	}
}

// Receiver with a key MUST reject unsigned frames (otherwise an
// attacker could bypass auth by simply not signing).
func TestSigned_KeyedReceiverRejectsUnsigned(t *testing.T) {
	buf, _ := Encode(nil, Header{ChunkTotal: 1, Flags: FlagFinal}, []byte("x"), nil)
	_, _, err := Decode(buf, testKey)
	if !errors.Is(err, ErrSignedExpected) {
		t.Fatalf("err: got %v, want %v", err, ErrSignedExpected)
	}
}

// Callers must NOT pre-set FlagSigned; Encode owns that bit. Pre-setting
// is treated as a misuse and rejected.
func TestSigned_EncodeRejectsPreSetFlag(t *testing.T) {
	_, err := Encode(nil, Header{ChunkTotal: 1, Flags: FlagSigned}, nil, testKey)
	if !errors.Is(err, ErrReservedFlags) {
		t.Fatalf("err: got %v, want %v", err, ErrReservedFlags)
	}
}

// A signed empty payload is a valid heartbeat-style frame too.
func TestSigned_EmptyPayload(t *testing.T) {
	h := Header{ChunkTotal: 1, Flags: FlagFinal | FlagHeartbeat}
	buf, err := Encode(nil, h, nil, testKey)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	gotHdr, payload, err := Decode(buf, testKey)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if len(payload) != 0 || !gotHdr.IsHeartbeat() || !gotHdr.IsFinal() || !gotHdr.IsSigned() {
		t.Fatalf("unexpected header: %+v", gotHdr)
	}
}
