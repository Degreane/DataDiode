package framing

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"testing"
)

func randSID(t *testing.T) SessionID {
	t.Helper()
	var s SessionID
	if _, err := rand.Read(s[:]); err != nil {
		t.Fatalf("rand: %v", err)
	}
	return s
}

// ---- SOH roundtrip --------------------------------------------------------

func TestSOH_RoundTrip(t *testing.T) {
	contentHash := sha256.Sum256([]byte("file contents"))
	soh := SOH{
		SessionID:     randSID(t),
		ChunkTotal:    100,
		ChunkSize:     1400,
		TotalBytes:    139400, // 99 * 1400 + 200
		ContentSHA256: contentHash,
		Mode:          0o644,
		Name:          "report.pdf",
	}
	buf, err := EncodeSOH(nil, soh, nil)
	if err != nil {
		t.Fatalf("EncodeSOH: %v", err)
	}
	got, err := DecodeSOH(buf, nil)
	if err != nil {
		t.Fatalf("DecodeSOH: %v", err)
	}
	if got.SessionID != soh.SessionID ||
		got.ChunkTotal != soh.ChunkTotal ||
		got.ChunkSize != soh.ChunkSize ||
		got.TotalBytes != soh.TotalBytes ||
		got.ContentSHA256 != soh.ContentSHA256 ||
		got.Mode != soh.Mode ||
		got.Name != soh.Name {
		t.Fatalf("SOH mismatch:\n got  %+v\n want %+v", got, soh)
	}
}

func TestSOH_Signed(t *testing.T) {
	key := bytes.Repeat([]byte{0x42}, 32)
	soh := SOH{SessionID: randSID(t), ChunkTotal: 1, ChunkSize: 100, TotalBytes: 50, Name: "x"}
	buf, err := EncodeSOH(nil, soh, key)
	if err != nil {
		t.Fatalf("EncodeSOH signed: %v", err)
	}
	if _, err := DecodeSOH(buf, key); err != nil {
		t.Fatalf("DecodeSOH signed: %v", err)
	}
	// Wrong key → ErrDecryptFailed.
	if _, err := DecodeSOH(buf, bytes.Repeat([]byte{0x00}, 32)); !errors.Is(err, ErrDecryptFailed) {
		t.Fatalf("wrong key err: got %v, want ErrDecryptFailed", err)
	}
	// No key → ErrUnexpectedEncrypted (signed frame to keyless receiver).
	if _, err := DecodeSOH(buf, nil); !errors.Is(err, ErrUnexpectedEncrypted) {
		t.Fatalf("keyless err: got %v, want ErrUnexpectedEncrypted", err)
	}
}

func TestSOH_UnsignedRejectedByKeyedReceiver(t *testing.T) {
	soh := SOH{SessionID: randSID(t), ChunkTotal: 1, ChunkSize: 100, TotalBytes: 50, Name: "x"}
	buf, _ := EncodeSOH(nil, soh, nil)
	_, err := DecodeSOH(buf, bytes.Repeat([]byte{0x11}, 32))
	if !errors.Is(err, ErrEncryptedExpected) {
		t.Fatalf("err: got %v, want ErrEncryptedExpected", err)
	}
}

func TestSOH_RejectsBadNames(t *testing.T) {
	bad := []string{"", ".", "..", "foo/bar", "foo\\bar", "foo\x00bar"}
	for _, n := range bad {
		t.Run(n, func(t *testing.T) {
			soh := SOH{SessionID: randSID(t), ChunkTotal: 1, ChunkSize: 100, TotalBytes: 50, Name: n}
			_, err := EncodeSOH(nil, soh, nil)
			if err == nil {
				t.Fatalf("accepted bad name %q", n)
			}
		})
	}
}

func TestSOH_TamperedNameRejected(t *testing.T) {
	soh := SOH{SessionID: randSID(t), ChunkTotal: 1, ChunkSize: 100, TotalBytes: 50, Name: "ok.txt"}
	buf, _ := EncodeSOH(nil, soh, nil)
	// Patch name bytes to a traversal attempt; sha will then fail first
	// (good), but we also want to be sure the name check catches anything
	// the sha doesn't.
	buf[SOHHeaderLen] = '.'
	buf[SOHHeaderLen+1] = '.'
	_, err := DecodeSOH(buf, nil)
	if err == nil {
		t.Fatalf("tampered name accepted")
	}
}

