package mailmonitor

import (
	"strconv"
	"strings"
	"time"

	"github.com/chickenrat17/wildstone-homelab/keystone/runtimedb"
)

// maxAhead is how far ahead a model-read date may be before it is
// treated as a misreading rather than an event.
const maxAhead = 400 * 24 * time.Hour

// modelEvent is a validated model output turned into candidate fields.
type modelEvent struct {
	fields     runtimedb.MailCandidateFields
	correction bool
	excerpt    string
}

// resolveModel turns a VALIDATED output into candidate fields in the
// household timezone, deciding for itself — not taking the model's word
// — what is missing or ambiguous:
//
//   - a date or time with no textual basis in the message is ambiguous;
//   - a stated zone Keystone cannot resolve is ambiguous, and then no
//     time is kept at all;
//   - a wall time that does not exist (a DST gap) is ambiguous;
//   - an end at or before the start is ambiguous;
//   - a date before the message was sent, or implausibly far ahead, is
//     ambiguous.
//
// The model's own missing/ambiguous lists are unioned in, never
// subtracted from.
func resolveModel(o ModelOutput, in ExtractInput) (modelEvent, bool) {
	if !*o.IsEvent {
		return modelEvent{}, false
	}
	household := in.Household
	if household == nil {
		household = time.UTC
	}
	missing := map[string]bool{}
	ambiguous := map[string]bool{}
	for _, f := range *o.Missing {
		missing[f] = true
	}
	for _, f := range *o.Ambiguous {
		ambiguous[f] = true
	}
	f := runtimedb.MailCandidateFields{
		Title:      safeText(*o.Title, runtimedb.MaxMailTitle, false),
		Location:   safeText(*o.Location, runtimedb.MaxMailLocation, false),
		Notes:      safeText(*o.Notes, runtimedb.MaxMailNotes, true),
		Confidence: *o.Confidence,
		AllDay:     *o.AllDay,
	}
	if f.Title == "" {
		missing[runtimedb.MailFieldTitle] = true
	}

	loc := household
	if tz := strings.TrimSpace(*o.Timezone); tz != "" {
		if z, ok := loadZone(tz); ok {
			loc = z
		} else {
			ambiguous[runtimedb.MailFieldTimezone] = true
		}
	}
	text := in.Subject + "\n" + in.Text

	var day time.Time
	switch {
	case *o.Date == "":
		missing[runtimedb.MailFieldDate] = true
	case !dateEvidence.MatchString(text):
		ambiguous[runtimedb.MailFieldDate] = true
	default:
		day, _ = time.ParseInLocation(time.DateOnly, *o.Date, loc)
		if !in.Sent.IsZero() {
			sentDay := in.Sent.In(loc)
			sentDay = time.Date(sentDay.Year(), sentDay.Month(), sentDay.Day(), 0, 0, 0, 0, loc)
			if day.Before(sentDay) || day.Sub(sentDay) > maxAhead {
				ambiguous[runtimedb.MailFieldDate] = true
			}
		}
	}

	hasTime := timeEvidence.MatchString(text)
	clock := func(s string) (int, int) {
		h, _ := strconv.Atoi(s[:2])
		m, _ := strconv.Atoi(s[3:])
		return h, m
	}
	if !day.IsZero() && !ambiguous[runtimedb.MailFieldDate] && !ambiguous[runtimedb.MailFieldTimezone] {
		if f.AllDay {
			f.Start = day
			f.End = day.AddDate(0, 0, 1)
		} else {
			switch {
			case *o.StartTime == "":
				missing[runtimedb.MailFieldStart] = true
			case !hasTime:
				ambiguous[runtimedb.MailFieldStart] = true
			default:
				h, m := clock(*o.StartTime)
				start, exists := wallTime(day.Year(), day.Month(), day.Day(), h, m, loc)
				if !exists {
					ambiguous[runtimedb.MailFieldStart] = true
					break
				}
				f.Start = start
				switch {
				case *o.EndTime != "":
					eh, em := clock(*o.EndTime)
					end, exists := wallTime(day.Year(), day.Month(), day.Day(), eh, em, loc)
					if !exists || !end.After(start) {
						ambiguous[runtimedb.MailFieldEnd] = true
					} else {
						f.End = end
					}
				case *o.DurationMinutes > 0:
					f.End = start.Add(time.Duration(*o.DurationMinutes) * time.Minute)
					f.DurationMinutes = *o.DurationMinutes
				default:
					missing[runtimedb.MailFieldEnd] = true
				}
			}
		}
	}
	// A time that could not be pinned down keeps no instant at all, so
	// nothing downstream checks a conflict against it.
	if ambiguous[runtimedb.MailFieldDate] || ambiguous[runtimedb.MailFieldTimezone] ||
		ambiguous[runtimedb.MailFieldStart] || missing[runtimedb.MailFieldDate] || missing[runtimedb.MailFieldStart] {
		f.Start, f.End, f.DurationMinutes = time.Time{}, time.Time{}, 0
	}
	if !f.Start.IsZero() {
		f.TZ = loc.String()
	}
	delete(missing, runtimedb.MailFieldLocation)
	if f.Location == "" {
		missing[runtimedb.MailFieldLocation] = true
	}
	f.Missing, f.Ambiguous = sortedKeys(missing), sortedKeys(ambiguous)

	ev := modelEvent{fields: f, correction: *o.IsCorrection}
	if modelEvidence(in.Text, *o.Evidence) || modelEvidence(in.Subject, *o.Evidence) {
		ev.excerpt = safeText(*o.Evidence, runtimedb.MaxMailExcerpt, false)
	} else {
		ev.excerpt = safeText(in.Text, runtimedb.MaxMailExcerpt, false)
	}
	return ev, true
}
