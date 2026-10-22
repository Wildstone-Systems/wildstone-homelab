// Package household owns the household event envelope (ADR-W024 A2): the
// canonical shape every publisher — a first-party app, a sidecar adapter,
// Home Assistant calling a webhook — speaks, and the rules that make an
// envelope acceptable. Storage is keystone/runtimedb; this package owns
// the semantics, the way keystone/principal does for identity.
//
// An envelope is a FACT about the household, not a prompt: it says what
// happened, when, to whom, in a small bounded payload. Nothing here
// interprets one. Anything that reaches a model later does so through the
// same redaction and untrusted-content envelope every other external text
// gets (ADR-W024 A4).
package household

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode"

	"github.com/Wildstone-Systems/wildstone-homelab/keystone/runtimedb"
)

const (
	// MaxPayloadBytes bounds the payload. An event carries the few fields a
	// handler needs to decide; the application's database stays
	// authoritative for the rest (ADR-W024 A2).
	MaxPayloadBytes = 4096
	// MaxEventIDBytes bounds the publisher-supplied id. A UUID or ULID
	// fits with room; a payload smuggled into the id does not.
	MaxEventIDBytes = 128
	// MaxFieldBytes bounds subject, entity, and correlation id.
	MaxFieldBytes = 256
	// MaxFutureSkew is how far ahead of Keystone's clock an occurred_at may
	// be before it is refused as a clock fault rather than accepted as a
	// fact from the future.
	MaxFutureSkew = 5 * time.Minute
)

// EventTypeRegexp is the canonical event type shape: a lowercase
// namespace, a dot, a lowercase name — `person.left_home`,
// `pantry.meal_logged`. Deeper namespaces are allowed
// (`calendar.school.closed`). keystone/pkgspec carries the same pattern
// for a package's `publishes` block; a test there pins the two equal.
var EventTypeRegexp = regexp.MustCompile(`^[a-z][a-z0-9_]*(\.[a-z][a-z0-9_]*)+$`)

// Envelope is the wire shape a publisher sends. Source and recorded_at
// are NOT here: the intake stamps source from the publisher's token and
// recorded_at from its own clock, so a publisher can only ever speak for
// itself (ADR-W024 A3).
type Envelope struct {
	EventID       string          `json:"eventId"`
	OccurredAt    time.Time       `json:"occurredAt"`
	Subject       string          `json:"subject,omitempty"`
	Type          string          `json:"type"`
	Entity        string          `json:"entity,omitempty"`
	Payload       json.RawMessage `json:"payload,omitempty"`
	CorrelationID string          `json:"correlationId,omitempty"`
	SchemaVersion int             `json:"schemaVersion,omitempty"`
}

// ValidationError names the field that failed.
type ValidationError struct {
	Field string
	Msg   string
}

func (e *ValidationError) Error() string { return "household: " + e.Field + ": " + e.Msg }

// Validate checks an envelope against the contract. now is the intake's
// clock. A defaulted SchemaVersion (0) becomes 1.
func (e *Envelope) Validate(now time.Time) error {
	if err := checkField("eventId", e.EventID, MaxEventIDBytes, true); err != nil {
		return err
	}
	if e.OccurredAt.IsZero() {
		return &ValidationError{Field: "occurredAt", Msg: "required"}
	}
	if e.OccurredAt.After(now.Add(MaxFutureSkew)) {
		return &ValidationError{Field: "occurredAt", Msg: fmt.Sprintf("more than %s in the future; check the publisher's clock", MaxFutureSkew)}
	}
	if e.Type == "" {
		return &ValidationError{Field: "type", Msg: "required"}
	}
	if !EventTypeRegexp.MatchString(e.Type) {
		return &ValidationError{Field: "type", Msg: fmt.Sprintf("%q must be namespace.name in lowercase [a-z0-9_], e.g. person.left_home", e.Type)}
	}
	for _, f := range []struct{ name, value string }{
		{"subject", e.Subject}, {"entity", e.Entity}, {"correlationId", e.CorrelationID},
	} {
		if err := checkField(f.name, f.value, MaxFieldBytes, false); err != nil {
			return err
		}
	}
	if len(e.Payload) > MaxPayloadBytes {
		return &ValidationError{Field: "payload", Msg: fmt.Sprintf("%d bytes exceeds the %d-byte bound; publish a reference, not the record", len(e.Payload), MaxPayloadBytes)}
	}
	if len(e.Payload) > 0 {
		var obj map[string]json.RawMessage
		if err := json.Unmarshal(e.Payload, &obj); err != nil || obj == nil {
			return &ValidationError{Field: "payload", Msg: "must be a JSON object"}
		}
	}
	if e.SchemaVersion < 0 {
		return &ValidationError{Field: "schemaVersion", Msg: "must be positive"}
	}
	if e.SchemaVersion == 0 {
		e.SchemaVersion = 1
	}
	return nil
}

// checkField refuses empty (when required), oversize, and control
// characters — an id or subject is an identifier, never a document.
func checkField(name, value string, max int, required bool) error {
	if value == "" {
		if required {
			return &ValidationError{Field: name, Msg: "required"}
		}
		return nil
	}
	if len(value) > max {
		return &ValidationError{Field: name, Msg: fmt.Sprintf("exceeds %d bytes", max)}
	}
	if i := strings.IndexFunc(value, func(r rune) bool { return unicode.IsControl(r) }); i >= 0 {
		return &ValidationError{Field: name, Msg: fmt.Sprintf("control character at offset %d", i)}
	}
	return nil
}

// Receipt is what a publisher gets back: the row's sequence number and
// whether this delivery was a retry of one already recorded.
type Receipt struct {
	Seq       int64  `json:"seq"`
	EventID   string `json:"eventId"`
	Duplicate bool   `json:"duplicate"`
}

// Store appends validated envelopes to the runtime DB.
type Store struct {
	DB *runtimedb.DB
	// Now is the intake clock; nil means time.Now.
	Now func() time.Time
}

// Append validates env and records it as published by source (the
// authenticated principal's name — never a value from the request).
func (s *Store) Append(env Envelope, source string) (Receipt, error) {
	if source == "" {
		return Receipt{}, fmt.Errorf("household: source required")
	}
	now := time.Now
	if s.Now != nil {
		now = s.Now
	}
	if err := env.Validate(now()); err != nil {
		return Receipt{}, err
	}
	seq, dup, err := s.DB.AppendHouseholdEvent(runtimedb.HouseholdEvent{
		EventID:       env.EventID,
		OccurredAt:    env.OccurredAt,
		Subject:       env.Subject,
		Source:        source,
		Type:          env.Type,
		Entity:        env.Entity,
		Payload:       env.Payload,
		CorrelationID: env.CorrelationID,
		SchemaVersion: env.SchemaVersion,
	})
	if err != nil {
		return Receipt{}, err
	}
	return Receipt{Seq: seq, EventID: env.EventID, Duplicate: dup}, nil
}

// After returns events past the cursor, oldest first.
func (s *Store) After(after int64, limit int) ([]runtimedb.HouseholdEvent, error) {
	return s.DB.HouseholdEventsAfter(after, limit)
}
