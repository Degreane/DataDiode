// Package reassembly turns a stream of decoded ADR-0002 frames into
// completed messages.
//
// One Reassembler instance is fed every frame the receiver decodes (in
// order of arrival). Frames carrying the HEARTBEAT flag are counted and
// discarded. For data frames the reassembler buffers chunks keyed by
// (msg_id, chunk_index) and emits the concatenated payload via a
// caller-supplied deliver callback once all chunks for a msg_id have
// been seen.
//
// Loss handling: the reassembler does NOT request retransmission (the
// diode has no return path). A partial message that never completes
// would otherwise leak memory forever, so the reassembler enforces both
// a pending-message count cap and an aggregate byte budget. When either
// limit is exceeded, the oldest pending message is evicted.
//
// The type is NOT safe for concurrent use; the calling Receiver.Run
// loop is single-goroutine by design.
package reassembly

import (
	"errors"
	"fmt"

	"github.com/degreane/datadiode/internal/framing"
)

// DeliverFunc is invoked once per fully-reassembled message. The slice
// is owned by the callback for the duration of the call only; copy if
// you need to retain it.
type DeliverFunc func(payload []byte) error

// Stats is a snapshot of reassembler counters.
type Stats struct {
	FramesIn      uint64
	FramesDup     uint64 // chunk we already had for an active message
	FramesIgnored uint64 // heartbeat or rejected (mismatched total, etc.)
	MsgsDelivered uint64
	MsgsEvicted   uint64 // dropped due to byte / count budget
	BytesPending  int    // bytes held in incomplete messages right now
}

// Options configure a Reassembler.
type Options struct {
	// MaxPending is the maximum number of incomplete messages held at
	// once. When exceeded, oldest are evicted. Must be > 0.
	MaxPending int
	// MaxBytes is the aggregate byte budget for incomplete messages.
	// When exceeded, oldest are evicted. Must be > 0.
	MaxBytes int
	// RecentDeliveredSize is the number of recently-delivered MsgIDs
	// remembered, so that REDUNDANT copies of a just-completed
	// message are recognised as duplicates instead of being treated
	// as a new (possibly reused) MsgID. Must be >= 0; 0 disables the
	// cache (every redundant copy of a completed single-chunk
	// message will re-deliver).
	RecentDeliveredSize int
}

// DefaultOptions returns sensible Sprint-01 defaults.
func DefaultOptions() Options {
	return Options{
		MaxPending:          1024,
		MaxBytes:            64 << 20, // 64 MiB
		RecentDeliveredSize: 1024,
	}
}

// Reassembler buffers frame chunks into messages.
type Reassembler struct {
	opts     Options
	deliver  DeliverFunc
	pending  map[uint32]*partial
	order    []uint32 // oldest-first; used for FIFO eviction
	curBytes int

	// Recently-delivered MsgIDs, FIFO-evicted at opts.RecentDeliveredSize.
	// Used to recognise REDUNDANT copies arriving after delivery.
	recent      map[uint32]struct{}
	recentOrder []uint32

	stats Stats
}

type partial struct {
	chunks   [][]byte // indexed by chunk_index; nil = not received yet
	received int      // count of non-nil entries in chunks
	total    int
	bytes    int
}

// New returns a Reassembler that delivers completed messages via deliver.
func New(deliver DeliverFunc, opts Options) (*Reassembler, error) {
	if deliver == nil {
		return nil, errors.New("reassembly: nil DeliverFunc")
	}
	if opts.MaxPending <= 0 || opts.MaxBytes <= 0 {
		return nil, fmt.Errorf("reassembly: MaxPending and MaxBytes must be > 0; got %+v", opts)
	}
	if opts.RecentDeliveredSize < 0 {
		return nil, fmt.Errorf("reassembly: RecentDeliveredSize must be >= 0; got %d", opts.RecentDeliveredSize)
	}
	return &Reassembler{
		opts:    opts,
		deliver: deliver,
		pending: make(map[uint32]*partial, opts.MaxPending),
		recent:  make(map[uint32]struct{}, opts.RecentDeliveredSize),
	}, nil
}

