package caldav

// Invitations: the iCalendar that arrives in MAIL rather than from a
// calendar server (epic #326 S05).
//
// parseICS above reads a household's own calendar and resolves every
// time on the spot, falling back to the household zone for a TZID it
// does not know — right for events the household wrote, wrong for a
// stranger's invitation, where an unknown zone is exactly the thing that
// must not be guessed. ParseInvitation shares the same unfolding and
// unescaping but returns times UNRESOLVED (value, TZID, date/UTC flags)
// plus the iTIP method, SEQUENCE, STATUS, DURATION and whether a
// recurrence rule is present, and leaves every decision to the caller.
//
// It is bounded: at most MaxInvitationEvents events, lines of at most
// maxInvitationLine bytes, and nothing outside VEVENT (VTIMEZONE, VALARM
// and VTODO are skipped; an embedded VALARM's properties never leak into
// the event).

import (
	"errors"
	"strconv"
	"strings"
)

// Bounds on one invitation.
const (
	MaxInvitationEvents = 20
	maxInvitationLine   = 8 << 10
	maxInvitationBytes  = 256 << 10
)

// ErrNotInvitation means the data holds no VCALENDAR.
var ErrNotInvitation = errors.New("caldav: not an iCalendar object")

// ICSTime is one DTSTART/DTEND as written.
type ICSTime struct {
	// Value is the raw value, e.g. 20261015T180000 or 20261015.
	Value string
	// TZID is the TZID parameter, "" for none.
	TZID string
	// Date: a VALUE=DATE (all-day) value.
	Date bool
	// UTC: a value ending in Z.
	UTC bool
}

// IsZero reports whether the property was absent.
func (t ICSTime) IsZero() bool { return t.Value == "" }

// InvitationEvent is one VEVENT, unresolved.
type InvitationEvent struct {
	UID         string
	Sequence    int
	Status      string
	Summary     string
	Location    string
	Description string
	Start       ICSTime
	End         ICSTime
	// Duration is the raw DURATION value ("PT1H30M"), "" for none.
	Duration string
	// Recurring: an RRULE or RDATE is present.
	Recurring bool
}

// Invitation is a parsed iCalendar object.
type Invitation struct {
	// Method is the iTIP METHOD (REQUEST, CANCEL, PUBLISH…), upper-cased;
	// "" when the object declares none.
	Method string
	Events []InvitationEvent
	// Truncated: more than MaxInvitationEvents events were present.
	Truncated bool
}

// ParseInvitation parses one text/calendar body.
func ParseInvitation(data string) (Invitation, error) {
	if len(data) > maxInvitationBytes {
		return Invitation{}, errors.New("caldav: invitation is too large")
	}
	var inv Invitation
	var cur *InvitationEvent
	sawCalendar := false
	// depth counts components nested inside the current VEVENT (VALARM),
	// whose properties are not the event's.
	depth := 0
	for _, line := range unfold(data) {
		if len(line) > maxInvitationLine {
			continue
		}
		name, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		key, params, _ := strings.Cut(name, ";")
		key = strings.ToUpper(strings.TrimSpace(key))
		val := strings.TrimSpace(value)
		switch key {
		case "BEGIN":
			comp := strings.ToUpper(val)
			switch {
			case comp == "VCALENDAR":
				sawCalendar = true
			case cur != nil:
				depth++
			case comp == "VEVENT":
				cur = &InvitationEvent{}
			}
			continue
		case "END":
			comp := strings.ToUpper(val)
			switch {
			case cur != nil && depth > 0:
				depth--
			case cur != nil && comp == "VEVENT":
				if len(inv.Events) < MaxInvitationEvents {
					inv.Events = append(inv.Events, *cur)
				} else {
					inv.Truncated = true
				}
				cur = nil
			}
			continue
		case "METHOD":
			if cur == nil {
				inv.Method = strings.ToUpper(val)
			}
			continue
		}
		if cur == nil || depth > 0 {
			continue
		}
		switch key {
		case "UID":
			cur.UID = val
		case "SEQUENCE":
			if n, err := strconv.Atoi(val); err == nil && n >= 0 {
				cur.Sequence = n
			}
		case "STATUS":
			cur.Status = strings.ToUpper(val)
		case "SUMMARY":
			cur.Summary = unescapeICS(value)
		case "LOCATION":
			cur.Location = unescapeICS(value)
		case "DESCRIPTION":
			cur.Description = unescapeICS(value)
		case "DTSTART":
			cur.Start = icsTimeOf(val, params)
		case "DTEND":
			cur.End = icsTimeOf(val, params)
		case "DURATION":
			cur.Duration = strings.ToUpper(val)
		case "RRULE", "RDATE":
			cur.Recurring = true
		}
	}
	if !sawCalendar {
		return Invitation{}, ErrNotInvitation
	}
	return inv, nil
}

func icsTimeOf(value, params string) ICSTime {
	upper := strings.ToUpper(params)
	t := ICSTime{Value: value, TZID: icsParam(params, "TZID")}
	t.Date = strings.Contains(upper, "VALUE=DATE") && !strings.Contains(upper, "VALUE=DATE-TIME") || len(value) == 8
	t.UTC = strings.HasSuffix(strings.ToUpper(value), "Z")
	return t
}
