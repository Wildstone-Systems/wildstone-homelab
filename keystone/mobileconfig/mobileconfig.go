// Package mobileconfig builds the Apple configuration profile that puts
// the household's calendars on a phone.
//
// # Why this exists at all
//
// A CalDAV client discovers the calendars under the principal it signed
// in as. That is the whole of what it will find on its own, and it is
// why the household's SHARED calendar — which deliberately lives under
// its own principal so that it belongs to the house rather than to
// whoever made it — does not appear on an iPhone however correctly the
// server is configured. There is no "add this URL" for a writable
// account in iOS's own settings.
//
// `CalDAVPrincipalURL` is the one key that answers it. A configuration
// profile may name the principal an account should use, and iOS mounts
// it as an ordinary read-write CalDAV account. So the profile is not a
// convenience wrapper around instructions a person could follow by hand:
// it is the only path to the thing the household actually wants.
//
// # What it deliberately does not carry
//
// A password. `CalDAVPassword` is a real key and putting a value in it
// would make installation one tap shorter. It would also mean Keystone
// holding a household member's own password, writing it into a file, and
// serving that file over HTTP — three things this control plane does not
// do, to save one prompt. Omitted, iOS asks the person on install, which
// is the right party being asked for the right secret.
//
// # Stability
//
// Every UUID and identifier here is DERIVED from what the profile is
// about, never random. Installing a second time then replaces the first
// profile rather than adding a second copy of every calendar — which is
// what a random UUID produces, and it produces it on the phone of
// somebody who was only trying again because the first attempt looked
// like it had not worked.
package mobileconfig

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/url"
	"strings"
	"text/template"
)

// ContentType is what a profile must be served as. iOS decides whether
// to hand a download to the Settings app on this and nothing else; served
// as text/xml it renders as a page of XML and the person concludes the
// household's calendar is broken.
const ContentType = "application/x-apple-aspen-config"

// Filename is the download name. It is the visible half of the same
// decision — a phone that saves this to Files should show something a
// person recognises a week later.
const Filename = "wildstone-calendar.mobileconfig"

// organization is the profile's stated publisher, shown on the install
// screen above the list of what it will change.
const organization = "Wildstone"

// identifierRoot namespaces every identifier this package mints. Apple's
// convention is reverse-DNS and the practical requirement is only that
// two different profiles never collide on one device.
const identifierRoot = "dev.wildstone.calendar"

// Account is one calendar account the profile installs.
type Account struct {
	// Name is what the account is called on the phone: "Household",
	// "Mika's calendars".
	Name string
	// PrincipalURL is the path the account is rooted at, e.g. "/mika/"
	// or "/household/". THIS is the key that makes a shared calendar
	// possible; see the package comment.
	PrincipalURL string
}

// Profile is everything the phone needs except the password.
type Profile struct {
	// Host is the calendar server as the PHONE reaches it, without a
	// scheme: "cal.home.example.ts.net". Not the container address.
	Host string
	// Port is the port the phone reaches it on. 0 means 443.
	Port int
	// Username is the household member the profile is for. A profile is
	// built for one person and is not transferable: it carries their
	// login, and their calendars are theirs.
	Username string
	// Accounts are the calendars to install, in the order they should
	// appear.
	Accounts []Account
}

// Build renders the profile.
//
// It returns an error rather than a best-effort file for anything it
// cannot state truthfully. A malformed profile fails on the phone with
// "Profile Installation Failed" and no detail at all, so every check
// that can happen here is worth more than it costs.
func (p Profile) Build() ([]byte, error) {
	host := strings.TrimSpace(p.Host)
	if host == "" {
		return nil, fmt.Errorf("mobileconfig: no calendar host")
	}
	// A host with a scheme or a path on it is a caller that passed a URL
	// where a hostname belongs. iOS would take it literally and the
	// account would fail to connect with nothing to explain why.
	if strings.ContainsAny(host, "/:") {
		return nil, fmt.Errorf("mobileconfig: host %q must be a hostname, not a URL or host:port", host)
	}
	if strings.TrimSpace(p.Username) == "" {
		return nil, fmt.Errorf("mobileconfig: no username: a profile is built for one person")
	}
	if len(p.Accounts) == 0 {
		return nil, fmt.Errorf("mobileconfig: no accounts: a profile that installs nothing is a failed install")
	}
	port := p.Port
	if port == 0 {
		port = 443
	}

	data := profileData{
		Identifier:   identifierRoot,
		UUID:         derivedUUID(identifierRoot, host, p.Username),
		DisplayName:  "Wildstone Calendar",
		Description:  fmt.Sprintf("Adds the household's calendars to this device for %s.", p.Username),
		Organization: organization,
	}
	for _, a := range p.Accounts {
		name := strings.TrimSpace(a.Name)
		if name == "" {
			return nil, fmt.Errorf("mobileconfig: an account with no name")
		}
		principal, err := cleanPrincipal(a.PrincipalURL)
		if err != nil {
			return nil, err
		}
		id := identifierRoot + "." + slug(name)
		data.Accounts = append(data.Accounts, accountData{
			Identifier:  id,
			UUID:        derivedUUID(id, host, p.Username),
			DisplayName: name,
			Description: name,
			Host:        host,
			Port:        port,
			Username:    p.Username,
			Principal:   principal,
		})
	}

	var buf bytes.Buffer
	if err := profileTemplate.Execute(&buf, data); err != nil {
		return nil, fmt.Errorf("mobileconfig: rendering the profile: %w", err)
	}
	return buf.Bytes(), nil
}

