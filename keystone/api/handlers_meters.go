package api

// Meter readings (ADR-W034 K1): the intake for the numbers a household
// keeps, and the two reads that answer "where does this stand".
//
// WHY THIS IS NOT A PLAN. Every other write to what the household has
// declared goes through the plan pipeline, and a reading deliberately
// does not. A reading is evidence, not authority: it changes no rule,
// grants nothing, and asserts only what a meter said at a moment. Making
// it a Plan would mean an approval queue between a person and writing
// down a mileage, and the household that then never writes one down is
// the exact failure the design exists to prevent (A16, A20).
//
// The Meter it is about IS desired state, and is checked here: a reading
// of a meter nobody declared is refused rather than accumulating under a
// name with a typo in it, where it would be invisible to every commitment
// and look like a meter nobody reads.

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/chickenrat17/wildstone-homelab/keystone/commitment"
	"github.com/chickenrat17/wildstone-homelab/keystone/principal"
	"github.com/chickenrat17/wildstone-homelab/keystone/resource"
	"github.com/chickenrat17/wildstone-homelab/keystone/runtimedb"
)

// maxReadingBytes bounds a reading request. A number, a timestamp and a
// sentence; anything larger is not a reading.
const maxReadingBytes = 4 << 10

// readingRequest is what a caller sends. Note what is absent: who
// recorded it and what kind of caller they are. Both come from the
// authenticated principal, because a body that could name its own author
// is a body that can name somebody else's.
type readingRequest struct {
	Value float64 `json:"value"`
	// At is when the meter was read. Optional: omitted means now, which
	// is right for somebody reading a dashboard as they type. Supplying
	// it is how a number taken this morning is recorded this evening
	// without pretending it is fresh.
	At *time.Time `json:"at,omitempty"`
	// Rebase marks a replaced meter: the coordinate space restarts, the
	// obligation does not. Prior is what the old one read beforehand.
	Rebase bool    `json:"rebase,omitempty"`
	Prior  float64 `json:"prior,omitempty"`
	Reason string  `json:"reason,omitempty"`
	// ExternalID lets a publisher retry a lost delivery without
	// recording the number twice. A person leaves it empty and may
	// legitimately record the same value again.
	ExternalID string `json:"externalId,omitempty"`
}

// recordMeterReading takes one reading for a declared meter.
func (s *Server) recordMeterReading(r *http.Request, id principal.Identity) (any, int, error) {
	name := r.PathValue("name")
	if err := s.meterMustBeDeclared(name); err != nil {
		return nil, http.StatusNotFound, err
	}

	var req readingRequest
	if err := json.NewDecoder(http.MaxBytesReader(nil, r.Body, maxReadingBytes)).Decode(&req); err != nil {
		return nil, http.StatusBadRequest, fmt.Errorf("parsing reading: %w", err)
	}

	at := time.Now().UTC()
	if req.At != nil {
		at = req.At.UTC()
	}
	// A reading from the future is a clock fault, not a fact. The same
	// bound the household intake puts on occurred_at, and the same
	// reason: the due-ness function refuses to see past Now anyway, so
	// accepting one here would only store a row nothing can ever read.
	if at.After(time.Now().Add(maxReadingSkew)) {
		return nil, http.StatusBadRequest, fmt.Errorf(
			"a reading taken at %s is in the future; check the clock that produced it", at.Format(time.RFC3339))
	}
	if req.Rebase && req.Prior < 0 {
		return nil, http.StatusBadRequest, fmt.Errorf("prior is what the old meter read, so it cannot be negative")
	}
	if req.Value < 0 {
		return nil, http.StatusBadRequest, fmt.Errorf("a meter counts up from zero; %g is not a reading", req.Value)
	}

	seq, duplicate, err := s.DB.AppendMeterReading(runtimedb.MeterReading{
		Meter: name, Value: req.Value, At: at,
		RecordedBy: id.Name, Source: readingSourceFor(id),
		Rebase: req.Rebase, Prior: req.Prior, Reason: req.Reason,
		ExternalID: req.ExternalID,
	})
	if err != nil {
		return nil, http.StatusInternalServerError, err
	}
	receipt := map[string]any{"seq": seq, "duplicate": duplicate}
	if duplicate {
		return receipt, http.StatusOK, nil
	}
	return receipt, http.StatusCreated, nil
}

