// Package udp provides the one-way UDP transport.
//
// Sender opens a connected UDP socket and writes frames. Receiver opens a
// bound UDP socket and reads frames. The Receiver type exposes only a
// read-side API — there is no exported method that writes to the network,
// by design.
package udp
