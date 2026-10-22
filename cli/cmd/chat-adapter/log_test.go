package main

import (
	"io"
	"log/slog"
)

// testLogger discards output: these tests assert on the wire, and a
// failing transport is a normal path here, not something to shout about.
func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}
