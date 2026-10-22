package api

// The household authors its own tracker (ADR-W035 A20, extended).
//
// WHY THIS IS NOT A PLAN A MEMBER WAITS FOR. Applying a Meter or a
// Commitment is a no-op by construction — the reconciler says so in as
// many words, and the switch that would handle them is marked
// unreachable. Nothing starts, nothing is written to a node, nothing is
// spent. The blast radius of a wrong commitment is a reminder about the
// wrong thing.
//
// That is why they were in the same bucket as `Automation` and should
// not have been: an Automation makes the house DO something, which is
// what earns review. A commitment cannot act. Owner approval fell out
// of a chain — statefile changes go through Plans, Plans are approved
// in Console, Console is Owner-only — and nobody decided a Member
// should not be able to add "change the furnace filter".
//
// WHAT IS KEPT. The document still lands in the Statefile, so it is
// backed up, diffable and reviewable, and the agent can still draft one
// from a sentence. It still goes through the ONE proposal pipeline's
// checks — schema validation and standing policy — because not needing
// approval is not the same as not being checked. And it still leaves a
// plan record naming the member, so the household's history says who
// added what.
//
// THE STRUCTURAL GUARANTEE IS THAT THIS FILE BUILDS RESOURCES FROM
// TYPED FIELDS. A member sends a description and an interval; they
// never send a document. There is no path here that turns a request
// body into a Kind, so this door cannot write a Container, a Secret or
// a Route however the JSON is shaped — which is the property a test
// pins rather than a reviewer remembering.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/Wildstone-Systems/wildstone-homelab/keystone/plan"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/policy"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/principal"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/resource"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/runtimedb"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/upkeep"
)

const maxAuthoringBytes = 16 << 10

// authored is one household-authored change: what to put, what to
// remove, and the sentence the plan record carries.
type authored struct {
	Put     []resource.Resource
	Delete  []plan.DeletionRef
	Summary string
	// BasedOn is the Statefile head this change was computed against —
	// `basedOnHead(r)`, the head the portal read the member's view of
	// the house at. Apply refuses to commit if the head has moved since.
	//
	// There is no "unset" value. "" is what Head() reports for a
	// household whose Statefile has no commits yet, so treating it as
	// "skip the check" would turn the very first race into the silent
	// overwrite this field exists to stop.
	BasedOn string
}

