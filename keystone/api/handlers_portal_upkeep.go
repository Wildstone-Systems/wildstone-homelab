package api

// The household's own tracker page (ADR-W035 A19, amended).
//
// A19 said commitments get no surface of their own, and its argument
// was narrower than it read: *"the difference between a tracker people
// pass every day and one they have to remember to visit."* That is an
// argument against REPLACING the Needs-you card with a page, not
// against a page the card links to — and the portal already works that
// way for Activity, Files and the Assistant, each with a card that
// summarises and a page that holds the rest.
//
// THE CARD IS STILL THE FRONT DOOR. It keeps its rows and its buttons;
// this page is where a household looks at everything the card
// deliberately hides, which is most of it. `commitmentWants` filters
// the card to things asking for something, so a commitment quietly
// counting down was visible to nobody but an Owner in the Console. A
// truck at 3,000 of 5,000 miles is the normal case, and it had no home.
//
// THE GROUPING IS THE DATA MODEL'S, NOT A SECOND ONE. A meter is a
// number the household keeps; a commitment is a thing measured against
// it. One odometer feeds the oil, the tyres and the brakes — so the
// page groups by meter, the truck reads as one thing, and the number is
// entered once however many things are measured against it.

import (
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/chickenrat17/wildstone-homelab/keystone/commitment"
	"github.com/chickenrat17/wildstone-homelab/keystone/portal"
	"github.com/chickenrat17/wildstone-homelab/keystone/resource"
	"github.com/chickenrat17/wildstone-homelab/keystone/runtimedb"
	"github.com/chickenrat17/wildstone-homelab/keystone/upkeep"
)

// upkeepPage is everything the tracker page renders.
type upkeepPage struct {
	homeCard
	// Tracked are the meters and what is measured against each.
	Tracked []trackedMeter `json:"tracked"`
	// Calendar are the commitments with no counter behind them: the
	// checkup, the passports, the dentist.
	Calendar []commitmentRow `json:"calendar"`
	// Done are finished one-offs and waived cycles. A repeating
	// commitment never lands here — it opens its next cycle instead, and
	// its history lives on its own row.
	Done []commitmentRow `json:"done"`
	// People are who a new commitment can be about: the household's
	// admitted members, children included, by the handle `about` takes.
	// Offered as suggestions, not a closed list — a commitment about
	// somebody the household has not declared is still a commitment.
	People []string `json:"people"`
}

// trackedMeter is one number the household keeps, and the things
// measured against it.
type trackedMeter struct {
	Meter meterView `json:"meter"`
	// Wanted is true when this meter's number would help: never read, or
	// too old for anything measured against it to conclude from. It is
	// the page's cue to show the box filled rather than empty.
	Wanted      bool            `json:"wanted,omitempty"`
	Commitments []commitmentRow `json:"commitments"`
}

// commitmentRow is one commitment as the family page shows it.
//
// Deliberately not upkeep.Standing. A Standing carries the engine's
// whole working — every rule's anchor, lead, progress and coordinate
// space — and this surface says what a household needs in its own
// words. Handing the raw verdict to the page would put the engine's
// vocabulary on a wall tablet.
type commitmentRow struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Owner       string `json:"owner,omitempty"`
	Personal    bool   `json:"personal,omitempty"`
	// Paused is a commitment the household told the engine to stop
	// evaluating. It is shown rather than hidden: a tracker that
	// silently omits something is one a household cannot trust to be
	// complete.
	Paused bool `json:"paused,omitempty"`
	// Standing is the state in household words: "past its date",
	// "coming up", "booked", "can't tell".
	Standing string `json:"standing"`
	// Tone drives colour, and colour is never the only signal — every
	// row says its standing in words too.
	Tone portal.Tone `json:"tone"`
	// Detail is the one line under the name: where it stands, in the
	// units the household thinks in.
	Detail string `json:"detail,omitempty"`
	// Asks are the answers on offer, from the same function the Needs-you
	// card uses, so the two surfaces cannot disagree about what may be
	// answered here.
	Asks []needsYouAsk `json:"asks,omitempty"`
	// Needs are the meters an answer would want a number for. Empty on
	// this page's grouped half, where the meter's own box collects it.
	Needs []needsYouMeter `json:"needs,omitempty"`
	// Booking is the appointment, when there is one.
	BookedFor *time.Time `json:"bookedFor,omitempty"`
	// Cycles is how many have closed, so a row can say "4th time" without
	// the page fetching the history to count.
	Cycles int `json:"cycles"`
}