// Ingest consumes a single received UDP datagram. It decodes the frame
// and either:
//   - drops it (heartbeat, duplicate chunk, validation failure),
//   - buffers a chunk and updates eviction bookkeeping, or
//   - delivers a completed message via the DeliverFunc.
//
// Returns whatever the DeliverFunc returns; the receiver loop uses that
// error to stop (e.g., closed output pipe).
func (r *Reassembler) Ingest(rawFrame []byte) error {
	r.stats.FramesIn++

	h, payload, err := framing.Decode(rawFrame)
	if err != nil {
		// Per ADR-0002 §"Validation rules": drop silently. Counter
		// only; logging at line rate would DoS the operator.
		r.stats.FramesIgnored++
		return nil
	}
	if h.IsHeartbeat() {
		r.stats.FramesIgnored++
		return nil
	}

	// A REDUNDANT frame for a MsgID we already delivered is a duplicate,
	// not a fresh message. Only consult the recent-delivered cache when
	// the frame carries REDUNDANT, so MsgID wrap (~4 billion messages
	// later) still works correctly for legitimately reused IDs.
	if h.IsRedundant() {
		if _, seen := r.recent[h.MsgID]; seen {
			r.stats.FramesDup++
			return nil
		}
	}

	p, ok := r.pending[h.MsgID]
	if !ok {
		p = &partial{
			chunks: make([][]byte, h.ChunkTotal),
			total:  int(h.ChunkTotal),
		}
		r.pending[h.MsgID] = p
		r.order = append(r.order, h.MsgID)
	} else if p.total != int(h.ChunkTotal) {
		// Same msg_id but different chunk_total — corruption or
		// MsgID wrap landing on a still-pending message. Either way
		// the existing partial is unrecoverable; drop it and start
		// fresh.
		r.curBytes -= p.bytes
		r.removeFromOrder(h.MsgID)
		p = &partial{
			chunks: make([][]byte, h.ChunkTotal),
			total:  int(h.ChunkTotal),
		}
		r.pending[h.MsgID] = p
		r.order = append(r.order, h.MsgID)
	}

	if p.chunks[h.ChunkIndex] != nil {
		// Duplicate chunk (usually a REDUNDANT copy). Ignore.
		r.stats.FramesDup++
		return nil
	}

	chunk := make([]byte, len(payload))
	copy(chunk, payload)
	p.chunks[h.ChunkIndex] = chunk
	p.received++
	p.bytes += len(chunk)
	r.curBytes += len(chunk)

	if p.received == p.total {
		// Complete: assemble and deliver.
		full := make([]byte, 0, p.bytes)
		for _, c := range p.chunks {
			full = append(full, c...)
		}
		delete(r.pending, h.MsgID)
		r.removeFromOrder(h.MsgID)
		r.curBytes -= p.bytes
		r.stats.MsgsDelivered++
		r.markRecent(h.MsgID)
		return r.deliver(full)
	}

	r.evictIfOverBudget()
	return nil
}

// evictIfOverBudget removes oldest pending messages until both budgets
// are satisfied. A message is only evicted if it actually has pending
// chunks (already-delivered IDs may remain in the order slice briefly).
func (r *Reassembler) evictIfOverBudget() {
	for r.curBytes > r.opts.MaxBytes || len(r.pending) > r.opts.MaxPending {
		if len(r.order) == 0 {
			return
		}
		oldest := r.order[0]
		r.order = r.order[1:]
		if op, ok := r.pending[oldest]; ok {
			r.curBytes -= op.bytes
			delete(r.pending, oldest)
			r.stats.MsgsEvicted++
		}
	}
}

// markRecent records that id was just delivered. FIFO-evicts when the
// cache exceeds opts.RecentDeliveredSize. No-op when size is 0.
func (r *Reassembler) markRecent(id uint32) {
	if r.opts.RecentDeliveredSize <= 0 {
		return
	}
	if _, ok := r.recent[id]; ok {
		// Already in cache; refresh insertion order by re-appending.
		// (Doesn't preserve strict LRU semantics — strict FIFO is good
		// enough at this cache size.)
		return
	}
	r.recent[id] = struct{}{}
	r.recentOrder = append(r.recentOrder, id)
	for len(r.recentOrder) > r.opts.RecentDeliveredSize {
		evict := r.recentOrder[0]
		r.recentOrder = r.recentOrder[1:]
		delete(r.recent, evict)
	}
}

// removeFromOrder removes the first occurrence of id from r.order.
// O(n) on a slice of recent IDs; n is bounded by MaxPending.
func (r *Reassembler) removeFromOrder(id uint32) {
	for i, v := range r.order {
		if v == id {
			r.order = append(r.order[:i], r.order[i+1:]...)
			return
		}
	}
}

// Stats returns a snapshot of the current counters.
func (r *Reassembler) Stats() Stats {
	s := r.stats
	s.BytesPending = r.curBytes
	return s
}
