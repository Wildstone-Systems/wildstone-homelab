package api

// Commitments on the household's own front page (ADR-W035 K5).
//
// They get no surface of their own, which is the decision A19 turned on.
// The Home page already has a card for "things waiting on somebody" and
// an Activity record for "things the house did", and a tracker that
// lives on a page the household already opens is the difference between
// one people pass every day and one they have to remember to visit.
//
// WHO MAY ANSWER is the question this file exists to get right. A
// commitment is ABOUT somebody — a child's checkup — and may be answered
// by somebody else — a parent. Rendering an item the member cannot act
// on as read-only rather than refusing them after they try is the
// homepage's own rule, and `decidable` is where the two designs meet.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/chickenrat17/wildstone-homelab/keystone/commitment"
	"github.com/chickenrat17/wildstone-homelab/keystone/portal"
	"github.com/chickenrat17/wildstone-homelab/keystone/resource"
	"github.com/chickenrat17/wildstone-homelab/keystone/runtimedb"
	"github.com/chickenrat17/wildstone-homelab/keystone/upkeep"
)

// who is the person a commitment question is being asked about: the
// household name the door canonicalised, and the directory groups their
// session arrived with.
//
// THE GROUPS TRAVEL WITH THE MEMBER, and this type exists so that they
// cannot be left behind. `mayComplete` may name a group, and a check
// that had only the name in hand could do nothing with `group/parents`
// but guess — which it did, permissively, so every member of the
// household could answer a commitment restricted to the adults. Making
// the pair one value means a caller that forgets the groups does not
// compile.
//
// They come from the same forwarded header the door authenticated
// against, so a member added to a group after they signed in is treated
// as the group they signed in with. That is the rule every other group
// check on this surface already follows.
type who struct {
	member string
	groups []string
}

// memberWho reads the groups the identity proxy forwarded for a request
// the door has already admitted.
func memberWho(r *http.Request, member string) who {
	return who{member: member, groups: forwardedGroups(r)}
}

func forwardedGroups(r *http.Request) []string {
	if r == nil {
		return nil
	}
	var out []string
	for _, g := range strings.Split(r.Header.Get(ConsoleGroupsHeader), ",") {
		if g = strings.TrimSpace(g); g != "" {
			out = append(out, g)
		}
	}
	return out
}

// commitmentItems are the Needs-you rows a member's commitments produce.
//
// Only the ones that want something. A commitment quietly counting down
// is not waiting on anybody, and putting it here would fill the one card
// a household reads with things it does not have to do.
func (s *Server) commitmentItems(w who, desired []resource.Resource, now time.Time) ([]needsYouItem, error) {
	if s.Upkeep == nil {
		return nil, nil
	}
	standings, err := s.Upkeep.Evaluate()
	if err != nil {
		return nil, err
	}
	units := meterUnits(desired)
	var items []needsYouItem
	for _, st := range standings {
		if st.Disabled || !commitmentWants(st) {
			continue
		}
		if !visibleTo(st, w) {
			continue
		}
		items = append(items, needsYouItem{
			Ref:         st.Name,
			Kind:        needsYouCommitment,
			Summary:     st.Description,
			WaitingOn:   waitingOn(st, w),
			Waited:      humanSince(st.Occurrence.OpenedAt, now),
			Consequence: commitmentConsequence(st),
			Decidable:   mayComplete(st, w),
			Tone:        commitmentTone(st),
		})
		items[len(items)-1].Asks, items[len(items)-1].Needs =
			commitmentAsks(st, mayComplete(st, w), units)
	}
	return items, nil
}

