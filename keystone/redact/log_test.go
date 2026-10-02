package redact

import (
	"bytes"
	"errors"
	"log/slog"
	"strings"
	"testing"
)

func TestLogHandlerRedactsMessagesAttrsAndErrors(t *testing.T) {
	var buf bytes.Buffer
	log := Logger(slog.New(slog.NewTextHandler(&buf, nil)))
	const secret = "REPLACEDxAPPxPASSWORDxabcdefghijklmnop0123"
	log.With("bound", "password="+secret).Info("login failed password="+secret,
		"err", errors.New("provider said token: "+secret),
		"detail", secret,
		slog.Group("g", "inner", secret),
		"connection", "sam-mail", "count", 3)
	out := buf.String()
	if strings.Contains(out, secret) {
		t.Fatalf("a secret-shaped value reached the log: %s", out)
	}
	for _, keep := range []string{"connection=sam-mail", "count=3"} {
		if !strings.Contains(out, keep) {
			t.Fatalf("an ordinary attribute was lost (%s): %s", keep, out)
		}
	}
	if Logger(log) != log {
		t.Fatal("wrapping twice should reuse the redacting logger")
	}
}