// apply runs the household's change through the same checks a proposal
// faces and commits it in one act.
//
// It does NOT reuse submitProposal, and the reason is worth stating: a
// proposal's job is to produce a record a human approves later, and
// half of that pipeline exists to make the wait safe — the based-on
// head, the supersede-on-drift, the proposed state. There is no wait
// here, so reusing it would mean recording a proposal and immediately
// approving it on the caller's behalf, which reads in the audit log as
// a member approving their own plan. What matters is that the CHECKS
// are the same ones, and those are called directly below.
func (s *Server) applyAuthored(member string, ch authored) (int, error) {
	if s.State == nil || s.DB == nil {
		return http.StatusServiceUnavailable, fmt.Errorf("this page is not ready yet")
	}
	// THE CEILING, ENFORCED RATHER THAN INTENDED. Every put is built in
	// this file from typed fields, and a static test pins that no other
	// Kind constant is even named here — but a DELETION carries its kind
	// as a STRING, and a string conversion is exactly the place a static
	// guarantee stops holding. So both halves are checked here, at the
	// one point every authored change passes through.
	for _, res := range ch.Put {
		if !familyMayWrite(res.Kind) {
			return http.StatusForbidden, fmt.Errorf("the family page does not write %s", res.Kind)
		}
	}
	for _, d := range ch.Delete {
		if !familyMayWrite(resource.Kind(d.Kind)) {
			return http.StatusForbidden, fmt.Errorf("the family page does not remove %s", d.Kind)
		}
	}
	for _, res := range ch.Put {
		// The same validation submitProposal runs, on a document this
		// file built rather than one a member sent.
		if err := res.Validate(); err != nil {
			return http.StatusBadRequest, err
		}
	}
	current, err := s.State.Load()
	if err != nil {
		return http.StatusInternalServerError, err
	}
	desired := applyChanges(current, ch.Put, ch.Delete)

	// Standing policy, on the resulting state. A member does not need
	// approval; they do not get to leave the household's invariants
	// behind either.
	//
	// Role is empty rather than borrowed. Only "butler" changes what
	// policy does, and it NARROWS — so the strictest general path is
	// the honest one for somebody whose authority this file is defining
	// rather than inheriting.
	who := "household/" + member
	violations, exempted := s.Policy.Validate(policy.Change{
		Proposed: desired, Current: current, Principal: who,
	})
	id := principal.Identity{Name: who}
	for _, v := range exempted {
		s.event("policy.exempted", "", id, v.String())
	}
	if len(violations) > 0 {
		msgs := make([]string, 0, len(violations))
		for _, v := range violations {
			msgs = append(msgs, v.String())
		}
		s.event("policy.rejected", "", id, strings.Join(msgs, "; "))
		return http.StatusUnprocessableEntity, fmt.Errorf("%s", strings.Join(msgs, "; "))
	}

	p := plan.Diff(desired, current)
	if len(p.Steps) == 0 {
		return http.StatusBadRequest, fmt.Errorf("that changes nothing")
	}
	p.Summary = ch.Summary

	// The record first, so a crash between writing the Statefile and
	// recording why leaves the household with a change nobody can
	// account for rather than the other way round.
	docs := make([]string, 0, len(ch.Put))
	for _, res := range ch.Put {
		doc, merr := resource.Marshal(res)
		if merr != nil {
			return http.StatusInternalServerError, merr
		}
		docs = append(docs, string(doc))
	}
	// NOBODY WAITS, AND NOBODY LOSES A WRITE SILENTLY. Dropping the
	// approval step dropped the check that came with it: the Plans path
	// refuses to commit a plan whose Statefile moved since it was
	// computed, and this path had nothing. Two members editing the same
	// commitment both read, both wrote, and whichever landed last
	// overwrote the other — with a 200 for the one whose change
	// vanished.
	//
	// This is optimistic rather than a lock: the window narrows from the
	// whole request to these few statements, and a caller who loses is
	// told to look and try again rather than told they succeeded. A lock
	// over the Statefile is the real answer and is not this PR's to
	// introduce.
	head, err := s.State.Head()
	if err != nil {
		return http.StatusInternalServerError, err
	}
	if head != ch.BasedOn {
		return http.StatusConflict, fmt.Errorf(
			"somebody else changed what the house tracks while you were editing; take another look and try again")
	}
	stored, err := json.Marshal(storedProposal{
		BasedOnHead: head, Summary: ch.Summary, AuditOnly: true,
		Put: docs, Delete: ch.Delete, Plan: p,
	})
	if err != nil {
		return http.StatusInternalServerError, err
	}
	planID, err := s.DB.BeginHouseholdAudit(who, ch.Summary, p.OverallRisk.String(), string(stored))
	if err != nil {
		return http.StatusInternalServerError, err
	}
	applied := false
	defer func() {
		if !applied {
			_ = s.DB.TransitionPlan(planID, runtimedb.PlanFailed, who)
		}
	}()
	for _, res := range ch.Put {
		if err := s.State.Put(res); err != nil {
			return http.StatusInternalServerError, err
		}
	}
	for _, d := range ch.Delete {
		if err := s.State.Delete(resource.Kind(d.Kind), d.Name); err != nil {
			return http.StatusInternalServerError, err
		}
	}
	if err := s.State.Commit(who, planID, ch.Summary); err != nil {
		return http.StatusInternalServerError, err
	}
	newHead, err := s.State.Head()
	if err != nil {
		return http.StatusInternalServerError, err
	}
	if err := s.DB.SetStatefileHead(newHead); err != nil {
		return http.StatusInternalServerError, err
	}
	if err := s.DB.TransitionPlan(planID, runtimedb.PlanApplied, who); err != nil {
		return http.StatusInternalServerError, err
	}
	applied = true
	s.event("household.tracker.updated", planID, id, ch.Summary)
	s.pokeUpkeep()
	return http.StatusCreated, nil
}