// cleanPrincipal normalises a principal path and refuses one that could
// not be a path.
//
// It must be absolute and it must end in a slash: CalDAV clients resolve
// relative references against it, and "/mika" resolves one level higher
// than "/mika/" — which on a server whose rights are anchored per
// principal is the difference between seeing your calendars and seeing a
// 403.
func cleanPrincipal(p string) (string, error) {
	p = strings.TrimSpace(p)
	if p == "" {
		return "", fmt.Errorf("mobileconfig: an account with no principal URL")
	}
	if strings.Contains(p, "://") {
		return "", fmt.Errorf("mobileconfig: principal %q must be a path, not a URL: the host is carried separately", p)
	}
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	if !strings.HasSuffix(p, "/") {
		p += "/"
	}
	// Each segment is escaped, and the separators are not: a member
	// whose name needs escaping is rare and a profile that silently
	// pointed at the wrong principal would not be.
	segments := strings.Split(strings.Trim(p, "/"), "/")
	for i, seg := range segments {
		if seg == "." || seg == ".." {
			return "", fmt.Errorf("mobileconfig: principal %q may not traverse", p)
		}
		segments[i] = url.PathEscape(seg)
	}
	return "/" + strings.Join(segments, "/") + "/", nil
}

// slug makes an identifier segment out of a display name.
func slug(name string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(name) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == ' ', r == '-', r == '_':
			b.WriteByte('-')
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		return "account"
	}
	return out
}

// derivedUUID makes a stable UUID out of what the thing IS.
//
// Not random, and the reason is in the package comment: a profile
// installed twice must replace itself. The shape is a v4-looking UUID
// over a SHA-256 of the inputs — Apple checks the format and the
// uniqueness, never the randomness.
func derivedUUID(parts ...string) string {
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	h := hex.EncodeToString(sum[:16])
	return strings.ToUpper(fmt.Sprintf("%s-%s-%s-%s-%s", h[0:8], h[8:12], h[12:16], h[16:20], h[20:32]))
}

type accountData struct {
	Identifier  string
	UUID        string
	DisplayName string
	Description string
	Host        string
	Port        int
	Username    string
	Principal   string
}

type profileData struct {
	Identifier   string
	UUID         string
	DisplayName  string
	Description  string
	Organization string
	Accounts     []accountData
}

// profileTemplate renders the plist.
//
// text/template with an explicit escaper rather than encoding/xml,
// because a plist is a positional <key>/<value> dict and modelling that
// as Go structs produces markup nobody can read against Apple's own
// documentation. The cost is that escaping is this file's job, which is
// what `xmlesc` is for and what TestEveryTextValueIsEscaped checks.
var profileTemplate = template.Must(template.New("mobileconfig").
	Funcs(template.FuncMap{"x": xmlesc}).
	Parse(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>PayloadContent</key>
	<array>
{{- range .Accounts}}
		<dict>
			<key>PayloadType</key>
			<string>com.apple.caldav.account</string>
			<key>PayloadVersion</key>
			<integer>1</integer>
			<key>PayloadIdentifier</key>
			<string>{{x .Identifier}}</string>
			<key>PayloadUUID</key>
			<string>{{x .UUID}}</string>
			<key>PayloadDisplayName</key>
			<string>{{x .DisplayName}}</string>
			<key>CalDAVAccountDescription</key>
			<string>{{x .Description}}</string>
			<key>CalDAVHostName</key>
			<string>{{x .Host}}</string>
			<key>CalDAVPort</key>
			<integer>{{.Port}}</integer>
			<key>CalDAVUseSSL</key>
			<true/>
			<key>CalDAVUsername</key>
			<string>{{x .Username}}</string>
			<key>CalDAVPrincipalURL</key>
			<string>{{x .Principal}}</string>
		</dict>
{{- end}}
	</array>
	<key>PayloadType</key>
	<string>Configuration</string>
	<key>PayloadVersion</key>
	<integer>1</integer>
	<key>PayloadIdentifier</key>
	<string>{{x .Identifier}}</string>
	<key>PayloadUUID</key>
	<string>{{x .UUID}}</string>
	<key>PayloadDisplayName</key>
	<string>{{x .DisplayName}}</string>
	<key>PayloadDescription</key>
	<string>{{x .Description}}</string>
	<key>PayloadOrganization</key>
	<string>{{x .Organization}}</string>
	<key>PayloadRemovalDisallowed</key>
	<false/>
</dict>
</plist>
`))

// xmlesc escapes the five XML entities.
//
// Ampersand first, or every entity this introduces is escaped again by
// the replacements after it. A household member called "Ben & Jo" is not
// a contrived input, and an unescaped one produces a profile iOS refuses
// with no message beyond "Profile Installation Failed".
func xmlesc(v string) string {
	r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;", "'", "&apos;")
	return r.Replace(v)
}
