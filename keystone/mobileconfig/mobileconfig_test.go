package mobileconfig

// A malformed configuration profile fails on the phone with "Profile
// Installation Failed" and no detail whatsoever — not a line number, not
// a key name, nothing. There is no log to read and no way to ask the
// device what it objected to.
//
// So these tests are the only feedback loop this code has, and they are
// written against Apple's key names rather than against what the
// template happens to emit.

import (
	"encoding/xml"
	"strings"
	"testing"
)

func household() Profile {
	return Profile{
		Host:     "cal.home.example.ts.net",
		Port:     51443,
		Username: "mika",
		Accounts: []Account{
			{Name: "Household", PrincipalURL: "/household/"},
			{Name: "Mika", PrincipalURL: "/mika/"},
		},
	}
}

func build(t *testing.T, p Profile) string {
	t.Helper()
	out, err := p.Build()
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	return string(out)
}

// The whole point of the profile: a principal the phone would never
// discover on its own.
func TestTheProfileNamesThePrincipalIosWouldNotFind(t *testing.T) {
	got := build(t, household())
	if !strings.Contains(got, "<key>CalDAVPrincipalURL</key>\n\t\t\t<string>/household/</string>") {
		t.Errorf("the household principal is not pinned — without this key iOS discovers only the "+
			"signed-in member's own calendars and the shared one never appears:\n%s", got)
	}
	if !strings.Contains(got, "<string>/mika/</string>") {
		t.Errorf("the member's own principal is missing:\n%s", got)
	}
}

// A password in here would be Keystone holding a household member's own
// credential and serving it over HTTP, to save one prompt.
func TestTheProfileCarriesNoPassword(t *testing.T) {
	got := build(t, household())
	if strings.Contains(got, "CalDAVPassword") {
		t.Errorf("the profile carries a password key:\n%s", got)
	}
}

// Installing twice must replace, not duplicate. A random UUID gives the
// person who tried again two copies of every calendar.
func TestIdentifiersAreStableAcrossBuilds(t *testing.T) {
	first, second := build(t, household()), build(t, household())
	if first != second {
		t.Error("two builds of the same profile differ; reinstalling would add a second copy of every calendar " +
			"rather than replacing the first")
	}

	// And different people get different profiles, or one household
	// member's install replaces another's on a shared iPad.
	other := household()
	other.Username = "giulia"
	if build(t, other) == first {
		t.Error("two members produced the same profile")
	}
	if !strings.Contains(build(t, other), "<string>giulia</string>") {
		t.Error("the second member's username is not in their profile")
	}
}

// Every profile has to be a well-formed plist, which is the one thing
// that can be checked here without a device.
func TestTheProfileIsWellFormedXML(t *testing.T) {
	got := build(t, household())
	decoder := xml.NewDecoder(strings.NewReader(got))
	for {
		_, err := decoder.Token()
		if err != nil {
			if err.Error() == "EOF" {
				break
			}
			t.Fatalf("the profile is not well-formed XML (%v):\n%s", err, got)
		}
	}
	for _, want := range []string{
		`<?xml version="1.0" encoding="UTF-8"?>`,
		`<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN"`,
		`<key>PayloadType</key>`,
		`<string>com.apple.caldav.account</string>`,
		`<string>Configuration</string>`,
		`<key>CalDAVUseSSL</key>`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the profile is missing %q:\n%s", want, got)
		}
	}
}

// "Ben & Jo" is not a contrived household member.
func TestEveryTextValueIsEscaped(t *testing.T) {
	p := household()
	p.Username = `ben&jo`
	p.Accounts = []Account{{Name: `Ben & Jo's <calendars>`, PrincipalURL: "/ben&jo/"}}

	got := build(t, p)
	decoder := xml.NewDecoder(strings.NewReader(got))
	for {
		_, err := decoder.Token()
		if err != nil {
			if err.Error() == "EOF" {
				break
			}
			t.Fatalf("an unescaped value made the profile malformed (%v):\n%s", err, got)
		}
	}
	if strings.Contains(got, "<string>ben&jo</string>") {
		t.Errorf("an ampersand reached the plist raw:\n%s", got)
	}
	if !strings.Contains(got, "ben&amp;jo") {
		t.Errorf("the escaped username is not present:\n%s", got)
	}
}

// A trailing slash is the difference between seeing your calendars and
// seeing a 403, on a server whose rights are anchored per principal.
func TestAPrincipalIsAlwaysAnAbsolutePathEndingInASlash(t *testing.T) {
	for _, in := range []string{"mika", "/mika", "mika/", "/mika/"} {
		p := household()
		p.Accounts = []Account{{Name: "Mine", PrincipalURL: in}}
		if got := build(t, p); !strings.Contains(got, "<string>/mika/</string>") {
			t.Errorf("principal %q did not normalise to /mika/:\n%s", in, got)
		}
	}
}

// Everything the caller can get wrong fails here rather than on a phone
// with no error message.
func TestNonsenseIsRefusedHereRatherThanOnThePhone(t *testing.T) {
	for name, p := range map[string]Profile{
		"no host": {Username: "mika", Accounts: []Account{{Name: "x", PrincipalURL: "/x/"}}},
		"a URL where a hostname belongs": {
			Host: "https://cal.example/", Username: "mika", Accounts: []Account{{Name: "x", PrincipalURL: "/x/"}},
		},
		"host:port in the host": {
			Host: "cal.example:51443", Username: "mika", Accounts: []Account{{Name: "x", PrincipalURL: "/x/"}},
		},
		"no username": {Host: "cal.example", Accounts: []Account{{Name: "x", PrincipalURL: "/x/"}}},
		"no accounts": {Host: "cal.example", Username: "mika"},
		"an account with no principal": {
			Host: "cal.example", Username: "mika", Accounts: []Account{{Name: "x"}},
		},
		"an account with no name": {
			Host: "cal.example", Username: "mika", Accounts: []Account{{PrincipalURL: "/x/"}},
		},
		"a traversing principal": {
			Host: "cal.example", Username: "mika", Accounts: []Account{{Name: "x", PrincipalURL: "/../etc/"}},
		},
		"a URL where a principal belongs": {
			Host: "cal.example", Username: "mika",
			Accounts: []Account{{Name: "x", PrincipalURL: "https://elsewhere.example/x/"}},
		},
	} {
		if _, err := p.Build(); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
}

// The default is the one a tailnet deployment does not use, so it is
// worth pinning that an explicit port survives.
func TestThePortIsCarriedBecauseATailnetNeedsIt(t *testing.T) {
	if !strings.Contains(build(t, household()), "<integer>51443</integer>") {
		t.Error("the port was dropped; under the tailnet posture every service shares one name and differs by port")
	}
	p := household()
	p.Port = 0
	if !strings.Contains(build(t, p), "<integer>443</integer>") {
		t.Error("an unset port did not default to 443")
	}
}