// familyMayWrite is the whole list, and it is two.
//
// A Meter and a Commitment apply to nothing — the reconciler marks both
// unreachable — which is the entire argument for this door existing
// without an Owner behind it. Any kind that does something is not this
// door's to write, whatever a request looks like.
func familyMayWrite(kind resource.Kind) bool {
	return kind == resource.KindMeter || kind == resource.KindCommitment
}

// --- naming ----------------------------------------------------------

var notNameable = regexp.MustCompile(`[^a-z0-9]+`)

// householdName turns what a person typed into a resource name.
//
// Resource names are kebab-case and unique across the household, and
// neither is something to make a family think about. "Kai's passport"
// becomes kai-s-passport, and a second one becomes kai-s-passport-2 —
// quietly, because a collision is the system's problem and an error on
// a wall tablet reading "name already exists" is the system making it
// somebody else's.
func householdName(typed string, taken func(string) bool) string {
	base := strings.Trim(notNameable.ReplaceAllString(strings.ToLower(typed), "-"), "-")
	if base == "" {
		base = "tracked"
	}
	if len(base) > 48 {
		base = strings.Trim(base[:48], "-")
	}
	name := base
	for n := 2; taken(name); n++ {
		name = fmt.Sprintf("%s-%d", base, n)
		if n > 500 {
			// Not reachable by a household, and an unbounded loop on a
			// request path is not a thing to leave to chance.
			return fmt.Sprintf("%s-%d", base, time.Now().UnixNano())
		}
	}
	return name
}

// nameTaken reports whether a name is already spoken for — by a
// resource that exists now, OR by history under that name.
//
// THE LEDGER KEEPS THE NAME. Removing a commitment deliberately leaves
// its completions where they are: that is "stop asking me", not "this
// never happened". But the name is how they are keyed, so a household
// that removed "Furnace filter" and added it again would find the new
// one already four cycles old, measuring its next from an oil change
// that belonged to something else. The same for a meter, whose
// readings outlive it.
//
// So a name with a past is taken, and the next one gets a -2. The
// history stays findable and the new thing starts clean, which is what
// somebody adding it a second time meant either way.
func (s *Server) nameTaken(kind resource.Kind) func(string) bool {
	current, err := s.State.Load()
	if err != nil {
		// A name cannot be checked, so every name is taken: the caller
		// fails loudly rather than colliding silently.
		return func(string) bool { return true }
	}
	past := map[string]bool{}
	switch kind {
	case resource.KindCommitment:
		if counts, cerr := s.DB.CommitmentCycleCounts(); cerr == nil {
			for name := range counts {
				past[name] = true
			}
		}
	case resource.KindMeter:
		if names, nerr := s.DB.MeterNames(); nerr == nil {
			for _, name := range names {
				past[name] = true
			}
		}
	}
	return func(name string) bool {
		if past[name] {
			return true
		}
		for _, res := range current {
			if res.Kind == kind && res.Metadata.Name == name {
				return true
			}
		}
		return false
	}
}

// --- what a member may say -------------------------------------------

// meterFields is a number the household keeps, as a person describes it.
type meterFields struct {
	Description string `json:"description"`
	Unit        string `json:"unit"`
	StaleAfter  string `json:"staleAfter,omitempty"`
	Estimate    string `json:"estimate,omitempty"`
}

// commitmentFields is a thing owed, as a person describes it.
//
// `owner`, `mayComplete` and `notify` answer three different questions
// and a member may set all three. `notify` is deliberately
// unconstrained: it is the one field with any social teeth, and the
// household was trusted with it rather than given a rule that would
// stop a parent putting a child's checkup on their own list.
// commitmentEdits is an edit body, where every field is optional and
// ABSENCE MEANS PRESERVE. See homeEditCommitment for why that is not a
// convenience.
type commitmentEdits struct {
	Description     *string   `json:"description,omitempty"`
	Meter           *string   `json:"meter,omitempty"`
	Every           *string   `json:"every,omitempty"`
	Lead            *string   `json:"lead,omitempty"`
	At              *string   `json:"at,omitempty"`
	AtValue         *float64  `json:"atValue,omitempty"`
	Rollover        *string   `json:"rollover,omitempty"`
	Anchor          *string   `json:"anchor,omitempty"`
	RequiresBooking *bool     `json:"requiresBooking,omitempty"`
	Scope           *string   `json:"scope,omitempty"`
	PartOfDay       *string   `json:"partOfDay,omitempty"`
	About           *string   `json:"about,omitempty"`
	MayComplete     *[]string `json:"mayComplete,omitempty"`
	Notify          *[]string `json:"notify,omitempty"`
}