// homeUpkeep builds the tracker page.
func (s *Server) homeUpkeep(_ http.ResponseWriter, r *http.Request, member string, desired []resource.Resource) (any, int, error) {
	page := upkeepPage{
		homeCard: homeCard{Available: true},
		Tracked:  []trackedMeter{}, Calendar: []commitmentRow{}, Done: []commitmentRow{}, People: []string{},
	}
	if s.Upkeep == nil {
		// The same sentence every card on this surface uses for "cannot
		// tell". Silence and all-clear must not look the same.
		page.Available = false
		page.Problem = "Can't tell what the house is keeping track of — Wildstone isn't answering."
		return page, http.StatusOK, nil
	}
	w := memberWho(r, member)
	standings, err := s.Upkeep.Evaluate()
	if err != nil {
		s.Log.Error("home upkeep", "err", err)
		page.Available = false
		page.Problem = "Can't tell what the house is keeping track of — Wildstone isn't answering."
		return page, http.StatusOK, nil
	}
	units := meterUnits(desired)
	counts, err := s.completionCounts()
	if err != nil {
		return nil, http.StatusInternalServerError, err
	}

	// Which meter does each commitment belong under? Its first declared
	// counter. A commitment measured against two of them is rare and has
	// no single home, so it is filed under the first and shows every
	// threshold when a member opens it — listing it twice would make the
	// counts beside each meter lie.
	byMeter := map[string][]commitmentRow{}
	for _, st := range standings {
		if !visibleTo(st, w) {
			continue
		}
		row := s.commitmentRowFor(st, w, units, counts[st.Name])
		switch {
		case finishedForever(st):
			page.Done = append(page.Done, row)
		default:
			if meter := firstCounter(st); meter != "" {
				byMeter[meter] = append(byMeter[meter], row)
			} else {
				page.Calendar = append(page.Calendar, row)
			}
		}
	}

	metersSeen := map[string]bool{}
	for _, res := range desired {
		if res.Kind != resource.KindMeter || res.Meter == nil {
			continue
		}
		metersSeen[res.Metadata.Name] = true
		view, verr := s.meterView(res)
		if verr != nil {
			return nil, http.StatusInternalServerError, verr
		}
		page.Tracked = append(page.Tracked, trackedMeter{
			Meter:       view,
			Wanted:      view.Latest == nil || view.Stale,
			Commitments: sortRows(byMeter[res.Metadata.Name]),
		})
	}
	// A commitment measured against a meter nobody declared cannot be
	// grouped and must not vanish. It reads as a calendar row whose
	// detail says what is missing, which is the honest rendering: the
	// household still owes the thing, and nothing can say when.
	for meter, rows := range byMeter {
		if !metersSeen[meter] {
			page.Calendar = append(page.Calendar, rows...)
		}
	}
	sort.Slice(page.Tracked, func(i, j int) bool {
		return page.Tracked[i].Meter.Name < page.Tracked[j].Meter.Name
	})
	page.Calendar = sortRows(page.Calendar)
	page.Done = sortRows(page.Done)
	page.People = s.householdPeople()
	return page, http.StatusOK, nil
}

// householdPeople is every admitted, current member's handle, sorted.
// A failure to read them costs the form its suggestions, not the page.
func (s *Server) householdPeople() []string {
	out := []string{}
	if s.DB == nil {
		return out
	}
	members, err := s.DB.ListButlerMembers()
	if err != nil {
		s.Log.Warn("home upkeep: listing people", "err", err)
		return out
	}
	for _, m := range members {
		if m.Member == "" || !m.OffboardedAt.IsZero() || !m.DisabledAt.IsZero() {
			continue
		}
		// ADMITTED, OR A CHILD. The doc comment above has always said
		// "children included" and this filter could never include one:
		// AdmitButlerMember refuses a child record by every path
		// (ADR-W030 Part C), so a child is admitted=0 forever and was
		// silently dropped here. A child is exactly the person a
		// household wants to name on a commitment, and ADR-W033 A8 is
		// the statement that being scheduled does not require being
		// able to sign in.
		if m.Admitted || m.Profile == runtimedb.ProfileChild {
			out = append(out, m.Member)
		}
	}
	sort.Strings(out)
	return out
}

