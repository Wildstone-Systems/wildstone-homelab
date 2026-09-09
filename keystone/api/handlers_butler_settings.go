package api

// Butler runtime settings from the Console (ADR-W030 A4/A10, P4).
//
// How long the Butler may work on a question before checking in, how
// long it waits on a model that has stopped answering, how often it
// retries, how stale a recorded reading may be, and what hours of the
// night it must not send a message — these are household preferences,
// not implementation constants, and they follow the same A9 pattern the
// provider choice already uses: runtime state, no restart, validated on
// write.
//
// Validation happens in runtimedb so that any writer gets it, and the
// error comes back verbatim so a Console form can show the person which
// field is wrong and what the range is.

import (
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/chickenrat17/wildstone-homelab/keystone/principal"
	"github.com/chickenrat17/wildstone-homelab/keystone/runtimedb"
)

// getButlerSettings returns the stored policy, or the defaults with
// stored=false.
//
// Absent is not an error: a deployment that has never opened this screen
// is correctly running on ADR-W030 A4's starting points, and reporting
// that as a failure would send an operator looking for a problem that is
// not there.
func (s *Server) getButlerSettings(_ *http.Request, _ principal.Identity) (any, int, error) {
	settings, stored, err := s.DB.GetButlerSettings()
	if err != nil {
		return nil, http.StatusInternalServerError, err
	}
	return map[string]any{"stored": stored, "settings": settings}, http.StatusOK, nil
}

// setButlerSettings validates and records the deployment's policy.
func (s *Server) setButlerSettings(r *http.Request, id principal.Identity) (any, int, error) {
	// Start from what is in force rather than from zero, so a Console
	// form that sends only the field it changed does not silently reset
	// the rest to invalid values.
	settings, _, err := s.DB.GetButlerSettings()
	if err != nil {
		return nil, http.StatusInternalServerError, err
	}
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&settings); err != nil {
		return nil, http.StatusBadRequest, fmt.Errorf("parsing body: %w", err)
	}
	// UpdatedAt is stamped by the store; a client-supplied one would be
	// a claim about when a change happened, made by the party with the
	// least reason to be accurate.
	settings.UpdatedAt = runtimedb.ButlerSettings{}.UpdatedAt
	// The acting identity where there is one, the principal otherwise —
	// a token minted on behalf of a person records the person, and an
	// operator's own token records the operator. Recording "" for the
	// second case would make the audit trail thinnest exactly where the
	// change was made directly.
	settings.UpdatedBy = id.ActingFor
	if settings.UpdatedBy == "" {
		settings.UpdatedBy = id.Name
	}
	if err := s.DB.SetButlerSettings(settings); err != nil {
		// The store's error names the field and its range, which is
		// exactly what the form needs to say.
		return nil, http.StatusBadRequest, err
	}
	stored, _, err := s.DB.GetButlerSettings()
	if err != nil {
		return nil, http.StatusInternalServerError, err
	}
	return map[string]any{"stored": true, "settings": stored}, http.StatusOK, nil
}
