package api

// What the assistant remembers about a member, and their right to make
// it forget.
//
// THE SCOPING IS THE STORE'S, NOT THIS FILE'S. ExportMemoryFor answers
// "what may this person read" — their own records, plus household-shared
// ones that have not been forgotten or superseded — and it is the same
// query the Console's export uses. A second implementation of "whose
// memory is this" would be a second place for the answer to be wrong.
//
// THE LIFECYCLE IS THIS FILE'S, and that is a different question the
// store answers differently on purpose. An export includes a person's
// OWN forgotten records, because A3 says a copy of them is theirs to
// have. A panel headed with what the assistant currently remembers
// includes no such thing, or the member's own forget would appear to
// have done nothing. So the scope comes from the store and the
// eligibility test is applied here, where the heading is.
//
// FORGETTING IS GATED HERE, because the store's ForgetMemory takes a
// sequence number and an actor and does not check ownership. It cannot:
// the Console forgets on an operator's behalf, and an owner may forget
// household memory. So the portal is the layer that knows this caller is
// a household member on the dashboard, and it refuses anything that is
// not theirs before the store is ever asked. The same shape as the
// notifications Clear endpoint, and for the same reason: a DB call that
// takes an id and trusts it needs somebody above it to decide whose id
// it may be.
//
// §12.2 draws the line: "a member may forget anything it learned about
// them; household memory is the owner's."

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/chickenrat17/wildstone-homelab/keystone/resource"
	"github.com/chickenrat17/wildstone-homelab/keystone/runtimedb"
)

// memoryItem is one thing the assistant has learned, as the panel shows
// it.
type memoryItem struct {
	Seq     int64     `json:"seq"`
	Content string    `json:"content"`
	At      time.Time `json:"at,omitzero"`
	// Mine marks a record about this member, which is the only kind
	// they may forget. Everything else is the household's and is shown
	// with no control — marked "shared", never a button that refuses.
	Mine bool `json:"mine"`
	// From is where it came from, in household words: a member should
	// be able to see WHY the assistant believes something, not only
	// that it does.
	From string `json:"from,omitempty"`
}

// maxMemoryShown bounds the panel. A household's memory grows without
// limit and a side panel does not.
const maxMemoryShown = 40

// assistantMemory lists what the assistant may recall for this member.
func (s *Server) assistantMemory(_ http.ResponseWriter, _ *http.Request, member string, _ []resource.Resource) (any, int, error) {
	if s.DB == nil {
		return nil, http.StatusServiceUnavailable, fmt.Errorf("no assistant is configured on this deployment")
	}
	records, err := s.DB.ExportMemoryFor(member)
	if err != nil {
		// Quarantine closes the retrieval gate, which is a real state
		// and not a fault: the household surface says it cannot read
		// rather than showing an empty list that would read as "it has
		// learned nothing about you".
		s.Log.Info("assistant memory unavailable", "member", member, "err", err)
		return nil, http.StatusServiceUnavailable, fmt.Errorf("can't read what the assistant remembers right now")
	}
	items := make([]memoryItem, 0, len(records))
	for _, m := range records {
		// ExportMemoryFor is an EXPORT, and an export deliberately
		// hands a person their own forgotten records back — they are
		// theirs to have a copy of (ADR-W030 A3). This panel is not an
		// export. It says "what the assistant remembers about you", and
		// a record the member forgot an hour ago is exactly what it
		// must not list: the surface would be telling them their own
		// forget did nothing, and offering to do it again.
		if !m.Eligible() {
			continue
		}
		if len(items) >= maxMemoryShown {
			break
		}
		items = append(items, memoryItem{
			Seq:     m.Seq,
			Content: m.Content,
			At:      m.ObservedAt,
			Mine:    strings.EqualFold(m.Owner, member) && m.Audience != runtimedb.ButlerAudienceShared,
			From:    memorySource(m),
		})
	}
	// Newest first: what it learned about you today is what you are most
	// likely to want to correct.
	for i, j := 0, len(items)-1; i < j; i, j = i+1, j-1 {
		items[i], items[j] = items[j], items[i]
	}
	return map[string]any{"items": items, "member": member}, http.StatusOK, nil
}

// memorySource says where a record came from in household language.
//
// The stored SourceKind is machine vocabulary ("butler_session",
// "household_event"), and this surface does not print machine
// vocabulary — the same rule portal.Sentence follows for the activity
// stream. An unmapped kind yields no attribution rather than the raw
// string.
func memorySource(m runtimedb.MemoryRecord) string {
	switch m.SourceKind {
	case "butler_session", "session":
		return "from something you said"
	case "household_event", "event":
		return "from something that happened at home"
	case "routine":
		return "from a routine"
	case "correction":
		return "from a correction"
	}
	return ""
}

// assistantForget makes the assistant forget one thing about this
// member.
func (s *Server) assistantForget(_ http.ResponseWriter, r *http.Request, member string, _ []resource.Resource) (any, int, error) {
	if s.DB == nil {
		return nil, http.StatusServiceUnavailable, fmt.Errorf("no assistant is configured on this deployment")
	}
	seq, err := strconv.ParseInt(strings.TrimSpace(r.URL.Query().Get("seq")), 10, 64)
	if err != nil || seq <= 0 {
		return nil, http.StatusBadRequest, fmt.Errorf("which one?")
	}
	m, err := s.DB.GetMemory(seq)
	if errors.Is(err, runtimedb.ErrNotFound) {
		return nil, http.StatusNotFound, fmt.Errorf("there is nothing here to forget")
	}
	if err != nil {
		return nil, http.StatusInternalServerError, err
	}
	// Somebody else's record, and the household's shared memory, read
	// as NOT FOUND rather than forbidden — the same shape every other
	// refusal on this surface takes, because "that is not yours"
	// confirms the record exists and whose it is.
	if !strings.EqualFold(m.Owner, member) || m.Audience == runtimedb.ButlerAudienceShared {
		return nil, http.StatusNotFound, fmt.Errorf("there is nothing here to forget")
	}
	if err := s.DB.ForgetMemory(seq, member); err != nil {
		s.Log.Error("assistant forget", "seq", seq, "err", err)
		return nil, http.StatusInternalServerError, fmt.Errorf("that didn't go through")
	}
	return map[string]any{"forgotten": seq}, http.StatusOK, nil
}
