package mailmonitor

import (
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/Wildstone-Systems/wildstone-homelab/keystone/adapter/caldav"
)

// windowsZones maps the Windows zone names Outlook and Exchange write as
// a TZID onto IANA zones. Only unambiguous, common names: an unknown
// TZID is NOT resolved to the household's zone — it makes the candidate
// needs_details instead.
var windowsZones = map[string]string{
	"eastern standard time":        "America/New_York",
	"central standard time":        "America/Chicago",
	"mountain standard time":       "America/Denver",
	"us mountain standard time":    "America/Phoenix",
	"pacific standard time":        "America/Los_Angeles",
	"alaskan standard time":        "America/Anchorage",
	"hawaiian standard time":       "Pacific/Honolulu",
	"atlantic standard time":       "America/Halifax",
	"gmt standard time":            "Europe/London",
	"w. europe standard time":      "Europe/Berlin",
	"romance standard time":        "Europe/Paris",
	"central europe standard time": "Europe/Budapest",
	"aus eastern standard time":    "Australia/Sydney",
	"new zealand standard time":    "Pacific/Auckland",
	"utc":                          "UTC",
	"coordinated universal time":   "UTC",
}

// mozillaPrefix is the path some clients prepend to an IANA name.
var mozillaPrefix = regexp.MustCompile(`^/?(?:mozilla\.org/[^/]+/|softwarestudio\.org/[^/]+/[^/]+/)?`)

// loadZone resolves a TZID. ok=false means it is not a zone this
// resolver may use, and the caller must treat the time zone as
// ambiguous rather than substitute one.
func loadZone(tzid string) (*time.Location, bool) {
	name := strings.Trim(strings.TrimSpace(tzid), `"`)
	if name == "" {
		return nil, false
	}
	if iana, ok := windowsZones[strings.ToLower(name)]; ok {
		name = iana
	}
	name = mozillaPrefix.ReplaceAllString(name, "")
	// Abbreviations (EST, PST, CET) are deliberately refused: several
	// mean different offsets in different places, and Go's database
	// resolves some of them to fixed offsets that ignore daylight time.
	if !strings.Contains(name, "/") && name != "UTC" {
		return nil, false
	}
	loc, err := time.LoadLocation(name)
	if err != nil {
		return nil, false
	}
	return loc, true
}

// wallTime builds a wall-clock time in loc and reports whether that wall
// clock exists there (a spring-forward gap does not; Go would silently
// move it an hour).
func wallTime(y int, mo time.Month, d, h, mi int, loc *time.Location) (time.Time, bool) {
	t := time.Date(y, mo, d, h, mi, 0, 0, loc)
	return t, t.Year() == y && t.Month() == mo && t.Day() == d && t.Hour() == h && t.Minute() == mi
}

// icsInstant resolves one DTSTART/DTEND. zone is the zone the time was
// resolved in; tzOK=false means the TZID could not be resolved (the
// caller marks the timezone ambiguous and keeps no time); ok=false means
// the value itself is unusable.
func icsInstant(t caldav.ICSTime, household *time.Location) (at time.Time, zone *time.Location, ok, tzOK bool) {
	v := strings.TrimSpace(t.Value)
	switch {
	case t.Date:
		d, err := time.ParseInLocation("20060102", v, household)
		return d, household, err == nil, true
	case t.UTC:
		u, err := time.Parse("20060102T150405Z", strings.ToUpper(v))
		return u, household, err == nil, true
	}
	zone = household
	if t.TZID != "" {
		z, found := loadZone(t.TZID)
		if !found {
			return time.Time{}, nil, true, false
		}
		zone = z
	}
	// A floating time (no TZID, no Z) is, by RFC 5545, the wall clock of
	// whoever reads it: the household.
	p, err := time.Parse("20060102T150405", v)
	if err != nil {
		return time.Time{}, nil, false, true
	}
	w, exists := wallTime(p.Year(), p.Month(), p.Day(), p.Hour(), p.Minute(), zone)
	if !exists {
		return time.Time{}, nil, false, true
	}
	return w.Add(time.Duration(p.Second()) * time.Second), zone, true, true
}

var icsDuration = regexp.MustCompile(`^([+-])?P(?:(\d+)W|(?:(\d+)D)?(?:T(?:(\d+)H)?(?:(\d+)M)?(?:(\d+)S)?)?)$`)

// parseICSDuration parses an RFC 5545 DURATION. Negative or zero
// durations are unusable.
func parseICSDuration(s string) (time.Duration, bool) {
	m := icsDuration.FindStringSubmatch(strings.ToUpper(strings.TrimSpace(s)))
	if m == nil || m[1] == "-" {
		return 0, false
	}
	n := func(i int) time.Duration {
		v, _ := strconv.Atoi(m[i])
		return time.Duration(v)
	}
	d := n(2)*7*24*time.Hour + n(3)*24*time.Hour + n(4)*time.Hour + n(5)*time.Minute + n(6)*time.Second
	return d, d > 0
}
