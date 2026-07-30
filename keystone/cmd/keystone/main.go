// Command keystone is the Wildstone control plane: API gateway, Reconciler,
// Capability Engine, Policy Engine, Statefile, and Butler host process.
package main

import (
	"fmt"
	"os"
)

func main() {
	fmt.Fprintln(os.Stderr, "keystone: pre-alpha scaffold (spike S2 pending)")
	os.Exit(1)
}
