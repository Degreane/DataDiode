// Package udp provides the one-way UDP transport for DataDiode.
//
// Two types live here:
//
//   - Sender:   write-only. Wraps a connected *net.UDPConn. Exposes Send/Close.
//   - Receiver: read-only.  Wraps a bound      *net.UDPConn. Exposes Recv/Run/Close.
//
// The Receiver type deliberately exposes no method that writes to the
// network. That is half of the diode discipline (the other half is the
// host firewall rule installed by scripts/). Per-frame peer addresses
// returned by net.UDPConn.ReadFromUDP are discarded — the diode does
// not need to know who sent a frame and exposing that knowledge would
// invite a future contributor to "just reply".
//
// See ADR-0002 for the frame format these bytes carry. This package
// does not interpret the bytes; it just moves them.
package udp

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/degreane/datadiode/internal/framing"
)

// DefaultReadBufferLen is the size of the userspace buffer allocated
// per Recv call or per Run loop iteration. Must be at least
// framing.MaxFrameLen + framing.AEADTagLen (signed frames are 32 bytes
// larger than unsigned). Anything smaller would silently truncate
// datagrams.
const DefaultReadBufferLen = framing.MaxFrameLen + framing.AEADTagLen

// DefaultSocketRcvBufBytes is the default SO_RCVBUF size we ask the
// kernel to set on the receiver socket. At full-MTU frames this holds
// about 3000 in-flight datagrams — enough for ~30 ms of 1 Gbps wire
// at the userspace decode rate we measured (~4 GB/s framing.Decode).
// The kernel may cap this at net.core.rmem_max (Linux); the result is
// observed via SetReadBuffer and silently truncated, not an error.
const DefaultSocketRcvBufBytes = 4 << 20 // 4 MiB

// ---------- Sender ----------------------------------------------------------

// SenderOption configures a Sender at construction time.
type SenderOption func(*senderOpts)

type senderOpts struct {
	rateBytesPerSec int64
	writeTimeout    time.Duration
}

// WithRateBytesPerSec applies a token-bucket throttle to Send. A value
// <= 0 (the default) means unlimited.
func WithRateBytesPerSec(n int64) SenderOption {
	return func(o *senderOpts) { o.rateBytesPerSec = n }
}

// WithWriteTimeout sets a per-Send write deadline. Zero (the default)
// means no deadline. UDP writes rarely block, but a deadline guards
// against pathological kernel buffering on misconfigured hosts.
func WithWriteTimeout(d time.Duration) SenderOption {
	return func(o *senderOpts) { o.writeTimeout = d }
}

// Sender is the write side of the diode transport. It is safe for
// concurrent use; the underlying *net.UDPConn handles its own locking.
type Sender struct {
	conn *net.UDPConn
	opts senderOpts
	rl   *rateLimiter
}

// Dial resolves addr ("host:port") and opens a connected UDP socket.
// Returns an error if the address is malformed or the kernel refuses
// to allocate the socket. No packets are sent.
func Dial(addr string, opts ...SenderOption) (*Sender, error) {
	ua, err := net.ResolveUDPAddr("udp", addr)
	if err != nil {
		return nil, fmt.Errorf("udp: resolve %q: %w", addr, err)
	}
	conn, err := net.DialUDP("udp", nil, ua)
	if err != nil {
		return nil, fmt.Errorf("udp: dial %q: %w", addr, err)
	}
	s := &Sender{conn: conn}
	for _, o := range opts {
		o(&s.opts)
	}
	if s.opts.rateBytesPerSec > 0 {
		s.rl = newRateLimiter(s.opts.rateBytesPerSec)
	}
	return s, nil
}

// Send writes p as a single UDP datagram. It blocks at most until the
// configured rate budget allows the write and the kernel accepts the
// bytes. Returns io.ErrShortWrite if the kernel reports a short write
// (rare for UDP — sub-MTU writes either succeed atomically or fail).
func (s *Sender) Send(p []byte) error {
	if s.rl != nil {
		s.rl.wait(len(p))
	}
	if s.opts.writeTimeout > 0 {
		_ = s.conn.SetWriteDeadline(time.Now().Add(s.opts.writeTimeout))
	}
	n, err := s.conn.Write(p)
	if err != nil {
		return fmt.Errorf("udp: send: %w", err)
	}
	if n != len(p) {
		return fmt.Errorf("udp: send: short write (%d/%d)", n, len(p))
	}
	return nil
}

// Close releases the underlying socket.
func (s *Sender) Close() error { return s.conn.Close() }

// LocalAddr reports the address the kernel bound to for the outbound
// socket. Useful in tests.
func (s *Sender) LocalAddr() net.Addr { return s.conn.LocalAddr() }

// ---------- Receiver --------------------------------------------------------

// ReceiverOption configures a Receiver at construction time.
type ReceiverOption func(*receiverOpts)

type receiverOpts struct {
	bufferLen    int
	socketRcvBuf int
}

// WithBufferLen sets the read buffer size in bytes. Must be at least
// framing.MaxFrameLen or the constructor returns an error.
func WithBufferLen(n int) ReceiverOption {
	return func(o *receiverOpts) { o.bufferLen = n }
}