type commitmentFields struct {
	Description     string   `json:"description"`
	Meter           string   `json:"meter,omitempty"`
	Every           string   `json:"every,omitempty"`
	Lead            string   `json:"lead,omitempty"`
	At              string   `json:"at,omitempty"`
	AtValue         float64  `json:"atValue,omitempty"`
	Rollover        string   `json:"rollover"`
	Anchor          string   `json:"anchor,omitempty"`
	RequiresBooking bool     `json:"requiresBooking,omitempty"`
	Scope           string   `json:"scope,omitempty"`
	PartOfDay       string   `json:"partOfDay,omitempty"`
	About           string   `json:"about,omitempty"`
	MayComplete     []string `json:"mayComplete,omitempty"`
	Notify          []string `json:"notify,omitempty"`
}

func (f commitmentFields) spec() resource.CommitmentSpec {
	due := resource.CommitmentDue{
		Meter: f.Meter, Every: f.Every, Lead: f.Lead, At: f.At, AtValue: f.AtValue,
	}
	if due.Meter == "" {
		due.Meter = resource.MeterClock
	}
	return resource.CommitmentSpec{
		Description: f.Description, Rollover: f.Rollover, Anchor: f.Anchor,
		RequiresBooking: f.RequiresBooking, Scope: f.Scope, PartOfDay: f.PartOfDay,
		MayComplete: f.MayComplete, Notify: f.Notify,
		Due: []resource.CommitmentDue{due},
	}
}

// owner is who a commitment is ABOUT, which is not who added it. A
// member adding their child's checkup says so; one adding the bins says
// nothing and it belongs to the household.
func (f commitmentFields) owner(member string) string {
	about := strings.TrimSpace(f.About)
	if about == "" {
		if f.Scope == resource.CommitmentScopePersonal {
			// A personal commitment with nobody named is the author's.
			// Left as "household" it would be private to nobody.
			return "household/" + member
		}
		return "household"
	}
	return "household/" + strings.TrimPrefix(about, "household/")
}

// trackRequest is the thing-first flow: what you keep, what it reads
// now, and the first thing you want to keep up with — in one act, so a
// failure halfway does not leave a meter nobody asked for.
type trackRequest struct {
	Meter      meterFields       `json:"meter"`
	Reading    *float64          `json:"reading,omitempty"`
	Commitment *commitmentFields `json:"commitment,omitempty"`
}

