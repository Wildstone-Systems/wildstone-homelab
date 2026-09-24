package agentapi

import (
	"errors"
	"fmt"
)

// ErrNotImplemented is the sentinel every NotImplementedError matches
// through errors.Is. It is the one protocol error a runtime should
// expect while the migration plan is in flight (ADR-W039 §8): the
// contract names every family from Phase 0, and a Wildstone side that
// does not yet serve one says so with this rather than with a silent
// success or an outcome that means something else.
var ErrNotImplemented = errors.New("agentapi: not implemented on this Wildstone side")

// NotImplementedError says which Client method is not yet served and
// which phase of the migration plan serves it. It is a transport-level
// error, not an Outcome: the call did not happen, and a runtime should
// treat it as it would any protocol failure — stop, do not retry.
type NotImplementedError struct {
	// Method is the Client method's name, e.g. "Propose".
	Method string
	// Phase is the ADR-W039 §8 phase that serves it, e.g. "Phase 3".
	Phase string
	// Reason says what the method waits on, for a person reading a log.
	Reason string
}

func (e *NotImplementedError) Error() string {
	msg := fmt.Sprintf("agentapi: %s is not implemented on this Wildstone side until ADR-W039 %s", e.Method, e.Phase)
	if e.Reason != "" {
		msg += ": " + e.Reason
	}
	return msg
}

// Is makes every NotImplementedError match ErrNotImplemented.
func (e *NotImplementedError) Is(target error) bool { return target == ErrNotImplemented }