// maxReadingSkew is how far ahead of Keystone's clock a reading may be
// taken and still be accepted, matching the household intake's bound.
const maxReadingSkew = 5 * time.Minute

// readingSourceFor says what kind of caller this is, from the role and
// nothing else. Only the two the credential proves: a capability's
// publisher principal, or a person. Which screen a person used is not
// something the control plane knows, and a column claiming otherwise
// would be holding the client's word for it.
func readingSourceFor(id principal.Identity) string {
	if id.Role == runtimedb.RolePublisher {
		return runtimedb.ReadingByPublisher
	}
	return runtimedb.ReadingByPerson
}

// meterMustBeDeclared refuses a reading for a meter the Statefile does
// not have, and for the built-in calendar, which reads itself.
func (s *Server) meterMustBeDeclared(name string) error {
	if name == "" {
		return fmt.Errorf("which meter?")
	}
	if name == commitment.MeterClock {
		return fmt.Errorf("%q is the calendar and reads itself; it takes no readings (ADR-W034 A2)", name)
	}
	current, err := s.State.Load()
	if err != nil {
		return err
	}
	for _, res := range current {
		if res.Kind == resource.KindMeter && res.Metadata.Name == name {
			return nil
		}
	}
	return fmt.Errorf("no meter named %q is declared; readings for one would be invisible to every commitment", name)
}

// meterView is one meter as the Console and the CLI want it: what it is,
// and where it stands.
type meterView struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Unit        string `json:"unit,omitempty"`
	Owner       string `json:"owner,omitempty"`
	StaleAfter  string `json:"staleAfter,omitempty"`
	Estimate    string `json:"estimate,omitempty"`
	// BuiltIn marks the calendar, which is declared nowhere and cannot be
	// (ADR-W034 A2). It is listed because a household asking what meters
	// it has should be told about the one it did not have to create.
	BuiltIn bool `json:"builtIn,omitempty"`
	// Latest is the newest reading, absent when the meter has never been
	// read. Absent is not zero: a meter nobody has read does not read
	// zero, and a caller must be able to tell those apart (A11).
	Latest *runtimedb.MeterReading `json:"latest,omitempty"`
	// Stale says the newest reading is older than the meter allows. It is
	// computed here rather than left to each caller, because "is this
	// number still worth anything" is one question with one answer.
	Stale bool `json:"stale,omitempty"`
}

// listMeters lists every declared meter, the built-in calendar, and
// where each stands.
func (s *Server) listMeters(_ *http.Request, _ principal.Identity) (any, int, error) {
	current, err := s.State.Load()
	if err != nil {
		return nil, http.StatusInternalServerError, err
	}
	out := []meterView{{
		Name:        commitment.MeterClock,
		Description: "The calendar. Every household has one; it is declared nowhere.",
		BuiltIn:     true,
	}}
	for _, res := range current {
		if res.Kind != resource.KindMeter || res.Meter == nil {
			continue
		}
		v := meterView{
			Name: res.Metadata.Name, Description: res.Meter.Description,
			Unit: res.Meter.Unit, Owner: res.Metadata.Owner,
			StaleAfter: res.Meter.StaleAfter, Estimate: res.Meter.Estimate,
		}
		latest, err := s.DB.LatestMeterReading(res.Metadata.Name)
		switch {
		case err == nil:
			v.Latest = &latest
			if window, perr := resource.MeterStaleAfter(res.Meter.StaleAfter); perr == nil && window > 0 {
				v.Stale = time.Since(latest.At) > window
			}
		case errors.Is(err, runtimedb.ErrNotFound):
			// Never read. Left absent rather than filled with a zero.
		default:
			return nil, http.StatusInternalServerError, err
		}
		out = append(out, v)
	}
	return out, http.StatusOK, nil
}

// listMeterReadings serves one meter's history, oldest first.
func (s *Server) listMeterReadings(r *http.Request, _ principal.Identity) (any, int, error) {
	name := r.PathValue("name")
	if err := s.meterMustBeDeclared(name); err != nil {
		return nil, http.StatusNotFound, err
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	readings, err := s.DB.ListMeterReadings(name, limit)
	if err != nil {
		return nil, http.StatusInternalServerError, err
	}
	return readings, http.StatusOK, nil
}
