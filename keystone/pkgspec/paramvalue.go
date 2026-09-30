package pkgspec

import (
	"fmt"
	"net/mail"
	"net/netip"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

// ParamShapes are the value shapes a param may declare with `validate:`.
//
// A closed vocabulary, for the reason every other closed vocabulary here
// is closed: an unrecognised shape must fail review rather than validate
// nothing. `validate: iana-timezone` — a plausible spelling of a real
// name — would otherwise be a param with no checking at all, which is
// the state this field exists to end.
//
// The field is separate from Source. Source says where a value COMES
// FROM (the deployment knows it, so the form offers it); this says what
// a value must LOOK LIKE once it arrives, whoever supplied it. Most
// params have a source and no shape, or a shape and no source.
var ParamShapes = map[string]func(string) error{
	"iana_timezone": validateIANATimezone,
	"url":           validateURL,
	"hostname":      validateHostname,
	"email":         validateEmail,
	"port":          validatePort,
	"duration":      validateDuration,
	"private_ipv4":  validatePrivateIPv4,
}

// ParamShapeNames lists the vocabulary, for a diagnostic that names what
// does exist rather than only what does not.
func ParamShapeNames() []string {
	names := make([]string, 0, len(ParamShapes))
	for n := range ParamShapes {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// enumShapePrefix opens the one PARAMETERIZED shape in the vocabulary: the
// values a closed-choice param accepts are named in the shape itself
// (`enum:a|b|c`), so they cannot live in ParamShapes' fixed map the way
// `iana_timezone` or `port` do — each package invents its own set.
//
// This is still a closed vocabulary in the sense that matters: what the
// FORM of a shape may be stays fixed (a bare name, or "enum:" plus a
// pipe-separated list), so `validate: enu:a|b` fails review as loudly as
// `validate: iana-timezone` (the wrong hyphen) always has. What varies is
// the values after the colon, which are the package author's to choose —
// exactly like an AccessGroups entry or a Route host, which name real
// things this package knows about rather than picking from Keystone's own
// vocabulary.
const enumShapePrefix = "enum:"

// enumShapeValues reports the pipe-separated values of an `enum:` shape,
// and whether shape is one at all. It does not validate them — a caller
// that only wants to know "is this the enum form" (ValidateParamValue)
// gets exactly that, and the one that also has to reject a malformed
// declaration (Package.Validate) checks the slice itself.
func enumShapeValues(shape string) ([]string, bool) {
	rest, ok := strings.CutPrefix(shape, enumShapePrefix)
	if !ok {
		return nil, false
	}
	return strings.Split(rest, "|"), true
}

// validateEnumShapeDecl checks an `enum:` shape's OWN declaration is
// well-formed: at least one value, none blank, none repeated. Called once,
// from Package.Validate, so a package with `validate: enum:` (nothing
// after the colon) or `validate: enum:a|a` fails review rather than
// shipping a picker with no options or two identical ones.
func validateEnumShapeDecl(values []string) error {
	if len(values) == 0 || (len(values) == 1 && strings.TrimSpace(values[0]) == "") {
		return fmt.Errorf("enum shape lists no values: write enum:a|b|c naming at least one")
	}
	seen := map[string]bool{}
	for _, v := range values {
		if strings.TrimSpace(v) == "" {
			return fmt.Errorf("enum shape %q has an empty value between two '|'s", enumShapePrefix+strings.Join(values, "|"))
		}
		if seen[v] {
			return fmt.Errorf("enum shape %q lists %q twice", enumShapePrefix+strings.Join(values, "|"), v)
		}
		seen[v] = true
	}
	return nil
}

// ValidateParamValue checks one value against one declared shape.
//
// An empty shape passes: a param that declares nothing is a param this
// cannot speak about, and inventing a rule for it would reject values
// authors deliberately left free-form.
//
// An empty VALUE also passes, for a different reason: whether a param
// may be empty is what `required` already answers, and duplicating that
// judgement here would make an optional param with a shape impossible to
// leave unset.
func ValidateParamValue(shape, value string) error {
	if shape == "" || value == "" {
		return nil
	}
	if values, ok := enumShapeValues(shape); ok {
		for _, v := range values {
			if v == value {
				return nil
			}
		}
		return fmt.Errorf("%q is not one of the allowed values: %s", value, strings.Join(values, ", "))
	}
	check, ok := ParamShapes[shape]
	if !ok {
		return fmt.Errorf("unknown validate shape %q (one of: %s, or enum:a|b|c for a closed choice)", shape, strings.Join(ParamShapeNames(), ", "))
	}
	return check(value)
}

// validateIANATimezone is the rule that started this file.
//
// A household enabled books-calibre-web with timezone "CST" — a real
// abbreviation, and not a zone. Every layer accepted it: the param is a
// string, lint checks the package rather than the operator's value, the
// Container reconciled, Docker reported the container Up. Inside it,
// Calibre-Web's scheduler asked tzlocal for the zone named CST, got
// ZoneInfoNotFoundError, and the app died while its s6 init kept PID 1
// alive. The proxy answered 502 to a container that was, by every
// measure Wildstone had, healthy.
//
// The zone database is the authority and it ships with the binary, so
// this is a lookup rather than a pattern: "Europe/London" resolves,
// "CST" does not, and neither does "Europe/Londo".
func validateIANATimezone(v string) error {
	// LoadLocation accepts these two, and neither is what a household
	// means by a timezone — "Local" is whatever the CONTAINER thinks it
	// is, which is the question the param exists to answer.
	switch v {
	case "Local":
		return fmt.Errorf("%q is the container's own idea of local time, not a zone: name the zone, e.g. Europe/London", v)
	case "UTC":
		return nil
	}
	if _, err := time.LoadLocation(v); err != nil {
		hint := ""
		if !strings.Contains(v, "/") {
			// The whole class the failure came from: CST, EST, PST, GMT+5.
			hint = " — an abbreviation is not a zone; use the Continent/City form, e.g. America/Chicago"
		}
		return fmt.Errorf("%q is not an IANA time zone%s", v, hint)
	}
	return nil
}

func validateURL(v string) error {
	u, err := url.Parse(v)
	if err != nil {
		return fmt.Errorf("%q is not a URL: %v", v, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("%q must have an http or https scheme", v)
	}
	if u.Host == "" {
		return fmt.Errorf("%q names no host", v)
	}
	return nil
}

func validateHostname(v string) error {
	if strings.ContainsAny(v, "/: ") {
		return fmt.Errorf("%q is a hostname, not a URL or an address with a port", v)
	}
	for _, label := range strings.Split(v, ".") {
		if label == "" {
			return fmt.Errorf("%q has an empty label", v)
		}
	}
	return nil
}

func validateEmail(v string) error {
	if _, err := mail.ParseAddress(v); err != nil {
		return fmt.Errorf("%q is not an email address: %v", v, err)
	}
	return nil
}

func validatePort(v string) error {
	n, err := strconv.Atoi(v)
	if err != nil {
		return fmt.Errorf("%q is not a port number", v)
	}
	if n < 1 || n > 65535 {
		return fmt.Errorf("port %d is outside 1-65535", n)
	}
	return nil
}

func validateDuration(v string) error {
	if _, err := time.ParseDuration(v); err != nil {
		return fmt.Errorf("%q is not a duration (e.g. 30s, 5m, 24h): %v", v, err)
	}
	return nil
}

// privateIPv4Ranges are the ranges a `private_ipv4` value may fall in:
// RFC 1918, and the shared address space a tailnet hands out.
var privateIPv4Ranges = []netip.Prefix{
	netip.MustParsePrefix("10.0.0.0/8"),
	netip.MustParsePrefix("172.16.0.0/12"),
	netip.MustParsePrefix("192.168.0.0/16"),
	netip.MustParsePrefix("100.64.0.0/10"),
}

// validatePrivateIPv4 is for a listener that must be reachable from the
// household's network and nowhere else — the MQTT broker's LAN listener
// is the case that asked for it. Every rejection is one an operator could
// plausibly type: 0.0.0.0 (every interface, the public one included),
// 127.0.0.1 (unreachable from any container, so it looks up and serves
// nobody), a hostname, or a public address.
func validatePrivateIPv4(v string) error {
	a, err := netip.ParseAddr(v)
	if err != nil || !a.Is4() {
		return fmt.Errorf("%q is not an IPv4 address: give this machine's LAN or tailnet address, e.g. 192.168.1.10", v)
	}
	if a.IsUnspecified() {
		return fmt.Errorf("%q means every interface, including a public one: give the one private address to listen on", v)
	}
	if a.IsLoopback() {
		return fmt.Errorf("%q is loopback, which no container and no other machine can reach: give this machine's LAN address", v)
	}
	for _, p := range privateIPv4Ranges {
		if p.Contains(a) {
			return nil
		}
	}
	return fmt.Errorf("%q is not a private address (10/8, 172.16/12, 192.168/16, or a tailnet's 100.64/10)", v)
}
