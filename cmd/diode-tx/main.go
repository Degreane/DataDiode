// Command diode-tx is the low-side (sender) daemon of the DataDiode.
//
// It reads bytes from an ingress source, chunks and frames them, and writes
// them out over a one-way transport (UDP by default). It NEVER opens a
// listening socket and NEVER reads from the network.
package main

import (
	"fmt"
	"os"
)

const version = "0.0.0-dev"

func main() {
	fmt.Fprintf(os.Stderr, "diode-tx %s (scaffold; not wired yet)\n", version)
}
