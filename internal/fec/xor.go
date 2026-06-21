// Package fec implements the XOR-based Forward Error Correction
// described in docs/architecture/ADR-0009-xor-fec.md.
//
// Each group of K data chunks gets one parity chunk = XOR of all K
// data chunks (zero-padded to chunk_size). The decoder can recover
// exactly one missing chunk per group by XORing the parity with the
// other K-1 received chunks.
//
// All inputs are byte slices of at most chunkSize bytes; shorter
// slices are treated as if zero-padded to chunkSize.
package fec

import "errors"

// ErrShardSizeMismatch is returned when any input shard is longer
// than chunkSize.
var ErrShardSizeMismatch = errors.New("fec: shard longer than chunkSize")

// ErrFECNotRecoverable is returned by Reconstruct when the number of
// missing shards is not exactly 1.
var ErrFECNotRecoverable = errors.New("fec: not recoverable (need exactly 1 missing shard)")

// Parity returns the XOR of every chunk in shards. Each shard may be
// up to chunkSize bytes; shorter shards are zero-padded on the fly
// (the tail past len(s) contributes nothing to the XOR). nil shards
// are skipped entirely.
//
// The returned slice is freshly allocated and has length chunkSize.
func Parity(shards [][]byte, chunkSize int) ([]byte, error) {
	if chunkSize <= 0 {
		return nil, errors.New("fec: chunkSize must be > 0")
	}
	out := make([]byte, chunkSize)
	for _, s := range shards {
		if s == nil {
			continue
		}
		if len(s) > chunkSize {
			return nil, ErrShardSizeMismatch
		}
		for i := 0; i < len(s); i++ {
			out[i] ^= s[i]
		}
	}
	return out, nil
}

// Reconstruct fills in exactly one missing data shard from the parity
// and the remaining K-1 received data shards.
//
//	shards is the group's slots in order, length groupSize. Each slot
//	  is nil if the shard is missing, or a chunkSize-byte (or shorter)
//	  slice if present.
//	parity is the chunkSize-byte parity for the group.
//
// Returns the reconstructed shard (length chunkSize) and the slot
// index it occupied. Returns ErrFECNotRecoverable if 0 or ≥2 shards
// are missing.
func Reconstruct(shards [][]byte, parity []byte, chunkSize int) ([]byte, int, error) {
	if chunkSize <= 0 {
		return nil, -1, errors.New("fec: chunkSize must be > 0")
	}
	if len(parity) != chunkSize {
		return nil, -1, ErrShardSizeMismatch
	}
	missingIdx := -1
	for i, s := range shards {
		if s == nil {
			if missingIdx != -1 {
				return nil, -1, ErrFECNotRecoverable
			}
			missingIdx = i
			continue
		}
		if len(s) > chunkSize {
			return nil, -1, ErrShardSizeMismatch
		}
	}
	if missingIdx == -1 {
		return nil, -1, ErrFECNotRecoverable
	}
	out := make([]byte, chunkSize)
	copy(out, parity)
	for i, s := range shards {
		if i == missingIdx || s == nil {
			continue
		}
		for j := 0; j < len(s); j++ {
			out[j] ^= s[j]
		}
	}
	return out, missingIdx, nil
}

// ParityTotal returns the number of parity chunks for chunkTotal data
// chunks at the given group size. Returns 0 when groupSize is 0 (FEC
// disabled).
func ParityTotal(chunkTotal, groupSize uint32) uint32 {
	if groupSize == 0 {
		return 0
	}
	return (chunkTotal + groupSize - 1) / groupSize
}

// GroupOf returns the group index (0-based) that contains data chunk
// dataIdx, given the group size.
func GroupOf(dataIdx, groupSize uint32) uint32 {
	if groupSize == 0 {
		return 0
	}
	return dataIdx / groupSize
}

// DataIndicesInGroup returns the (start, end) data indices for the
// given group. end is exclusive and capped at chunkTotal.
func DataIndicesInGroup(group, groupSize, chunkTotal uint32) (start, end uint32) {
	start = group * groupSize
	end = start + groupSize
	if end > chunkTotal {
		end = chunkTotal
	}
	return
}