// homeTrack declares something the household keeps track of.
func (s *Server) homeTrack(_ http.ResponseWriter, r *http.Request, member string, _ []resource.Resource) (any, int, error) {
	var req trackRequest
	if code, err := decodeAuthored(r, &req); err != nil {
		return nil, code, err
	}
	if strings.TrimSpace(req.Meter.Description) == "" {
		return nil, http.StatusBadRequest, fmt.Errorf("what are you keeping track of?")
	}
	if strings.TrimSpace(req.Meter.Unit) == "" {
		return nil, http.StatusBadRequest, fmt.Errorf("what does %s count — miles, hours, gallons?", req.Meter.Description)
	}
	name := householdName(req.Meter.Description, s.nameTaken(resource.KindMeter))
	if name == resource.MeterClock {
		// The calendar is built in and reads itself (A2). A household
		// with two of them has the interesting question of which is
		// right.
		return nil, http.StatusBadRequest, fmt.Errorf("%q is the calendar and the house already has one", resource.MeterClock)
	}
	ch := authored{
		Put: []resource.Resource{{
			ApiVersion: resource.ApiVersion, Kind: resource.KindMeter,
			Metadata: resource.Metadata{Name: name, Owner: "household"},
			Meter: &resource.MeterSpec{
				Description: req.Meter.Description, Unit: req.Meter.Unit,
				StaleAfter: req.Meter.StaleAfter, Estimate: req.Meter.Estimate,
			},
		}},
		Summary: member + " started tracking " + req.Meter.Description,
		BasedOn: basedOnHead(r),
	}
	var commitmentName string
	if req.Commitment != nil {
		res, cname, code, err := s.commitmentResource(*req.Commitment, member, name)
		if err != nil {
			return nil, code, err
		}
		commitmentName = cname
		ch.Put = append(ch.Put, res)
		ch.Summary = member + " started tracking " + req.Meter.Description +
			" and " + req.Commitment.Description
	}
	if code, err := s.applyAuthored(member, ch); err != nil {
		return nil, code, err
	}
	// The first reading after the meter exists, through the ordinary
	// intake — so declaring the truck and saying where it is are one act
	// for the household and two records for the ledger, which is what
	// they are.
	if req.Reading != nil {
		if _, _, err := s.DB.AppendMeterReading(runtimedb.MeterReading{
			Meter: name, Value: *req.Reading, At: time.Now().UTC(),
			RecordedBy: member, Source: runtimedb.ReadingByPerson,
		}); err != nil {
			return nil, http.StatusInternalServerError, err
		}
		s.pokeUpkeep()
	}
	return map[string]any{"meter": name, "commitment": commitmentName}, http.StatusCreated, nil
}

// homeAddCommitment adds a thing owed, against a meter or the calendar.
func (s *Server) homeAddCommitment(_ http.ResponseWriter, r *http.Request, member string, _ []resource.Resource) (any, int, error) {
	var f commitmentFields
	if code, err := decodeAuthored(r, &f); err != nil {
		return nil, code, err
	}
	res, name, code, err := s.commitmentResource(f, member, f.Meter)
	if err != nil {
		return nil, code, err
	}
	if code, err := s.applyAuthored(member, authored{
		Put:     []resource.Resource{res},
		Summary: member + " added " + f.Description,
		BasedOn: basedOnHead(r),
	}); err != nil {
		return nil, code, err
	}
	return map[string]any{"commitment": name}, http.StatusCreated, nil
}

// clockSpanProblem refuses the family page a calendar interval shorter
// than a day.
//
// Validation accepts Go's h/m/s spans for the clock, and "m" is minutes
// there, so "6m" — what a person types for six months — was a reminder
// every six minutes, accepted without a word. Nothing a household keeps
// up with through this page recurs in hours, so the page's door names
// the units that exist for it instead, and says what "m" would have
// meant. Counters and other callers are untouched.
func clockSpanProblem(meter string, fields map[string]string) error {
	if meter != "" && meter != resource.MeterClock {
		return nil
	}
	for _, name := range []string{"every", "lead"} {
		v := strings.TrimSpace(fields[name])
		unit := strings.TrimLeft(v, "0123456789.")
		switch unit {
		case "h", "s", "ms", "us", "ns":
			return fmt.Errorf("%s %q is shorter than a day: use days (d), weeks (w), months (mo) or years (yr)", name, v)
		case "m":
			return fmt.Errorf("%s %q would be minutes — for months, write %smo", name, v, strings.TrimSuffix(v, "m"))
		}
	}
	return nil
}

