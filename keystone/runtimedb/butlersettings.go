package runtimedb

// Validated Butler runtime settings (ADR-W030 A4/A10, P4).
//
// These are household preferences, not implementation constants: how
// long the Butler may work on a question before checking in, how long it
// waits on a model that has stopped answering, how often it retries, how
// stale a recorded reading may be before it is taken again, and what
// hours of the night it must not send a message.
//
// They validate on WRITE. A stored setting outside its range would be a
// value nothing downstream can honour, discovered at the moment it
// matters; refusing it at the Console is the cheap place to find out.

import (
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// DefaultButlerSettings are A4's starting points, used until a
// deployment stores its own. The round limits are explicitly "subject to
// implementation evaluation" in the ADR, which is why they live here and
// in the database rather than in a const the Console cannot reach.
func DefaultButlerSettings() ButlerSettings {
	return ButlerSettings{
		HouseholdRounds: 16,
		OperatorRounds:  24,
		MaxToolCalls:    64,
		StallSeconds:    120,
		RetryLimit:      2,
		ObservationTTL:  300,
		SessionRecency:  1800,
		// No quiet hours until a household sets them. Defaulting to a
		// window would silently hold notices on a deployment that never
		// asked for one.
		QuietStartMin: 0,
		QuietEndMin:   0,
		Timezone:      "UTC",
	}
}

// ButlerSettings is the deployment's Butler runtime policy.
type ButlerSettings struct {
	HouseholdRounds int       `json:"householdRounds"`
	OperatorRounds  int       `json:"operatorRounds"`
	MaxToolCalls    int       `json:"maxToolCalls"`
	StallSeconds    int       `json:"stallSeconds"`
	RetryLimit      int       `json:"retryLimit"`
	ObservationTTL  int       `json:"observationTtlSeconds"`
	SessionRecency  int       `json:"sessionRecencySeconds"`
	QuietStartMin   int       `json:"quietStartMin"`
	QuietEndMin     int       `json:"quietEndMin"`
	Timezone        string    `json:"timezone"`
	UpdatedAt       time.Time `json:"updatedAt,omitzero"`
	UpdatedBy       string    `json:"updatedBy,omitempty"`
}

// Validate reports why these settings cannot be stored, or nil.
//
// The bounds are the same ones the schema CHECKs. Duplicating them is
// deliberate: the schema is the backstop that holds for any writer, and
// this is the layer that can say WHICH field is wrong and what the range
// is, which is what a Console form needs to show a person.
func (s ButlerSettings) Validate() error {
	for _, f := range []struct {
		name   string
		v      int
		lo, hi int
	}{
		{"householdRounds", s.HouseholdRounds, 1, 64},
		{"operatorRounds", s.OperatorRounds, 1, 64},
		{"maxToolCalls", s.MaxToolCalls, 1, 512},
		{"stallSeconds", s.StallSeconds, 5, 900},
		{"retryLimit", s.RetryLimit, 0, 10},
		{"observationTtlSeconds", s.ObservationTTL, 0, 86400},
		{"sessionRecencySeconds", s.SessionRecency, 60, 604800},
		{"quietStartMin", s.QuietStartMin, 0, 1439},
		{"quietEndMin", s.QuietEndMin, 0, 1439},
	} {
		if f.v < f.lo || f.v > f.hi {
			return fmt.Errorf("runtimedb: butler setting %s = %d, must be between %d and %d",
				f.name, f.v, f.lo, f.hi)
		}
	}
	if s.Timezone == "" {
		return fmt.Errorf("runtimedb: butler settings require a timezone")
	}
	// A timezone nothing can load would make quiet hours silently mean
	// UTC, which is the kind of "works, wrongly" a household discovers at
	// 3am.
	if _, err := time.LoadLocation(s.Timezone); err != nil {
		return fmt.Errorf("runtimedb: butler settings timezone %q is not a known zone: %w", s.Timezone, err)
	}
	return nil
}

// Location resolves the configured timezone, falling back to UTC. A
// stored zone always loads, because Validate refused anything else; the
// fallback covers a database restored onto a host with a different
// zoneinfo, where UTC is wrong but reporting a time is better than
// refusing to run.
func (s ButlerSettings) Location() *time.Location {
	if loc, err := time.LoadLocation(s.Timezone); err == nil {
		return loc
	}
	return time.UTC
}

// InQuietHours reports whether t (in the configured zone) falls inside
// the household's quiet window. Equal bounds mean no quiet hours.
func (s ButlerSettings) InQuietHours(t time.Time) bool {
	if s.QuietStartMin == s.QuietEndMin {
		return false
	}
	local := t.In(s.Location())
	m := local.Hour()*60 + local.Minute()
	if s.QuietStartMin < s.QuietEndMin {
		return m >= s.QuietStartMin && m < s.QuietEndMin
	}
	// The window wraps midnight — 22:00 to 07:00 — which is the shape a
	// household actually configures.
	return m >= s.QuietStartMin || m < s.QuietEndMin
}

// QuietHoursEnd returns the first instant at or after t that is outside
// quiet hours, so a held notice has a concrete delivery time rather than
// a flag something has to keep re-checking.
func (s ButlerSettings) QuietHoursEnd(t time.Time) time.Time {
	if !s.InQuietHours(t) {
		return t
	}
	loc := s.Location()
	local := t.In(loc)
	end := time.Date(local.Year(), local.Month(), local.Day(),
		s.QuietEndMin/60, s.QuietEndMin%60, 0, 0, loc)
	if !end.After(local) {
		end = end.AddDate(0, 0, 1)
	}
	return end.UTC()
}

// GetButlerSettings returns the stored settings, or the defaults with
// ok=false when a deployment has never set any.
func (db *DB) GetButlerSettings() (ButlerSettings, bool, error) {
	var s ButlerSettings
	var updated string
	err := db.sql.QueryRow(
		`SELECT household_rounds, operator_rounds, max_tool_calls, stall_seconds, retry_limit,
		        observation_ttl_seconds, session_recency_seconds, quiet_start_min, quiet_end_min, timezone, updated_at, updated_by
		 FROM butler_runtime_settings WHERE id = 1`).
		Scan(&s.HouseholdRounds, &s.OperatorRounds, &s.MaxToolCalls, &s.StallSeconds, &s.RetryLimit,
			&s.ObservationTTL, &s.SessionRecency, &s.QuietStartMin, &s.QuietEndMin, &s.Timezone, &updated, &s.UpdatedBy)
	if errors.Is(err, sql.ErrNoRows) {
		return DefaultButlerSettings(), false, nil
	}
	if err != nil {
		return ButlerSettings{}, false, fmt.Errorf("runtimedb: reading butler settings: %w", err)
	}
	if s.UpdatedAt, err = time.Parse(time.RFC3339Nano, updated); err != nil {
		return ButlerSettings{}, false, fmt.Errorf("runtimedb: bad butler settings updated_at %q: %w", updated, err)
	}
	return s, true, nil
}

// SetButlerSettings validates and stores the deployment's policy.
func (db *DB) SetButlerSettings(s ButlerSettings) error {
	if err := s.Validate(); err != nil {
		return err
	}
	_, err := db.sql.Exec(
		`INSERT INTO butler_runtime_settings (id, household_rounds, operator_rounds, max_tool_calls,
		     stall_seconds, retry_limit, observation_ttl_seconds, session_recency_seconds, quiet_start_min, quiet_end_min,
		     timezone, updated_at, updated_by)
		 VALUES (1, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(id) DO UPDATE SET
		   household_rounds = excluded.household_rounds,
		   operator_rounds = excluded.operator_rounds,
		   max_tool_calls = excluded.max_tool_calls,
		   stall_seconds = excluded.stall_seconds,
		   retry_limit = excluded.retry_limit,
		   observation_ttl_seconds = excluded.observation_ttl_seconds,
		   session_recency_seconds = excluded.session_recency_seconds,
		   quiet_start_min = excluded.quiet_start_min,
		   quiet_end_min = excluded.quiet_end_min,
		   timezone = excluded.timezone,
		   updated_at = excluded.updated_at,
		   updated_by = excluded.updated_by`,
		s.HouseholdRounds, s.OperatorRounds, s.MaxToolCalls, s.StallSeconds, s.RetryLimit,
		s.ObservationTTL, s.SessionRecency, s.QuietStartMin, s.QuietEndMin, s.Timezone, now(), s.UpdatedBy)
	if err != nil {
		return fmt.Errorf("runtimedb: writing butler settings: %w", err)
	}
	return nil
}