// finishedForever is a commitment that has been answered and will not
// come round again. It belongs in Done rather than among things coming
// up.
//
// THE STATE IS THE WHOLE TEST, and a rollover check beside it would be
// dead code. `satisfied` and `waived` are reachable only through
// `closedOnce`, which the engine gates on `Rollover == Once` — a
// repeating commitment never reports either, because closing its cycle
// opens the next one. An extra `Rollover != Once` here would read like
// a second condition while being true whenever the first one was, which
// is the shape that makes a test look load-bearing when it is not.
func finishedForever(st upkeep.Standing) bool {
	return st.Occurrence.State == commitment.StateSatisfied ||
		st.Occurrence.State == commitment.StateWaived
}

// firstCounter is the meter a commitment is filed under, or "" for one
// measured only by the calendar.
func firstCounter(st upkeep.Standing) string {
	for _, rule := range st.Occurrence.Rules {
		if !rule.Calendar && rule.Meter != resource.MeterClock {
			return rule.Meter
		}
	}
	return ""
}

// sortRows puts what needs attention first and settles the rest by
// name, so a refresh does not reshuffle the page under somebody's
// finger.
func sortRows(rows []commitmentRow) []commitmentRow {
	sort.SliceStable(rows, func(i, j int) bool {
		a, b := toneRank(rows[i].Tone), toneRank(rows[j].Tone)
		if a != b {
			return a < b
		}
		return rows[i].Name < rows[j].Name
	})
	if rows == nil {
		return []commitmentRow{}
	}
	return rows
}

// completionCounts is how many cycles each commitment has closed, so a
// row can say "4th time" without the page fetching a history to count.
func (s *Server) completionCounts() (map[string]int, error) {
	return s.DB.CommitmentCycleCounts()
}

// standingDetail is the one line under a commitment's name: where it
// stands, in the units the household thinks in rather than the
// engine's coordinate space.
func standingDetail(st upkeep.Standing, units map[string]string) string {
	occ := st.Occurrence
	if len(occ.Unresolved) > 0 && occ.Trigger == "" {
		return "nobody has read " + strings.Join(occ.Unresolved, " or ")
	}
	// The rule that decided it, or the nearest one when nothing has.
	var chosen *commitment.RuleStatus
	for i := range occ.Rules {
		rule := &occ.Rules[i]
		if occ.Trigger != "" && rule.Meter == occ.Trigger {
			chosen = rule
			break
		}
		if rule.Known && (chosen == nil || rule.Remaining < chosen.Remaining) {
			chosen = rule
		}
	}
	if chosen == nil {
		return ""
	}
	if chosen.Calendar {
		when := chosen.DueTime.Format("2 Jan 2006")
		if chosen.Crossed {
			return "was due " + when
		}
		return "due " + when
	}
	unit := units[chosen.Meter]
	if unit == "" {
		unit = "units"
	}
	if chosen.Crossed {
		return fmt.Sprintf("was due at %s %s — %s past",
			number(chosen.DueAt), unit, number(chosen.Position-chosen.DueAt))
	}
	line := fmt.Sprintf("due at %s %s — %s to go",
		number(chosen.DueAt), unit, number(chosen.DueAt-chosen.Position))
	// A projection is a guess and says so, every time it is shown.
	if occ.Projection != nil {
		line += ", around " + occ.Projection.At.Format("Jan 2006")
	}
	return line
}

// number renders a meter position the way a household writes one.
func number(v float64) string {
	return strconv.FormatFloat(v, 'f', -1, 64)
}

// commitmentRowFor renders one standing as the family page says it.
//
// The Asks come from the same function the Needs-you card uses, so the
// two surfaces cannot offer different answers for one commitment — the
// fault that put "Done" on a card where the write path refused it.
func (s *Server) commitmentRowFor(st upkeep.Standing, w who, units map[string]string, cycles int) commitmentRow {
	row := commitmentRow{
		Name: st.Name, Description: st.Description, Owner: st.Owner, Paused: st.Disabled,
		Personal: st.Scope == resource.CommitmentScopePersonal,
		Standing: standingWords(st), Tone: pausedTone(st),
		Detail: standingDetail(st, units), Cycles: cycles,
	}
	if st.Booking != nil && st.Booking.Live() {
		at := st.Booking.At
		row.BookedFor = &at
	}
	if !st.Disabled {
		// Nothing to answer on something the house has been told to stop
		// asking about. The affordance is absent, never
		// present-and-refused — the same rule the Needs-you card follows.
		row.Asks, row.Needs = commitmentAsks(st, mayComplete(st, w), units)
	}
	return row
}