// commitmentResource builds the document from typed fields. Nothing a
// member sends becomes a Kind.
func (s *Server) commitmentResource(f commitmentFields, member, meter string) (resource.Resource, string, int, error) {
	if strings.TrimSpace(f.Description) == "" {
		return resource.Resource{}, "", http.StatusBadRequest, fmt.Errorf("what do you want to keep up with?")
	}
	if f.Rollover == "" {
		return resource.Resource{}, "", http.StatusBadRequest,
			fmt.Errorf("does this come round again? (%s)", strings.Join(resource.CommitmentRollovers(), ", "))
	}
	f.Meter = meter
	if err := clockSpanProblem(meter, map[string]string{"every": f.Every, "lead": f.Lead}); err != nil {
		return resource.Resource{}, "", http.StatusBadRequest, err
	}
	if f.Anchor == "" {
		// Tracking starts today unless somebody says otherwise. A
		// commitment with no anchor is refused by validation, and "when
		// did this start" is not a question to make a household answer
		// to add a furnace filter.
		f.Anchor = time.Now().Format("2006-01-02")
	}
	name := householdName(f.Description, s.nameTaken(resource.KindCommitment))
	return resource.Resource{
		ApiVersion: resource.ApiVersion, Kind: resource.KindCommitment,
		Metadata:   resource.Metadata{Name: name, Owner: f.owner(member)},
		Commitment: ptr(f.spec()),
	}, name, 0, nil
}

func ptr[T any](v T) *T { return &v }

func decodeAuthored(r *http.Request, into any) (int, error) {
	dec := json.NewDecoder(http.MaxBytesReader(nil, r.Body, maxAuthoringBytes))
	dec.DisallowUnknownFields()
	if err := dec.Decode(into); err != nil {
		return http.StatusBadRequest, fmt.Errorf("parsing that: %w", err)
	}
	return 0, nil
}

// --- changing and removing -------------------------------------------

// mayAuthor decides whether this member may change or remove a
// commitment that already exists.
//
// TWO CLASSES, AND THE SAME BOUNDARY `visibleTo` DRAWS. A household
// commitment is the household's: anyone may fix the interval on the
// furnace filter, and a household that has to find out whose it was
// before correcting a typo is one where nobody corrects typos. A
// PERSONAL one is its owner's, and nobody else's — including an Owner,
// which is ADR-W033's line stated three separate times: administrative
// authority and disclosure authority are not the same thing.
func mayAuthor(st upkeep.Standing, w who) bool {
	// The operator surface authors anything, for the reason it sees
	// anything: the Console already lists every commitment, and one
	// that shows a record while refusing to let the administrator
	// change it is the inconsistency rather than the safeguard.
	if w.admin {
		return true
	}
	if st.Scope != resource.CommitmentScopePersonal {
		return true
	}
	owner := strings.TrimPrefix(st.Owner, "household/")
	// A guardian authors their child's, or a child's routine could be
	// created by the person who runs the week and then edited by
	// nobody — the child cannot sign in to do it themselves (A8/A9).
	return owner == w.member || w.guards(owner)
}

