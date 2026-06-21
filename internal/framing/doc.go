// Package framing encodes and decodes the DataDiode on-wire frame.
//
// Frame layout (v0, see ADR-0002):
//
//	magic (4B) | ver (1B) | flags (1B) | seq (8B BE) | payload_len (4B BE) |
//	payload (≤1400 B) | sha256 (32 B over magic..payload)
package framing