func TestSOH_RejectsZeroChunkTotal(t *testing.T) {
	soh := SOH{SessionID: randSID(t), ChunkTotal: 0, ChunkSize: 100, TotalBytes: 0, Name: "x"}
	_, err := EncodeSOH(nil, soh, nil)
	if !errors.Is(err, ErrChunkTotal) {
		t.Fatalf("err: got %v, want ErrChunkTotal", err)
	}
}

// ---- DATA roundtrip -------------------------------------------------------

func TestDATA_RoundTrip(t *testing.T) {
	sid := randSID(t)
	payload := bytes.Repeat([]byte{0xAB}, MaxPayloadLen)
	d := DATA{Flags: FlagFinal, SessionID: sid, ChunkIndex: 99}

	buf, err := EncodeDATA(nil, d, payload, nil)
	if err != nil {
		t.Fatalf("EncodeDATA: %v", err)
	}
	gotD, gotP, err := DecodeDATA(buf, nil)
	if err != nil {
		t.Fatalf("DecodeDATA: %v", err)
	}
	if gotD.SessionID != sid || gotD.ChunkIndex != 99 || !gotD.IsFinal() {
		t.Fatalf("DATA fields: %+v", gotD)
	}
	if !bytes.Equal(gotP, payload) {
		t.Fatalf("payload mismatch")
	}
}

func TestDATA_EmptyPayload(t *testing.T) {
	d := DATA{Flags: FlagFinal, SessionID: randSID(t), ChunkIndex: 0}
	buf, err := EncodeDATA(nil, d, nil, nil)
	if err != nil {
		t.Fatalf("EncodeDATA: %v", err)
	}
	_, p, err := DecodeDATA(buf, nil)
	if err != nil {
		t.Fatalf("DecodeDATA: %v", err)
	}
	if len(p) != 0 {
		t.Fatalf("payload len: got %d, want 0", len(p))
	}
}

func TestDATA_Signed(t *testing.T) {
	key := bytes.Repeat([]byte{0x55}, 32)
	d := DATA{SessionID: randSID(t), ChunkIndex: 1}
	buf, err := EncodeDATA(nil, d, []byte("hi"), key)
	if err != nil {
		t.Fatalf("EncodeDATA: %v", err)
	}
	if _, _, err := DecodeDATA(buf, key); err != nil {
		t.Fatalf("DecodeDATA signed: %v", err)
	}
	if _, _, err := DecodeDATA(buf, nil); !errors.Is(err, ErrUnexpectedEncrypted) {
		t.Fatalf("err: got %v, want ErrUnexpectedEncrypted", err)
	}
}

// ---- cross-type rejection -------------------------------------------------

func TestDecodeSOH_RejectsDATAFrame(t *testing.T) {
	d := DATA{SessionID: randSID(t)}
	buf, _ := EncodeDATA(nil, d, []byte("x"), nil)
	if _, err := DecodeSOH(buf, nil); !errors.Is(err, ErrFrameType) {
		t.Fatalf("err: got %v, want ErrFrameType", err)
	}
}

func TestDecodeDATA_RejectsSOHFrame(t *testing.T) {
	soh := SOH{SessionID: randSID(t), ChunkTotal: 1, ChunkSize: 100, TotalBytes: 50, Name: "x"}
	buf, _ := EncodeSOH(nil, soh, nil)
	if _, _, err := DecodeDATA(buf, nil); !errors.Is(err, ErrFrameType) {
		t.Fatalf("err: got %v, want ErrFrameType", err)
	}
}

// ---- PeekKind for cheap routing ------------------------------------------

func TestPeekKind_SOH(t *testing.T) {
	sid := randSID(t)
	soh := SOH{SessionID: sid, ChunkTotal: 1, ChunkSize: 100, TotalBytes: 50, Name: "x"}
	buf, _ := EncodeSOH(nil, soh, nil)
	isSOH, gotSid, ok := PeekKind(buf)
	if !ok || !isSOH || gotSid != sid {
		t.Fatalf("PeekKind SOH: ok=%v isSOH=%v sid=%x", ok, isSOH, gotSid)
	}
}

