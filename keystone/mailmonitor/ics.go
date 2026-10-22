package mailmonitor

import (
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Wildstone-Systems/wildstone-homelab/keystone/adapter/caldav"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/adapter/mail/imapread/mimenorm"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/runtimedb"
)

// maxEventsPerMessage bounds how many candidates one message can
// produce. A school newsletter listing a term's dates is not a request
// to review forty events.
const maxEventsPerMessage = 5

// icsEvent is one resolved VEVENT and what its message asked for.
type icsEvent struct {
	uid      string
	sequence int
	cancel   bool
	fields   runtimedb.MailCandidateFields
	excerpt  string
}

// icsOutcome is what the deterministic path decided for a message.
type icsOutcome struct {
	events []icsEvent
	// state/reason when there is no event to record.
	state, reason string
}

// extractICS reads every usable calendar part. It never calls a model
// and never guesses: an unknown TZID, a missing DTSTART or end, or a
// recurrence leaves the field missing or ambiguous (needs_details).
func extractICS(parts []mimenorm.CalendarPart, household *time.Location) icsOutcome {
	byUID := map[string]caldav.InvitationEvent{}
	methods := map[string]string{}
	var order []string
	usable := false
	anonymous := 0
	for _, part := range parts {
		if part.Oversized || strings.TrimSpace(part.Data) == "" {
			continue
		}
		inv, err := caldav.ParseInvitation(part.Data)
		if err != nil {
			continue
		}
		usable = true
		method := inv.Method
		if method == "" {
			method = strings.ToUpper(strings.TrimSpace(part.Method))
		}
		for _, ev := range inv.Events {
			uid := strings.TrimSpace(ev.UID)
			if uid == "" {
				// Without a UID nothing can correct or cancel it later;
				// it is still an event.
				anonymous++
				uid = "\x00anonymous-" + strconv.Itoa(anonymous)
			}
			prev, seen := byUID[uid]
			if !seen {
				order = append(order, uid)
			}
			// The same invitation often arrives twice in one message (an
			// inline text/calendar and an .ics attachment); keep the
			// highest SEQUENCE.
			if !seen || ev.Sequence > prev.Sequence {
				byUID[uid] = ev
				methods[uid] = method
			}
		}
	}
	if !usable {
		for _, part := range parts {
			if part.Oversized {
				return icsOutcome{state: runtimedb.MailMessageSkipped, reason: "ics_oversized"}
			}
		}
		return icsOutcome{state: runtimedb.MailMessageSkipped, reason: "ics_unusable"}
	}
	if len(order) == 0 {
		return icsOutcome{state: runtimedb.MailMessageNoEvent, reason: "ics_no_event"}
	}
	var out icsOutcome
	for _, uid := range order {
		ev := byUID[uid]
		method := methods[uid]
		realUID := uid
		if strings.HasPrefix(uid, "\x00") {
			realUID = ""
		}
		switch method {
		case "REPLY", "COUNTER", "REFRESH", "DECLINECOUNTER":
			// An attendee's answer, not an event for this Person.
			continue
		case "CANCEL":
			if realUID != "" {
				out.events = append(out.events, icsEvent{uid: realUID, sequence: ev.Sequence, cancel: true})
			}
			continue
		}
		if ev.Status == "CANCELLED" {
			if realUID != "" {
				out.events = append(out.events, icsEvent{uid: realUID, sequence: ev.Sequence, cancel: true})
			}
			continue
		}
		out.events = append(out.events, icsEvent{uid: realUID, sequence: ev.Sequence,
			fields: icsFields(ev, household), excerpt: icsExcerpt(ev)})
		if len(out.events) >= maxEventsPerMessage {
			break
		}
	}
	if len(out.events) == 0 {
		return icsOutcome{state: runtimedb.MailMessageNoEvent, reason: "ics_reply"}
	}
	return out
}

func icsFields(ev caldav.InvitationEvent, household *time.Location) runtimedb.MailCandidateFields {
	f := runtimedb.MailCandidateFields{
		Title:      safeText(ev.Summary, runtimedb.MaxMailTitle, false),
		Location:   safeText(ev.Location, runtimedb.MaxMailLocation, false),
		Notes:      safeText(ev.Description, runtimedb.MaxMailNotes, true),
		Confidence: 1,
	}
	missing := map[string]bool{}
	ambiguous := map[string]bool{}
	if f.Title == "" {
		missing[runtimedb.MailFieldTitle] = true
	}
	if ev.Recurring {
		ambiguous[runtimedb.MailFieldRecurrence] = true
	}
	start, zone, ok, tzOK := icsInstant(ev.Start, household)
	switch {
	case ev.Start.IsZero():
		missing[runtimedb.MailFieldDate] = true
		missing[runtimedb.MailFieldStart] = true
	case !tzOK:
		ambiguous[runtimedb.MailFieldTimezone] = true
	case !ok:
		ambiguous[runtimedb.MailFieldStart] = true
	default:
		f.Start = start
		f.AllDay = ev.Start.Date
		f.TZ = zone.String()
	}
	if !f.Start.IsZero() {
		switch {
		case !ev.End.IsZero():
			end, _, eok, etzOK := icsInstant(ev.End, household)
			switch {
			case !etzOK:
				ambiguous[runtimedb.MailFieldTimezone] = true
			case !eok || !end.After(f.Start):
				ambiguous[runtimedb.MailFieldEnd] = true
			default:
				f.End = end
			}
		case ev.Duration != "":
			if d, ok := parseICSDuration(ev.Duration); ok {
				f.End = f.Start.Add(d)
			} else {
				ambiguous[runtimedb.MailFieldEnd] = true
			}
		case f.AllDay:
			// RFC 5545: a DATE start with no end is one day long.
			f.End = f.Start.AddDate(0, 0, 1)
		default:
			missing[runtimedb.MailFieldEnd] = true
		}
	}
	if ambiguous[runtimedb.MailFieldTimezone] {
		// A time in an unknown zone is no time at all.
		f.Start, f.End, f.TZ = time.Time{}, time.Time{}, ""
	}
	f.Missing, f.Ambiguous = sortedKeys(missing), sortedKeys(ambiguous)
	if f.NeedsDetails() {
		f.Confidence = 0.5
	}
	return f
}

func icsExcerpt(ev caldav.InvitationEvent) string {
	parts := []string{ev.Summary}
	if ev.Location != "" {
		parts = append(parts, ev.Location)
	}
	if ev.Description != "" {
		parts = append(parts, ev.Description)
	}
	return safeText(strings.Join(parts, " — "), runtimedb.MaxMailExcerpt, false)
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
