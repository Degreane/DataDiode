package udp

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/degreane/datadiode/internal/framing"
)

// helper: start a receiver on an ephemeral port and return its addr.
func startReceiver(t *testing.T, opts ...ReceiverOption) *Receiver {
	t.Helper()
	r, err := Listen("127.0.0.1:0", opts...)
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	t.Cleanup(func() { _ = r.Close() })
	return r
}

func TestSendRecv_RoundTrip(t *testing.T) {
	r := startReceiver(t)

	s, err := Dial(r.LocalAddr().String())
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer s.Close()

	payload := []byte("hello, diode")
	if err := s.Send(payload); err != nil {
		t.Fatalf("Send: %v", err)
	}

	if err := r.conn.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatalf("SetReadDeadline: %v", err)
	}
	buf := make([]byte, DefaultReadBufferLen)
	n, err := r.Recv(buf)
	if err != nil {
		t.Fatalf("Recv: %v", err)
	}
	if !bytes.Equal(buf[:n], payload) {
		t.Fatalf("payload: got %q, want %q", buf[:n], payload)
	}
}

func TestSendRecv_MaxFrameLen(t *testing.T) {
	r := startReceiver(t)
	s, err := Dial(r.LocalAddr().String())
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer s.Close()

	payload := bytes.Repeat([]byte{0xAB}, framing.MaxFrameLen)
	if err := s.Send(payload); err != nil {
		t.Fatalf("Send: %v", err)
	}
	_ = r.conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, DefaultReadBufferLen)
	n, err := r.Recv(buf)
	if err != nil {
		t.Fatalf("Recv: %v", err)
	}
	if n != len(payload) {
		t.Fatalf("Recv: got %d bytes, want %d", n, len(payload))
	}
}

func TestRun_DeliversAndCancels(t *testing.T) {
	r := startReceiver(t)
	s, err := Dial(r.LocalAddr().String())
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer s.Close()

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	const N = 10
	var got atomic.Int64
	done := make(chan error, 1)
	go func() {
		done <- r.Run(ctx, func(payload []byte) error {
			got.Add(1)
			return nil
		})
	}()

	for i := range N {
		if err := s.Send(fmt.Appendf(nil, "msg-%d", i)); err != nil {
			t.Fatalf("Send: %v", err)
		}
	}

	deadline := time.Now().Add(2 * time.Second)
	for got.Load() < int64(N) && time.Now().Before(deadline) {
		time.Sleep(2 * time.Millisecond)
	}
	if got.Load() != int64(N) {
		t.Fatalf("delivered: got %d, want %d", got.Load(), N)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Fatalf("Run returned unexpected error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("Run did not return after ctx cancel")
	}
}

func TestRun_HandleErrorStops(t *testing.T) {
	r := startReceiver(t)
	s, err := Dial(r.LocalAddr().String())
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer s.Close()

	stopErr := errors.New("synthetic-stop")
	done := make(chan error, 1)
	go func() {
		done <- r.Run(t.Context(), func(payload []byte) error { return stopErr })
	}()

	if err := s.Send([]byte("x")); err != nil {
		t.Fatalf("Send: %v", err)
	}
	select {
	case err := <-done:
		if !errors.Is(err, stopErr) {
			t.Fatalf("Run err: got %v, want %v", err, stopErr)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("Run did not return after handle error")
	}
}

func TestListen_RejectsTooSmallBuffer(t *testing.T) {
	_, err := Listen("127.0.0.1:0", WithBufferLen(8))
	if err == nil {
		t.Fatalf("Listen: expected error for tiny buffer, got nil")
	}
}

func TestDial_BadAddr(t *testing.T) {
	_, err := Dial("not a host")
	if err == nil {
		t.Fatalf("Dial: expected error for bad addr, got nil")
	}
}

// Multiple senders to one receiver — proves the Receiver does not
// require a single peer.
func TestRun_MultipleSenders(t *testing.T) {
	r := startReceiver(t)

	const senders = 4
	const perSender = 25
	var got atomic.Int64

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- r.Run(ctx, func(_ []byte) error {
			got.Add(1)
			return nil
		})
	}()

	var wg sync.WaitGroup
	for i := range senders {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			s, err := Dial(r.LocalAddr().String())
			if err != nil {
				t.Errorf("Dial: %v", err)
				return
			}
			defer s.Close()
			for j := range perSender {
				if err := s.Send(fmt.Appendf(nil, "s%d-m%d", id, j)); err != nil {
					t.Errorf("Send: %v", err)
				}
			}
		}(i)
	}
	wg.Wait()

	deadline := time.Now().Add(2 * time.Second)
	want := int64(senders * perSender)
	for got.Load() < want && time.Now().Before(deadline) {
		time.Sleep(2 * time.Millisecond)
	}
	if got.Load() != want {
		t.Fatalf("delivered: got %d, want %d", got.Load(), want)
	}
	cancel()
	<-done
}

// The Receiver type must expose no exported method that writes to the
// network. This test asserts the API surface by reflection: any method
// with "Send", "Write", or "WriteTo" in its name is a regression.
func TestReceiver_NoWriteMethods(t *testing.T) {
	rt := reflect.TypeFor[*Receiver]()
	for m := range rt.NumMethod() {
		name := rt.Method(m).Name
		switch name {
		case "Send", "Write", "WriteTo", "WriteToUDP", "WriteMsgUDP":
			t.Fatalf("Receiver exposes %q — violates diode discipline", name)
		}
	}
}

// ---- rate limiter --------------------------------------------------------

// The rate limiter is approximate; we assert order-of-magnitude correctness,
// not exact timing.
func TestRateLimiter_ApproximatelyLimits(t *testing.T) {
	const rate = 100 * 1024 // 100 KiB/s
	rl := newRateLimiter(rate)

	// Drain the initial burst so the test sees steady-state behavior.
	rl.wait(rate)

	start := time.Now()
	const writes = 5
	const writeSize = 20 * 1024 // 20 KiB → 100 KiB total → ~1 s at steady state
	for range writes {
		rl.wait(writeSize)
	}
	elapsed := time.Since(start)
	// Expect roughly 1s; allow [500ms .. 2.5s] for CI noise.
	if elapsed < 500*time.Millisecond || elapsed > 2500*time.Millisecond {
		t.Fatalf("rate limiter took %v, expected ~1s", elapsed)
	}
}

// Disabled rate limiter (rate <= 0) means Send never sleeps.
func TestSender_NoRateLimitByDefault(t *testing.T) {
	r := startReceiver(t)
	s, err := Dial(r.LocalAddr().String())
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer s.Close()
	if s.rl != nil {
		t.Fatalf("rate limiter should be nil when no option supplied")
	}
}

// ---- benchmark -----------------------------------------------------------

func BenchmarkSendRecv_Loopback(b *testing.B) {
	r, err := Listen("127.0.0.1:0")
	if err != nil {
		b.Fatalf("Listen: %v", err)
	}
	defer r.Close()
	s, err := Dial(r.LocalAddr().String())
	if err != nil {
		b.Fatalf("Dial: %v", err)
	}
	defer s.Close()

	payload := bytes.Repeat([]byte{0xAB}, framing.MaxFrameLen)
	buf := make([]byte, DefaultReadBufferLen)
	b.SetBytes(int64(len(payload)))

	for b.Loop() {
		if err := s.Send(payload); err != nil {
			b.Fatalf("Send: %v", err)
		}
		_ = r.conn.SetReadDeadline(time.Now().Add(time.Second))
		if _, err := r.Recv(buf); err != nil {
			b.Fatalf("Recv: %v", err)
		}
	}
}