// WithSocketRcvBufBytes sets SO_RCVBUF on the underlying UDP socket.
// A value <= 0 leaves the kernel default in place. The kernel may
// silently cap this at net.core.rmem_max.
func WithSocketRcvBufBytes(n int) ReceiverOption {
	return func(o *receiverOpts) { o.socketRcvBuf = n }
}

// Receiver is the read side of the diode transport. It exposes no
// method that writes to the network. The underlying *net.UDPConn can
// in principle send, but is kept private and never used for sending.
type Receiver struct {
	conn *net.UDPConn
	opts receiverOpts
}

// Listen binds to addr ("host:port" or just ":port" for all interfaces)
// and returns a Receiver ready to read.
func Listen(addr string, opts ...ReceiverOption) (*Receiver, error) {
	ua, err := net.ResolveUDPAddr("udp", addr)
	if err != nil {
		return nil, fmt.Errorf("udp: resolve %q: %w", addr, err)
	}
	conn, err := net.ListenUDP("udp", ua)
	if err != nil {
		return nil, fmt.Errorf("udp: listen %q: %w", addr, err)
	}
	r := &Receiver{
		conn: conn,
		opts: receiverOpts{
			bufferLen:    DefaultReadBufferLen,
			socketRcvBuf: DefaultSocketRcvBufBytes,
		},
	}
	for _, o := range opts {
		o(&r.opts)
	}
	minBuf := framing.MaxFrameLen + framing.AEADTagLen
	if r.opts.bufferLen < minBuf {
		_ = conn.Close()
		return nil, fmt.Errorf("udp: buffer length %d < required %d (MaxFrameLen + HMACLen)", r.opts.bufferLen, minBuf)
	}
	if r.opts.socketRcvBuf > 0 {
		// Best-effort: ignore error so a sandbox without CAP_NET_ADMIN
		// or with a low net.core.rmem_max still gets a working receiver.
		// Operators who care can verify via `ss -ul`.
		_ = conn.SetReadBuffer(r.opts.socketRcvBuf)
	}
	return r, nil
}

// Recv reads one UDP datagram into dst and returns the number of bytes
// written. The peer address is intentionally discarded — see the
// package doc. If dst is shorter than the datagram, the datagram is
// truncated (matching net.UDPConn.ReadFromUDP semantics).
func (r *Receiver) Recv(dst []byte) (int, error) {
	n, _, err := r.conn.ReadFromUDP(dst)
	return n, err
}

// Run reads datagrams in a loop and hands each one to handle. Returns
// when ctx is cancelled, when handle returns an error, or when the
// underlying socket reports a fatal error.
//
// The slice passed to handle aliases an internal buffer and is only
// valid for the duration of the call. handle must copy any bytes it
// needs to retain.
func (r *Receiver) Run(ctx context.Context, handle func(payload []byte) error) error {
	// Closing the conn from a separate goroutine is the standard Go
	// idiom for cancelling a blocking ReadFromUDP. It causes the read
	// to return with a "use of closed network connection" error.
	done := make(chan struct{})
	var once sync.Once
	closeOnce := func() { once.Do(func() { _ = r.conn.Close() }) }
	defer closeOnce()

	go func() {
		select {
		case <-ctx.Done():
			closeOnce()
		case <-done:
		}
	}()
	defer close(done)

	buf := make([]byte, r.opts.bufferLen)
	for {
		n, _, err := r.conn.ReadFromUDP(buf)
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return ctx.Err()
			}
			return fmt.Errorf("udp: read: %w", err)
		}
		if err := handle(buf[:n]); err != nil {
			return err
		}
	}
}

// Close releases the underlying socket.
func (r *Receiver) Close() error { return r.conn.Close() }

// LocalAddr reports the address the receiver is bound to.
func (r *Receiver) LocalAddr() net.Addr { return r.conn.LocalAddr() }

// ---------- rate limiter (token bucket, no external deps) -------------------

type rateLimiter struct {
	mu          sync.Mutex
	bytesPerSec int64
	tokens      float64
	last        time.Time
}

func newRateLimiter(bytesPerSec int64) *rateLimiter {
	return &rateLimiter{
		bytesPerSec: bytesPerSec,
		tokens:      float64(bytesPerSec), // start with a full second of burst
		last:        time.Now(),
	}
}

func (rl *rateLimiter) wait(n int) {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	for {
		now := time.Now()
		elapsed := now.Sub(rl.last).Seconds()
		rl.last = now
		rl.tokens += elapsed * float64(rl.bytesPerSec)
		if burst := float64(rl.bytesPerSec); rl.tokens > burst {
			rl.tokens = burst // burst capped at 1s of throughput
		}
		if rl.tokens >= float64(n) {
			rl.tokens -= float64(n)
			return
		}
		deficit := float64(n) - rl.tokens
		sleep := time.Duration(deficit / float64(rl.bytesPerSec) * float64(time.Second))
		// Release the lock while sleeping so concurrent senders aren't
		// blocked on us; re-take it on the next iteration.
		rl.mu.Unlock()
		time.Sleep(sleep)
		rl.mu.Lock()
	}
}