// commitmentAsks is what the card offers a member who may answer, and
// what numbers it must collect to offer it honestly.
//
// THE PAGE DOES NOT WORK THIS OUT, AND NEITHER DOES THIS FUNCTION. It
// asks `upkeep.Missing` — the same function `upkeep.Evidence` refuses
// on — so an affordance the write path would reject cannot be
// rendered. Deciding it here independently is what went wrong twice:
// Done offered on a stale-but-overdue odometer that every tap returned
// 400 for, and a reading offered alone on a commitment wedged by a
// pre-0042 completion, which a reading cannot clear.
//
// Every item therefore offers a way to CLOSE the cycle. A card that
// asks a household for something and gives them no way to answer it is
// worse than one that never asked.
func commitmentAsks(st upkeep.Standing, may bool, units map[string]string) ([]needsYouAsk, []needsYouMeter) {
	if !may {
		// The affordance is absent for somebody who cannot act, never
		// present-and-refused. That is the homepage's own rule.
		return nil, nil
	}
	// Both the numbers without which nothing can close, and the ones the
	// engine would otherwise guess from a reading too old to rely on. The
	// card asks for both; only the first kind blocks the write, which is
	// why a household that ignores the box can still answer the item.
	var needs []needsYouMeter
	for _, meter := range append(upkeep.Missing(st, nil), upkeep.Wanted(st, nil)...) {
		needs = append(needs, needsYouMeter{Meter: meter, Unit: units[meter]})
	}
	// Saving the number without closing anything is its own answer, and
	// it belongs to the METER rather than to any one commitment — the
	// card collects it once and offers one Save. An ask per commitment
	// would put three Saves under one odometer.
	asks := []needsYouAsk{{Verb: "done", Label: "Done"}}
	// An appointment is only an answer for something that needs one, and
	// only while there is not one already: a booked cycle is not on this
	// card at all.
	if st.RequiresBooking {
		asks = append(asks, needsYouAsk{Verb: "booked", Label: "Booked it"})
	}
	return append(asks, needsYouAsk{Verb: "waived", Label: "Skip it"}), needs
}

// meterUnits is the declared unit for each meter, so the card can ask
// for "miles" rather than for a bare number.
func meterUnits(desired []resource.Resource) map[string]string {
	out := map[string]string{}
	for _, res := range desired {
		if res.Kind == resource.KindMeter && res.Meter != nil {
			out[res.Metadata.Name] = res.Meter.Unit
		}
	}
	return out
}

// commitmentWants reports whether a commitment is asking for anything.
func commitmentWants(st upkeep.Standing) bool {
	if len(st.Occurrence.Unresolved) > 0 && st.Occurrence.Trigger == "" {
		return true
	}
	switch st.Occurrence.State {
	case commitment.StateDueSoon, commitment.StateOverdue:
		return true
	case commitment.StateScheduled:
		// Booked and in hand. The household has done the thing it can do,
		// and a card that keeps asking after somebody has answered is
		// how a card stops being read.
		return false
	}
	return false
}

// visibleTo decides whether this member sees the item at all.
//
// A personal commitment is the owner's business and nobody else's — the
// same boundary W033's memory scopes draw, using the same words rather
// than a second vocabulary for the same question.
func visibleTo(st upkeep.Standing, w who) bool {
	if st.Scope != resource.CommitmentScopePersonal {
		return true
	}
	return strings.TrimPrefix(st.Owner, "household/") == w.member || mayComplete(st, w)
}