// homeEditCommitment changes one that already exists.
//
// IT MERGES ONTO THE DOCUMENT THAT IS THERE rather than rebuilding it
// from the body, and that is not a refinement — the first version
// rebuilt, and a rebuild silently destroyed everything the family page
// does not show. Editing the F-150's oil would have dropped "or every
// six months", because the page has one interval box and the
// commitment has two rules. Earliest-wins is the centre of this design
// and the edit path was quietly discarding half of it.
//
// So: the page owns what the page shows, and preserves the rest.
// `disabled`, `anchorAt`, `satisfiedBy` and every due rule after the
// first survive untouched, and a commitment whose scope the body does
// not mention keeps the scope it had — otherwise correcting a typo on
// a personal commitment would publish it to the household.
func (s *Server) homeEditCommitment(_ http.ResponseWriter, r *http.Request, member string, desired []resource.Resource) (any, int, error) {
	name := r.PathValue("name")
	w := s.who(r, member)
	st, code, err := s.commitmentFor(name, w)
	if err != nil {
		return nil, code, err
	}
	if !mayAuthor(st, w) {
		return nil, http.StatusForbidden, fmt.Errorf("%q is not yours to change", name)
	}
	var existing *resource.Resource
	for i := range desired {
		if desired[i].Kind == resource.KindCommitment && desired[i].Metadata.Name == name {
			existing = &desired[i]
		}
	}
	if existing == nil || existing.Commitment == nil {
		return nil, http.StatusNotFound, fmt.Errorf("no commitment named %q", name)
	}
	// ABSENT IS PRESERVED, EMPTY IS MEANT. Every field arrives as a
	// pointer so the two can be told apart, and that distinction is the
	// whole safety of this handler.
	//
	// It used to rebuild the spec from a flat struct, so a field the
	// caller did not send was written as its zero value: correcting a
	// description through a form that knows about four fields silently
	// cleared requiresBooking, mayComplete and notify, and turned a
	// meter rule into a clock rule. Every surface then had to send
	// every field or gate its edit button — a rule each new form would
	// have to rediscover, and the Console's did not.
	//
	// So the handler preserves instead. A form owns what it sends and
	// nothing else, and a field added to CommitmentSpec later cannot be
	// silently zeroed by a page written before it existed.
	var f commitmentEdits
	if code, err := decodeAuthored(r, &f); err != nil {
		return nil, code, err
	}
	spec := *existing.Commitment
	if f.Description != nil {
		spec.Description = *f.Description
	}
	if strings.TrimSpace(spec.Description) == "" {
		return nil, http.StatusBadRequest, fmt.Errorf("what do you want to keep up with?")
	}
	if f.Rollover != nil {
		spec.Rollover = *f.Rollover
	}
	if spec.Rollover == "" {
		return nil, http.StatusBadRequest, fmt.Errorf("does this come round again?")
	}
	// Empty is a real answer for these, which is exactly why they need
	// the pointer: "any time" and "leave it alone" are different, and a
	// routine's part of day is the thing most likely to be wrong first.
	if f.PartOfDay != nil {
		spec.PartOfDay = *f.PartOfDay
	}
	if f.RequiresBooking != nil {
		spec.RequiresBooking = *f.RequiresBooking
	}
	if f.MayComplete != nil {
		spec.MayComplete = *f.MayComplete
	}
	if f.Notify != nil {
		spec.Notify = *f.Notify
	}
	if f.Scope != nil && *f.Scope != "" {
		spec.Scope = *f.Scope
	}
	if f.Anchor != nil && *f.Anchor != "" {
		spec.Anchor = *f.Anchor
	}
	// The first rule is the one the page renders; the rest are not its
	// to touch. Overlaid onto the rule that is there rather than built
	// fresh, so a meter rule stays a meter rule when a form that only
	// knows about intervals saves a new description.
	edited := resource.CommitmentDue{Meter: resource.MeterClock}
	if len(spec.Due) > 0 {
		edited = spec.Due[0]
	}
	if f.Meter != nil {
		edited.Meter = *f.Meter
	}
	if f.Every != nil {
		edited.Every = *f.Every
	}
	if f.Lead != nil {
		edited.Lead = *f.Lead
	}
	if f.At != nil {
		edited.At = *f.At
	}
	if f.AtValue != nil {
		edited.AtValue = *f.AtValue
	}
	if edited.Meter == "" {
		edited.Meter = resource.MeterClock
	}
	if err := clockSpanProblem(edited.Meter, map[string]string{"every": edited.Every, "lead": edited.Lead}); err != nil {
		return nil, http.StatusBadRequest, err
	}
	if len(spec.Due) == 0 {
		spec.Due = []resource.CommitmentDue{edited}
	} else {
		spec.Due = append([]resource.CommitmentDue{edited}, spec.Due[1:]...)
	}
	owner := existing.Metadata.Owner
	if f.About != nil && strings.TrimSpace(*f.About) != "" {
		owner = commitmentFields{About: *f.About}.owner(member)
	}
	res := resource.Resource{
		ApiVersion: resource.ApiVersion, Kind: resource.KindCommitment,
		Metadata:   resource.Metadata{Name: name, Owner: owner},
		Commitment: &spec,
	}
	if code, err := s.applyAuthored(member, authored{
		Put: []resource.Resource{res}, Summary: member + " changed " + spec.Description,
		BasedOn: basedOnHead(r),
	}); err != nil {
		return nil, code, err
	}
	return map[string]any{"commitment": name}, http.StatusOK, nil
}

