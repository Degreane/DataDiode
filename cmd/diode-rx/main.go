// Command diode-rx is the high-side (receiver) daemon of the DataDiode.
//
// It listens on a one-way transport (UDP by default), verifies frame
// integrity, reassembles messages, and writes them to an egress sink. It
// NEVER opens an outbound socket and NEVER writes to the network. This
// property is the application-layer half of the diode guarantee; the
// network-layer half is enforced by host firewall rules (see scripts/).
package main

import (
	"fmt"
	"os"
)

const version = "0.0.0-dev"

func main() {
	fmt.Fprintf(os.Stderr, "diode-rx %s (scaffold; not wired yet)\n", version)
}
