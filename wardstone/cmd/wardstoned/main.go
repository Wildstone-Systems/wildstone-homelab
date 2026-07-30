// Command wardstoned is the Wildstone host agent: the only component that
// touches the host. It dials out to Keystone over mTLS gRPC and serves a
// closed, typed operation surface. There is no Exec(string) here and there
// never will be.
package main

import (
	"fmt"
	"os"
)

func main() {
	fmt.Fprintln(os.Stderr, "wardstoned: pre-alpha scaffold (spike S1 pending)")
	os.Exit(1)
}
