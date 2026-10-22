package main

import (
	"bytes"
	"context"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
)

// -agent-runtime=native with no runtime installed must not stop the
// control plane: conversations stay on the legacy loop, and the log
// says why.
func TestNativeRuntimeFallsBackLoudly(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, nil))

	if rt := nativeRuntime(context.Background(), butlerConfig{AgentRuntime: agentRuntimeLegacy}, log); rt != nil {
		t.Fatal("legacy configuration started the native runtime")
	}
	if rt := nativeRuntime(context.Background(), butlerConfig{}, log); rt != nil {
		t.Fatal("an unset runtime started the native runtime")
	}
	missing := filepath.Join(t.TempDir(), "not-installed")
	if rt := nativeRuntime(context.Background(), butlerConfig{AgentRuntime: agentRuntimeNative, NativeAgentDir: missing}, log); rt != nil {
		t.Fatal("a missing runtime was wired")
	}
	if !strings.Contains(buf.String(), "failed its self-check") || !strings.Contains(buf.String(), missing) {
		t.Fatalf("the fallback was not logged: %s", buf.String())
	}
}
