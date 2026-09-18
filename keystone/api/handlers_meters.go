package api

// Meter readings (ADR-W035 K1): the intake for the numbers a household
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
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/chickenrat17/wildstone-homelab/keystone/butler"
	"github.com/chickenrat17/wildstone-homelab/keystone/commitment"
	"github.com/chickenrat17/wildstone-homelab/keystone/principal"
	"github.com/chickenrat17/wildstone-homelab/keystone/resource"
	"github.com/chickenrat17/wildstone-homelab/keystone/runtimedb"
	"github.com/chickenrat17/wildstone-homelab/keystone/upkeep"
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
	if s.OnMeterReading != nil {
		s.OnMeterReading()
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
		return fmt.Errorf("%q is the calendar and reads itself; it takes no readings (ADR-W035 A2)", name)
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
	// (ADR-W035 A2). It is listed because a household asking what meters
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

// --- commitments, owner side (ADR-W035 K6) ---------------------------

// listCommitments answers where every declared commitment stands.
//
// The same evaluation the household's own page uses, unfiltered: an
// Owner in the Console is looking at the whole house, including the
// personal commitments the portal keeps to their owner. That is the
// Console being the control plane rather than a bigger Homepage — and it
// is why this route holds VerbRead and the portal's holds nothing.
func (s *Server) listCommitments(_ *http.Request, _ principal.Identity) (any, int, error) {
	if s.Upkeep == nil {
		return nil, http.StatusServiceUnavailable, fmt.Errorf("the commitment engine is not running")
	}
	standings, err := s.Upkeep.Evaluate()
	if err != nil {
		return nil, http.StatusInternalServerError, err
	}
	if standings == nil {
		standings = []upkeep.Standing{}
	}
	return standings, http.StatusOK, nil
}

// commitmentHistory is one commitment's ledger: where it stands now, and
// every cycle it has closed.
func (s *Server) commitmentHistory(r *http.Request, _ principal.Identity) (any, int, error) {
	if s.Upkeep == nil {
		return nil, http.StatusServiceUnavailable, fmt.Errorf("the commitment engine is not running")
	}
	name := r.PathValue("name")
	standings, err := s.Upkeep.Evaluate()
	if err != nil {
		return nil, http.StatusInternalServerError, err
	}
	var found *upkeep.Standing
	for i := range standings {
		if standings[i].Name == name {
			found = &standings[i]
		}
	}
	if found == nil {
		return nil, http.StatusNotFound, fmt.Errorf("no commitment named %q", name)
	}
	completions, err := s.DB.ListCommitmentCompletions(name)
	if err != nil {
		return nil, http.StatusInternalServerError, err
	}
	if completions == nil {
		completions = []runtimedb.CommitmentCompletion{}
	}
	return map[string]any{"standing": found, "completions": completions}, http.StatusOK, nil
}

// MeterReadingSeam wires the family agent's reading door (ADR-W035 A3)
// to the same store and the same refusals every other door uses.
//
// IT IS THE HANDLER'S RULES, NOT A SECOND SET. The meter must be
// declared, the clock refuses readings because it reads itself, and the
// value is stamped `person` because a household member is who the agent
// is speaking for — all exactly as the HTTP intake beside it does. A
// door that validated differently would be a fifth set of rules
// pretending to be a fourth door.
//
// What it will not do is decide anything. It writes a number and returns
// a sentence; the next pass works out what that number means, and the
// agent finds out the same way everybody else does.
func (s *Server) MeterReadingSeam() func(context.Context, butler.ReadingCall) (string, error) {
	return func(_ context.Context, c butler.ReadingCall) (string, error) {
		if err := s.meterMustBeDeclared(c.Meter); err != nil {
			return "", err
		}
		if c.Member == "" {
			return "", fmt.Errorf("a reading has to be somebody's")
		}
		if c.Value < 0 {
			return "", fmt.Errorf("a meter counts up from zero")
		}
		seq, duplicate, err := s.DB.AppendMeterReading(runtimedb.MeterReading{
			Meter: c.Meter, Value: c.Value, At: time.Now().UTC(),
			RecordedBy: c.Member, Source: runtimedb.ReadingByPerson,
		})
		if err != nil {
			return "", err
		}
		s.pokeUpkeep()
		if duplicate {
			return fmt.Sprintf("%s was already recorded at that number.", c.Meter), nil
		}
		return fmt.Sprintf("Recorded: %s is at %g (#%d).", c.Meter, c.Value, seq), nil
	}
}
