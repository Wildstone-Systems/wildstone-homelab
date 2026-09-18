package portal

// "Today at home": the household event stream as sentences a family
// reads, rather than as the verbs a publisher writes.
//
// THE MAPPING IS CLOSED, AND AN UNMAPPED VERB IS OMITTED. That is the
// whole design decision in this file and it is worth stating why,
// because the tempting fallback is to print the event type when there
// is no row for it. A dashboard that renders "automation.action_failed"
// to a family has not degraded gracefully; it has shown them a bug in a
// typeface that makes it look deliberate, and it teaches them that the
// page sometimes speaks a language they cannot read. An absent row is a
// missing row in a list that never claimed to be complete. A machine
// verb on the wall tablet is a defect with an audience.
//
// So the absence is deliberately detectable instead: Sentence reports
// ok=false, the caller drops the row, and the count of dropped rows is
// what tells an operator a publisher started emitting something nobody
// has written a sentence for.

import (
	"fmt"
	"strings"
)

// Tone is how a row reads at a glance, and it is deliberately four
// values rather than a bool.
//
// The fourth one — ToneRefused — is the one that earned its place. A
// request that was refused is not a failure and not a thing waiting:
// nothing broke, nobody is blocked, and the house did exactly what it
// was configured to do. Colouring it red would send a family looking
// for a problem that does not exist, and colouring it green would hide
// that somebody asked for something they could not have.
type Tone string

const (
	ToneOK      Tone = "ok"
	ToneWaiting Tone = "waiting"
	ToneFailed  Tone = "failed"
	ToneRefused Tone = "refused"
)

// line is one verb's rendering: a sentence template and the tone it
// carries.
//
// The template takes the event's subject — the person or thing it is
// about — as %s, or ignores it. Two forms rather than one because some
// events are about somebody ("Sandy got home") and some are about the
// house ("the morning report was written"), and forcing a subject into
// the second produces the machine-translated register that makes a
// household surface feel like a log viewer.
type line struct {
	// withSubject is used when the event names one; without is the
	// fallback when it does not. An event whose sentence NEEDS a subject
	// and has none is dropped, the same as an unmapped verb: "  got
	// home" is worse than one fewer row.
	withSubject string
	without     string
	tone        Tone
}

// lines is the vocabulary. One row per verb this household surface
// knows how to say out loud.
//
// Everything operator-facing is deliberately absent rather than
// mapped — reconcile.applied, slice.started, keystone.db and the rest.
// They are real events about real work, and none of them is something
// a family is being asked to know. Absence here is the filter.
var lines = map[string]line{
	"person.arrived_home": {withSubject: "%s got home", tone: ToneOK},
	"person.left_home":    {withSubject: "%s went out", tone: ToneOK},
	"presence.arrived":    {withSubject: "%s got home", without: "somebody got home", tone: ToneOK},
	"pantry.meal_logged":  {withSubject: "%s put a meal on the list", without: "a meal went on the list", tone: ToneOK},
	"bins.collected":      {without: "the bins went out", tone: ToneOK},
	"calendar.school_day": {without: "it is a school day", tone: ToneOK},
	// The routine engine. "acted" is the successful case and reads as
	// the house doing something on its own; "fired" is the trigger
	// landing, which is only worth a sentence when nothing followed it.
	"automation.acted":  {withSubject: "%s ran on its own", without: "a routine ran on its own", tone: ToneOK},
	"automation.fired":  {withSubject: "%s started", without: "a routine started", tone: ToneOK},
	"domestic.acted":    {withSubject: "the assistant did something for %s", without: "the assistant did something", tone: ToneOK},
	"tool.dispatched":   {withSubject: "the assistant used something for %s", without: "the assistant used something", tone: ToneOK},
	"filedrop.uploaded": {withSubject: "%s added a file", without: "a file was added", tone: ToneOK},
	"report.morning":    {without: "the morning summary was written", tone: ToneOK},
	// Waiting. Every one of these has a matching item in Needs you —
	// this card reports, it does not ask (§7).
	"automation.escalated": {withSubject: "%s stopped to ask somebody", without: "a routine stopped to ask somebody", tone: ToneWaiting},
	// Failing now.
	"automation.action_failed":     {withSubject: "%s tried something and it did not work", without: "a routine tried something and it did not work", tone: ToneFailed},
	"automation.escalation_failed": {withSubject: "%s could not reach anybody to ask", without: "a routine could not reach anybody to ask", tone: ToneFailed},
	// Refused, and deliberately not an alert: nothing broke and nothing
	// is waiting.
	"automation.suppressed":         {withSubject: "%s was asked to run and did not", without: "a routine was asked to run and did not", tone: ToneRefused},
	"automation.escalation_skipped": {without: "a routine decided not to ask anybody", tone: ToneRefused},
	"butler.household_session":      {withSubject: "%s asked the assistant something", without: "somebody asked the assistant something", tone: ToneOK},
	// Commitments (ADR-W034). The entity carries which one, so these
	// sentences say what happened to the household rather than naming
	// the thing twice; the Needs you card is where a person reads WHICH.
	//
	// due_soon is deliberately absent. Every one of them has an item on
	// Needs you, and a card that reports is not a card that asks (§7) —
	// a row saying "the truck is nearly due" beside an item saying the
	// same thing is the household being told twice and trusting both
	// less.
	"commitment.overdue":      {withSubject: "something of %s's went past its date", without: "something went past its date", tone: ToneWaiting},
	"commitment.scheduled":    {withSubject: "%s's appointment was booked", without: "an appointment was booked", tone: ToneOK},
	"commitment.satisfied":    {withSubject: "%s's was seen to", without: "something was seen to", tone: ToneOK},
	"commitment.waived":       {withSubject: "%s's was skipped on purpose", without: "something was skipped on purpose", tone: ToneRefused},
	"commitment.cycle_opened": {withSubject: "%s's starts again", without: "something starts again", tone: ToneOK},
	// The meter nobody can read. Not a failure and not an all-clear: a
	// request, which is exactly the distinction A11 turns on.
	"commitment.unresolved": {withSubject: "a number of %s's needs reading", without: "a number needs reading", tone: ToneWaiting},
}

