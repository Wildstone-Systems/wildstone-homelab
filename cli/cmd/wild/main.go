// Command wild is the Wildstone CLI, a thin client of the Keystone public
// API — the same API the Console and the Butler use.
package main

import (
	"fmt"
	"os"
)

func main() {
	fmt.Fprintln(os.Stderr, "wild: pre-alpha scaffold")
	os.Exit(1)
}