// mayComplete is the `decidable` question: may THIS member answer it?
//
// An unstated mayComplete means different things at the two scopes, and
// collapsing them is a bug this had before a test found it. A household
// commitment nobody narrowed is anybody's to answer — the bins, the
// filter, the truck. A PERSONAL one nobody narrowed is its owner's, or
// personal scope would mean nothing at all: a prescription everybody may
// tick off is not private, whatever the field says.
//
// Neither default is "the adults". W033 A1 makes `child` a Member
// profile rather than a role, so there is no adults-versus-children
// vocabulary here to lean on, and inventing one in this file would put
// it in the wrong place. A household wanting it narrower names who.
func mayComplete(st upkeep.Standing, w who) bool {
	if len(st.MayComplete) == 0 {
		if st.Scope == resource.CommitmentScopePersonal {
			return strings.TrimPrefix(st.Owner, "household/") == w.member
		}
		return true
	}
	for _, p := range st.MayComplete {
		if strings.TrimPrefix(p, "household/") == w.member {
			return true
		}
		// `household` is everybody who got through the door, and the
		// door is the household boundary.
		if p == "household" {
			return true
		}
		// A GROUP IS ANSWERED, NOT ASSUMED. This branch used to return
		// true for any group at all, on the reasoning that an affordance
		// wrongly offered would be refused by the write path — but the
		// write path calls this same function, so nothing refused it and
		// `mayComplete: [group/parents]` was a comment rather than a
		// rule (Codex, #244).
		//
		// The groups the proxy forwarded are the same ones the door
		// authenticated against, so the question is answerable here and
		// does not need a directory read. A group nobody is in denies,
		// which is the safe direction: a household that narrows a
		// commitment to a group it has not created has narrowed it to
		// nobody, and would rather find that out by nobody being able to
		// tick it off than by everybody being able to.
		if g, ok := strings.CutPrefix(p, "group/"); ok && slices.Contains(w.groups, g) {
			return true
		}
	}
	return false
}

func waitingOn(st upkeep.Standing, w who) string {
	if mayComplete(st, w) {
		return "you"
	}
	if owner := strings.TrimPrefix(st.Owner, "household/"); owner != st.Owner && owner != "" {
		return owner
	}
	return "somebody"
}

// commitmentConsequence says what has not happened yet, so a member can
// judge whether it matters now.
func commitmentConsequence(st upkeep.Standing) string {
	occ := st.Occurrence
	if len(occ.Unresolved) > 0 && occ.Trigger == "" {
		return "Nobody can tell whether this is due until the number is read."
	}
	switch occ.State {
	case commitment.StateOverdue:
		if st.Occurrence.PastDue {
			return "It is past its date and the appointment has gone."
		}
		return "It is past its date and nothing has been recorded."
	case commitment.StateDueSoon:
		if st.MayComplete != nil || st.Occurrence.Trigger != "" {
			return "Nothing has happened yet."
		}
	}
	return "Nothing has happened yet."
}

func commitmentTone(st upkeep.Standing) portal.Tone {
	if st.Occurrence.State == commitment.StateOverdue {
		return portal.ToneFailed
	}
	return portal.ToneWaiting
}

// --- the answer path -------------------------------------------------

type commitmentAnswer struct {
	// Done, Waived and Booked are the three things a member can say.
	// Exactly one per request; the route says which.
	At     *time.Time         `json:"at,omitempty"`
	Reason string             `json:"reason,omitempty"`
	Note   string             `json:"note,omitempty"`
	Meters map[string]float64 `json:"meters,omitempty"`
}

// answerCommitment records that a member did it, skipped it, or booked
// it. The route carries which; this carries the evidence.
func (s *Server) answerCommitment(what string) portalFunc {
	return func(_ http.ResponseWriter, r *http.Request, member string, _ []resource.Resource) (any, int, error) {
		name := r.PathValue("name")
		w := memberWho(r, member)
		st, code, err := s.commitmentFor(name, w)
		if err != nil {
			return nil, code, err
		}
		if !mayComplete(st, w) {
			// Reached only by somebody bypassing the page, since the
			// affordance is absent rather than present-and-refused.
			return nil, http.StatusForbidden, fmt.Errorf("%q is not yours to answer", name)
		}
		var req commitmentAnswer
		if err := json.NewDecoder(http.MaxBytesReader(nil, r.Body, maxReadingBytes)).Decode(&req); err != nil && err.Error() != "EOF" {
			return nil, http.StatusBadRequest, fmt.Errorf("parsing answer: %w", err)
		}
		at := time.Now().UTC()
		if req.At != nil {
			at = req.At.UTC()
		}
		if at.After(time.Now().Add(maxReadingSkew)) && what != "booked" {
			return nil, http.StatusBadRequest, fmt.Errorf("that is in the future")
		}

		switch what {
		case "done", "waived":
			positions, approx, perr := upkeep.Evidence(st, req.Meters)
			if perr != nil {
				return nil, http.StatusBadRequest, perr
			}
			seq, _, err := s.DB.RecordCommitmentCompletion(runtimedb.CommitmentCompletion{
				Commitment: name, At: at, RecordedBy: member,
				Waived: what == "waived", Reason: req.Reason, Positions: positions,
				Approximate: approx,
			})
			if err != nil {
				return nil, http.StatusInternalServerError, err
			}
			s.pokeUpkeep()
			return map[string]any{"seq": seq}, http.StatusCreated, nil
		case "booked":
			if req.At == nil {
				return nil, http.StatusBadRequest, fmt.Errorf("when is the appointment?")
			}
			seq, err := s.DB.RecordCommitmentBooking(runtimedb.CommitmentBooking{
				Commitment: name, At: at, BookedBy: member, Note: req.Note,
			})
			if err != nil {
				return nil, http.StatusInternalServerError, err
			}
			s.pokeUpkeep()
			return map[string]any{"seq": seq}, http.StatusCreated, nil
		}
		return nil, http.StatusBadRequest, fmt.Errorf("unknown answer %q", what)
	}
}