// homeRemoveCommitment stops tracking one.
//
// The history stays. A commitment's completions are the household's
// record of what it actually did, and deleting the schedule is saying
// "stop asking me", not "this never happened" — so the ledger rows are
// left where they are rather than cascaded away.
func (s *Server) homeRemoveCommitment(_ http.ResponseWriter, r *http.Request, member string, _ []resource.Resource) (any, int, error) {
	name := r.PathValue("name")
	w := s.who(r, member)
	st, code, err := s.commitmentFor(name, w)
	if err != nil {
		return nil, code, err
	}
	if !mayAuthor(st, w) {
		return nil, http.StatusForbidden, fmt.Errorf("%q is not yours to remove", name)
	}
	if code, err := s.applyAuthored(member, authored{
		Delete:  []plan.DeletionRef{{Kind: string(resource.KindCommitment), Name: name}},
		Summary: member + " stopped tracking " + st.Description,
		BasedOn: basedOnHead(r),
	}); err != nil {
		return nil, code, err
	}
	return map[string]any{"removed": name}, http.StatusOK, nil
}

// meterDependants names the commitments measured against a meter.
//
// Deleting one is the single destructive act in this set, and not
// because it is hard to undo: the commitments keep their thresholds and
// lose the number those thresholds measure against, so each silently
// stops being able to say whether it is due. That is why the grade is
// Medium while creating one is Low, and why this answer is offered
// BEFORE the delete rather than as an error after it.
func (s *Server) meterDependants(meter string, w who) ([]string, int, error) {
	if s.Upkeep == nil {
		return nil, 0, nil
	}
	standings, err := s.Upkeep.Evaluate()
	if err != nil {
		return nil, 0, err
	}
	var out []string
	hidden := 0
	for _, st := range standings {
		measures := false
		for _, rule := range st.Occurrence.Rules {
			if rule.Meter == meter && !rule.Calendar {
				measures = true
				break
			}
		}
		if !measures {
			continue
		}
		if visibleTo(st, w) {
			out = append(out, st.Description)
			continue
		}
		hidden++
	}
	return out, hidden, nil
}

// homeRemoveMeter stops keeping a number.
func (s *Server) homeRemoveMeter(_ http.ResponseWriter, r *http.Request, member string, desired []resource.Resource) (any, int, error) {
	name := r.PathValue("name")
	var found *resource.Resource
	for i := range desired {
		if desired[i].Kind == resource.KindMeter && desired[i].Metadata.Name == name &&
			desired[i].Meter != nil {
			found = &desired[i]
		}
	}
	if found == nil {
		return nil, http.StatusNotFound, fmt.Errorf("no meter named %q", name)
	}
	w := s.who(r, member)
	dependants, hidden, err := s.meterDependants(name, w)
	if err != nil {
		return nil, http.StatusInternalServerError, err
	}
	// Confirmed rather than forbidden. A member may do this — it is not
	// an Owner's decision — but not by accident, and the sentence names
	// exactly what stops working.
	//
	// INCLUDING WHAT THEY CANNOT SEE. Somebody else's personal
	// commitment may measure against this meter, and naming it would
	// leak what personal scope exists to hide. Counting it does not, and
	// leaving it out entirely would let a member break a thing they were
	// never told about — silently, which is the worst of the three.
	if (len(dependants) > 0 || hidden > 0) && r.URL.Query().Get("confirm") != "yes" {
		what := strings.Join(dependants, ", ")
		switch {
		case len(dependants) == 0:
			what = plural(hidden, "thing that is not yours to see", "things that are not yours to see")
		case hidden > 0:
			what += ", and " + plural(hidden, "thing that is not yours to see", "things that are not yours to see")
		}
		return map[string]any{
			"confirm": true, "dependants": dependants, "hidden": hidden,
			"consequence": what + " will keep their schedules and lose the number they measure " +
				"against, so none of them can say whether it is due.",
		}, http.StatusConflict, nil
	}
	if code, err := s.applyAuthored(member, authored{
		Delete:  []plan.DeletionRef{{Kind: string(resource.KindMeter), Name: name}},
		Summary: member + " stopped keeping " + found.Meter.Description,
		BasedOn: basedOnHead(r),
	}); err != nil {
		return nil, code, err
	}
	return map[string]any{"removed": name}, http.StatusOK, nil
}