func TestPeekKind_DATA(t *testing.T) {
	sid := randSID(t)
	d := DATA{SessionID: sid}
	buf, _ := EncodeDATA(nil, d, []byte("x"), nil)
	isSOH, gotSid, ok := PeekKind(buf)
	if !ok || isSOH || gotSid != sid {
		t.Fatalf("PeekKind DATA: ok=%v isSOH=%v sid=%x", ok, isSOH, gotSid)
	}
}

func TestPeekKind_Garbage(t *testing.T) {
	if _, _, ok := PeekKind([]byte{0, 0, 0, 0}); ok {
		t.Fatal("PeekKind accepted garbage")
	}
	if _, _, ok := PeekKind(nil); ok {
		t.Fatal("PeekKind accepted nil")
	}
}

// ---- corruption detection -------------------------------------------------

func TestDATA_TamperedPayloadRejected(t *testing.T) {
	d := DATA{SessionID: randSID(t), ChunkIndex: 0}
	buf, _ := EncodeDATA(nil, d, []byte("hello"), nil)
	buf[DATAHeaderLen] ^= 0xFF
	if _, _, err := DecodeDATA(buf, nil); !errors.Is(err, ErrHash) {
		t.Fatalf("err: got %v, want ErrHash", err)
	}
}

func TestDATA_BadMagic(t *testing.T) {
	d := DATA{SessionID: randSID(t)}
	buf, _ := EncodeDATA(nil, d, []byte("x"), nil)
	buf[0] ^= 0xFF
	if _, _, err := DecodeDATA(buf, nil); !errors.Is(err, ErrMagic) {
		t.Fatalf("err: got %v, want ErrMagic", err)
	}
}

func TestDATA_BadVersion(t *testing.T) {
	d := DATA{SessionID: randSID(t)}
	buf, _ := EncodeDATA(nil, d, []byte("x"), nil)
	buf[4] = 0x01
	if _, _, err := DecodeDATA(buf, nil); !errors.Is(err, ErrVersion) {
		t.Fatalf("err: got %v, want ErrVersion", err)
	}
}

func TestDATA_PayloadLenMismatch(t *testing.T) {
	d := DATA{SessionID: randSID(t)}
	buf, _ := EncodeDATA(nil, d, []byte("hello"), nil)
	binary.BigEndian.PutUint16(buf[26:28], 4) // lie
	if _, _, err := DecodeDATA(buf, nil); !errors.Is(err, ErrLenMismatch) {
		t.Fatalf("err: got %v, want ErrLenMismatch", err)
	}
}

// ---- session id string format --------------------------------------------

func TestSessionID_String(t *testing.T) {
	var sid SessionID
	for i := range sid {
		sid[i] = byte(i)
	}
	want := "00010203-0405-0607-0809-0a0b0c0d0e0f"
	if got := sid.String(); got != want {
		t.Fatalf("String: got %q want %q", got, want)
	}
}

// ---- bench ---------------------------------------------------------------

func BenchmarkEncodeDATA_MaxPayload(b *testing.B) {
	payload := bytes.Repeat([]byte{0xAB}, MaxPayloadLen)
	d := DATA{SessionID: SessionID{1, 2, 3}, ChunkIndex: 1}
	dst := make([]byte, 0, MaxDATAFrameLen)
	b.SetBytes(int64(MaxDATAFrameLen))
	for b.Loop() {
		dst = dst[:0]
		_, _ = EncodeDATA(dst, d, payload, nil)
	}
}

func BenchmarkDecodeDATA_MaxPayload(b *testing.B) {
	payload := bytes.Repeat([]byte{0xAB}, MaxPayloadLen)
	d := DATA{SessionID: SessionID{1, 2, 3}, ChunkIndex: 1}
	buf, _ := EncodeDATA(nil, d, payload, nil)
	b.SetBytes(int64(len(buf)))
	for b.Loop() {
		_, _, _ = DecodeDATA(buf, nil)
	}
}