// pausedTone keeps a paused commitment from wearing an alarm colour.
func pausedTone(st upkeep.Standing) portal.Tone {
	if st.Disabled {
		return portal.ToneRefused
	}
	return commitmentTone(st)
}

// standingWords is the state in household language. The engine's own
// state names — due_soon, overdue — are not what a family calls them.
func standingWords(st upkeep.Standing) string {
	// A paused commitment is not evaluated AT the household, so its
	// verdict is whatever was true when it was paused. Rendering that as
	// "past its date" tells somebody to act on a thing the house has
	// been told to stop asking about.
	if st.Disabled {
		return "paused"
	}
	if len(st.Occurrence.Unresolved) > 0 && st.Occurrence.Trigger == "" {
		return "can't tell"
	}
	switch st.Occurrence.State {
	case commitment.StateOverdue:
		if st.Occurrence.PastDue {
			return "the appointment has gone"
		}
		return "past its date"
	case commitment.StateDueSoon:
		return "coming up"
	case commitment.StateScheduled:
		return "booked"
	case commitment.StateSatisfied:
		return "done"
	case commitment.StateWaived:
		return "skipped on purpose"
	}
	return "nothing due yet"
}

// --- one commitment's history ----------------------------------------

// cycleRow is one closed cycle as the household reads it.
type cycleRow struct {
	At          time.Time `json:"at"`
	By          string    `json:"by"`
	Waived      bool      `json:"waived,omitempty"`
	Reason      string    `json:"reason,omitempty"`
	Note        string    `json:"note,omitempty"`
	FromEvent   bool      `json:"fromEvent,omitempty"`
	Position    *float64  `json:"position,omitempty"`
	Unit        string    `json:"unit,omitempty"`
	Approximate bool      `json:"approximate,omitempty"`
	// Since is the interval actually achieved: how far the meter moved,
	// or how many days passed, since the cycle before this one.
	//
	// THE MOST USEFUL NUMBER HERE AND THE ONLY DERIVED ONE. A household
	// that declared "every 5,000 miles" and is actually averaging 5,400
	// cannot learn that from the schedule — only from what it did. It is
	// absent on the first cycle, which has nothing to measure from.
	Since string `json:"since,omitempty"`
}

type commitmentHistoryPage struct {
	homeCard
	Row    commitmentRow `json:"row"`
	Cycles []cycleRow    `json:"cycles"`
}

// homeCommitmentHistory is one commitment's closed cycles, newest
// first, for a member who may see it.
func (s *Server) homeCommitmentHistory(_ http.ResponseWriter, r *http.Request, member string, desired []resource.Resource) (any, int, error) {
	name := r.PathValue("name")
	w := memberWho(r, member)
	st, code, err := s.commitmentFor(name, w)
	if err != nil {
		return nil, code, err
	}
	done, err := s.DB.ListCommitmentCompletions(name)
	if err != nil {
		return nil, http.StatusInternalServerError, err
	}
	units := meterUnits(desired)
	meter := firstCounter(st)
	page := commitmentHistoryPage{
		homeCard: homeCard{Available: true},
		Row:      s.commitmentRowFor(st, w, units, len(done)),
		Cycles:   []cycleRow{},
	}
	// Oldest first from the store, so each row can see the one before
	// it; reversed at the end because a history is read newest first.
	for i, c := range done {
		row := cycleRow{
			At: c.At, By: c.RecordedBy, Waived: c.Waived,
			Reason: c.Reason, Note: c.Note,
			FromEvent: c.ExternalID != "",
		}
		if meter != "" {
			if v, ok := c.Positions[meter]; ok {
				row.Position, row.Unit = &v, units[meter]
				for _, a := range c.Approximate {
					if a == meter {
						row.Approximate = true
					}
				}
				if i > 0 {
					if prev, had := done[i-1].Positions[meter]; had {
						row.Since = number(v-prev) + " " + row.Unit + " since last"
					}
				}
			}
		} else if i > 0 {
			days := int(c.At.Sub(done[i-1].At).Hours() / 24)
			row.Since = fmt.Sprintf("%d days since last", days)
		}
		page.Cycles = append(page.Cycles, row)
	}
	for i, j := 0, len(page.Cycles)-1; i < j; i, j = i+1, j-1 {
		page.Cycles[i], page.Cycles[j] = page.Cycles[j], page.Cycles[i]
	}
	return page, http.StatusOK, nil
}