// Sentence renders one event as a household sentence and its tone.
//
// ok=false means "this surface has nothing to say about this verb", and
// the caller MUST drop the row rather than substitute anything. See the
// file comment: the fallback everybody reaches for is the bug.
func Sentence(eventType, subject string) (string, Tone, bool) {
	l, known := lines[eventType]
	if !known {
		return "", "", false
	}
	subject = strings.TrimSpace(subject)
	if subject != "" && l.withSubject != "" {
		return fmt.Sprintf(l.withSubject, subject), l.tone, true
	}
	if l.without != "" {
		return l.without, l.tone, true
	}
	// A verb whose only form needs a subject, on an event that has
	// none. Dropped for the same reason an unmapped verb is.
	return "", "", false
}

// Says reports whether this surface has a sentence for a verb, without
// rendering one. It is what a caller uses to decide whether an event is
// household-visible at all — the Activity page's filters need the
// question answered before they have a subject to render with.
func Says(eventType string) bool {
	_, known := lines[eventType]
	return known
}

// Filter is one of the Activity page's tabs.
//
// THE FILTERS PARTITION THE SET EXACTLY, which the design asks for in
// as many words: every row belongs to exactly one of Home, Files and
// The assistant, and "Asked for a decision" cuts across them by TONE
// rather than by subject. A set of filters that overlapped would make
// the counts beside them lies, and a row in none of them would be a row
// a member can only find by turning every filter off.
type Filter string

const (
	FilterEverything Filter = ""
	FilterWaiting    Filter = "waiting"
	FilterHome       Filter = "home"
	FilterFiles      Filter = "files"
	FilterAssistant  Filter = "assistant"
)

// families maps each subject filter to the verb prefixes it owns. One
// prefix appears under exactly one filter, which is what makes the
// partition true rather than intended.
var families = map[Filter][]string{
	// Commitments sit under Home with the rest of what the house keeps
	// track of. They are not a tab of their own: a household does not
	// think of "the truck's oil" and "the bins went out" as different
	// kinds of thing, and a tab per subsystem is how a dashboard grows
	// into a filing cabinet.
	FilterHome:      {"person.", "presence.", "bins.", "calendar.", "automation.", "pantry.", "report.", "commitment."},
	FilterFiles:     {"filedrop."},
	FilterAssistant: {"butler.", "domestic.", "tool."},
}

// Subject reports which subject filter a verb belongs to, for a verb
// this surface has a sentence for.
//
// An unmapped verb has no subject, and neither does a verb whose family
// nobody has assigned: both are dropped by the caller, which is the
// same treatment Sentence gives them and for the same reason.
func Subject(eventType string) (Filter, bool) {
	if !Says(eventType) {
		return "", false
	}
	for f, prefixes := range families {
		for _, p := range prefixes {
			if strings.HasPrefix(eventType, p) {
				return f, true
			}
		}
	}
	// A verb with a sentence but no family is a gap in this table
	// rather than in the vocabulary. Reported as unassigned so it can be
	// counted, not silently filed under something it is not.
	return "", false
}

// Matches reports whether one row belongs under a filter.
func Matches(f Filter, eventType string, tone Tone) bool {
	switch f {
	case FilterEverything:
		return true
	case FilterWaiting:
		// By tone, not by subject: "something is waiting" is a state a
		// row is in, and it cuts across all three subjects.
		return tone == ToneWaiting
	}
	subject, ok := Subject(eventType)
	return ok && subject == f
}
