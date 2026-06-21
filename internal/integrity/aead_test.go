package integrity

import (
	"bytes"
	"encoding/hex"
	"errors"
	"testing"
)

// RFC 5869 Test Case 1: SHA-256, length 22 IKM, length 13 salt, length 10 info, OKM len 42.
func TestHKDFSHA256_KnownAnswer_RFC5869_Case1(t *testing.T) {
	ikm, _ := hex.DecodeString("0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b")
	salt, _ := hex.DecodeString("000102030405060708090a0b0c")
	info, _ := hex.DecodeString("f0f1f2f3f4f5f6f7f8f9")
	wantHex := "3cb25f25faacd57a90434f64d0362f2a" +
		"2d2d0a90cf1a5a4c5db02d56ecc4c5bf" +
		"34007208d5b887185865"
	want, _ := hex.DecodeString(wantHex)

	got, err := HKDFSHA256(salt, ikm, info, 42)
	if err != nil {
		t.Fatalf("HKDFSHA256: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("OKM mismatch:\n got  %x\n want %x", got, want)
	}
}

// Empty salt is allowed by RFC 5869 §2.2 — replaced by HashLen zeros.
func TestHKDFSHA256_EmptySalt(t *testing.T) {
	got, err := HKDFSHA256(nil, []byte("ikm"), []byte("info"), 32)
	if err != nil {
		t.Fatalf("HKDFSHA256: %v", err)
	}
	if len(got) != 32 {
		t.Fatalf("len: %d, want 32", len(got))
	}
}

func TestHKDFSHA256_LengthOutOfRange(t *testing.T) {
	if _, err := HKDFSHA256(nil, []byte("x"), nil, 0); err == nil {
		t.Fatal("expected error for okmLen=0")
	}
	if _, err := HKDFSHA256(nil, []byte("x"), nil, 255*32+1); err == nil {
		t.Fatal("expected error for okmLen > 255*HashLen")
	}
}

// DeriveAEADKey is deterministic per PSK and produces 32 bytes.
func TestDeriveAEADKey_Deterministic(t *testing.T) {
	psk := bytes.Repeat([]byte{0xAB}, 32)
	a, err := DeriveAEADKey(psk)
	if err != nil {
		t.Fatalf("DeriveAEADKey: %v", err)
	}
	b, _ := DeriveAEADKey(psk)
	if !bytes.Equal(a, b) {
		t.Fatalf("non-deterministic: %x != %x", a, b)
	}
	if len(a) != AEADKeyLen {
		t.Fatalf("len: %d want %d", len(a), AEADKeyLen)
	}
	// Different PSK → different subkey.
	psk2 := bytes.Repeat([]byte{0xCD}, 32)
	c, _ := DeriveAEADKey(psk2)
	if bytes.Equal(a, c) {
		t.Fatalf("different PSKs produced same subkey")
	}
}

// Round-trip: encrypt then decrypt with matching key/nonce/aad.
func TestAEAD_RoundTrip(t *testing.T) {
	key := bytes.Repeat([]byte{0x55}, AEADKeyLen)
	nonce := bytes.Repeat([]byte{0x11}, AEADNonceLen)
	aad := []byte("header bytes")
	plaintext := []byte("a quick brown fox jumps over the lazy dog")

	ct, err := AEADSeal(nil, key, nonce, aad, plaintext)
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	if len(ct) != len(plaintext)+AEADTagLen {
		t.Fatalf("ciphertext len: got %d, want %d", len(ct), len(plaintext)+AEADTagLen)
	}
	pt, err := AEADOpen(nil, key, nonce, aad, ct)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if !bytes.Equal(pt, plaintext) {
		t.Fatalf("plaintext mismatch")
	}
}

func TestAEAD_OpenWithWrongKey(t *testing.T) {
	key := bytes.Repeat([]byte{0x55}, AEADKeyLen)
	wrongKey := bytes.Repeat([]byte{0xAA}, AEADKeyLen)
	nonce := bytes.Repeat([]byte{0x11}, AEADNonceLen)
	ct, _ := AEADSeal(nil, key, nonce, nil, []byte("secret"))
	if _, err := AEADOpen(nil, wrongKey, nonce, nil, ct); !errors.Is(err, ErrAEADDecrypt) {
		t.Fatalf("err: got %v want ErrAEADDecrypt", err)
	}
}

func TestAEAD_OpenWithTamperedCiphertext(t *testing.T) {
	key := bytes.Repeat([]byte{0x55}, AEADKeyLen)
	nonce := bytes.Repeat([]byte{0x11}, AEADNonceLen)
	ct, _ := AEADSeal(nil, key, nonce, nil, []byte("hello"))
	ct[0] ^= 0xFF // flip a byte in the ciphertext (not the tag)
	if _, err := AEADOpen(nil, key, nonce, nil, ct); !errors.Is(err, ErrAEADDecrypt) {
		t.Fatalf("err: got %v want ErrAEADDecrypt", err)
	}
}

func TestAEAD_OpenWithTamperedAAD(t *testing.T) {
	key := bytes.Repeat([]byte{0x55}, AEADKeyLen)
	nonce := bytes.Repeat([]byte{0x11}, AEADNonceLen)
	ct, _ := AEADSeal(nil, key, nonce, []byte("hdr"), []byte("hello"))
	if _, err := AEADOpen(nil, key, nonce, []byte("HDR"), ct); !errors.Is(err, ErrAEADDecrypt) {
		t.Fatalf("err: got %v want ErrAEADDecrypt", err)
	}
}

func TestAEAD_OpenWithWrongNonce(t *testing.T) {
	key := bytes.Repeat([]byte{0x55}, AEADKeyLen)
	n1 := bytes.Repeat([]byte{0x11}, AEADNonceLen)
	n2 := bytes.Repeat([]byte{0x22}, AEADNonceLen)
	ct, _ := AEADSeal(nil, key, n1, nil, []byte("hello"))
	if _, err := AEADOpen(nil, key, n2, nil, ct); !errors.Is(err, ErrAEADDecrypt) {
		t.Fatalf("err: got %v want ErrAEADDecrypt", err)
	}
}

func TestNewAEAD_RejectsBadKeyLen(t *testing.T) {
	if _, err := NewAEAD(make([]byte, 16)); err == nil {
		t.Fatal("expected error for 16-byte key")
	}
}

func TestAEADSeal_RejectsBadNonceLen(t *testing.T) {
	key := make([]byte, AEADKeyLen)
	if _, err := AEADSeal(nil, key, make([]byte, 8), nil, []byte("x")); err == nil {
		t.Fatal("expected error for 8-byte nonce")
	}
}

// ----- benchmarks ---------------------------------------------------------

func BenchmarkAEADSeal_1KiB(b *testing.B) {
	key := bytes.Repeat([]byte{0x55}, AEADKeyLen)
	nonce := bytes.Repeat([]byte{0x11}, AEADNonceLen)
	pt := bytes.Repeat([]byte{0xAB}, 1024)
	b.SetBytes(int64(len(pt)))
	for b.Loop() {
		_, _ = AEADSeal(nil, key, nonce, nil, pt)
	}
}

func BenchmarkAEADOpen_1KiB(b *testing.B) {
	key := bytes.Repeat([]byte{0x55}, AEADKeyLen)
	nonce := bytes.Repeat([]byte{0x11}, AEADNonceLen)
	pt := bytes.Repeat([]byte{0xAB}, 1024)
	ct, _ := AEADSeal(nil, key, nonce, nil, pt)
	b.SetBytes(int64(len(pt)))
	for b.Loop() {
		_, _ = AEADOpen(nil, key, nonce, nil, ct)
	}
}