// homeMeterReading lets a member answer the other kind of question: not
// "did you do it" but "what does it say".
func (s *Server) homeMeterReading(_ http.ResponseWriter, r *http.Request, member string, desired []resource.Resource) (any, int, error) {
	name := r.PathValue("name")
	declared := slices.ContainsFunc(desired, func(res resource.Resource) bool {
		return res.Kind == resource.KindMeter && res.Metadata.Name == name
	})
	if !declared {
		return nil, http.StatusNotFound, fmt.Errorf("no meter named %q", name)
	}
	var req struct {
		Value float64    `json:"value"`
		At    *time.Time `json:"at,omitempty"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(nil, r.Body, maxReadingBytes)).Decode(&req); err != nil {
		return nil, http.StatusBadRequest, fmt.Errorf("parsing reading: %w", err)
	}
	if req.Value < 0 {
		return nil, http.StatusBadRequest, fmt.Errorf("a meter counts up from zero")
	}
	at := time.Now().UTC()
	if req.At != nil {
		at = req.At.UTC()
	}
	if at.After(time.Now().Add(maxReadingSkew)) {
		return nil, http.StatusBadRequest, fmt.Errorf("that reading is in the future")
	}
	seq, duplicate, err := s.DB.AppendMeterReading(runtimedb.MeterReading{
		Meter: name, Value: req.Value, At: at,
		RecordedBy: member, Source: runtimedb.ReadingByPerson,
	})
	if err != nil {
		return nil, http.StatusInternalServerError, err
	}
	s.pokeUpkeep()
	return map[string]any{"seq": seq, "duplicate": duplicate}, http.StatusCreated, nil
}

// commitmentFor finds a declared commitment a member may see.
func (s *Server) commitmentFor(name string, w who) (upkeep.Standing, int, error) {
	if s.Upkeep == nil {
		return upkeep.Standing{}, http.StatusServiceUnavailable, fmt.Errorf("this page is not ready yet")
	}
	standings, err := s.Upkeep.Evaluate()
	if err != nil {
		return upkeep.Standing{}, http.StatusInternalServerError, err
	}
	for _, st := range standings {
		if st.Name != name {
			continue
		}
		if !visibleTo(st, w) {
			// Absent rather than forbidden: a personal commitment
			// somebody else cannot see should not be confirmed to exist
			// by the shape of the refusal.
			break
		}
		return st, 0, nil
	}
	return upkeep.Standing{}, http.StatusNotFound, fmt.Errorf("no commitment named %q", name)
}

func (s *Server) pokeUpkeep() {
	if s.OnMeterReading != nil {
		s.OnMeterReading()
	}
}

// needsYouCommitment is the Kind a commitment item carries, so a client
// knows which answer path the row belongs to.
const needsYouCommitment = "commitment"
