package api

// The pending one-time code, read back for the admin (F78, ADR-W020 A6).
//
// Authelia delivers a one-time code by writing a file, because there is
// no mail relay and there will not be one. Without somewhere to read it
// from, two-factor could not be ENROLLED by the person it protects: the
// code lived inside a container, rotated silently on every request, and
// retrying tripped a rate limiter. That is a lockout, and it happened.
//
// A6 already decided delivery is out-of-band and human — the admin gets
// the code and hands it over. This is the surface that makes that
// possible without root, docker, and knowing which file.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"

	"github.com/chickenrat17/wildstone-homelab/keystone/principal"
	"github.com/chickenrat17/wildstone-homelab/keystone/redact"
	wsv1 "github.com/chickenrat17/wildstone-homelab/proto/gen/wardstone/v1"
)

// identityNotificationPath is where the identity package points
// Authelia's filesystem notifier. Under a managed prefix, so the agent
// can read it with the op it already has.
func identityNotificationPath() string { return "/srv/wildstone/identity/notification.txt" }

// oneTimeCodeRegexp matches the code Authelia writes on its own line.
// Deliberately anchored: the notification also contains a revocation
// URL, and handing an operator the wrong 8 characters wastes an attempt
// they are rate-limited on.
var oneTimeCodeRegexp = regexp.MustCompile(`(?m)^[A-Z0-9]{8}$`)

// resetLinkScanRegexp finds candidate links in the notification. Every
// candidate is then checked by resetLinkFrom; the regexp only narrows.
var resetLinkScanRegexp = regexp.MustCompile(`https://[^\s"'<>]+`)

// resetLinkPath is the only page a reset link may point at: Authelia's
// second password-reset step, which consumes the token.
const resetLinkPath = "/reset-password/step2"

// resetLinkFrom returns the one password-reset link in body, or "" when
// there is not exactly one well-formed link. Well-formed is strict on
// purpose: this value skips redaction (#463), so anything that is not
// unmistakably a reset link — another scheme, credentials in the host,
// a different page, extra parameters, a fragment — is not returned.
func resetLinkFrom(body string) string {
	found := ""
	for _, m := range resetLinkScanRegexp.FindAllString(body, -1) {
		u, err := url.Parse(m)
		if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil ||
			u.Path != resetLinkPath || u.Fragment != "" || u.RawFragment != "" {
			continue
		}
		q, err := url.ParseQuery(u.RawQuery)
		if err != nil || len(q) != 1 || len(q["token"]) != 1 || q.Get("token") == "" {
			continue
		}
		if found != "" && found != m {
			// Two different links: handing over the wrong one wastes
			// the reset, so hand over neither.
			return ""
		}
		found = m
	}
	return found
}

type identityNotification struct {
	// Code is the one-time code, when the file holds one.
	Code string `json:"code,omitempty"`
	// ResetLink is the password-reset link, when the file holds one. It
	// is the one field this response carries unredacted (see redacted).
	ResetLink string `json:"resetLink,omitempty"`
	// Body is the whole notification, so an admin can see what they are
	// handing over. It is redacted like any other body: a reset link in
	// it arrives with its token replaced, which is why ResetLink exists.
	Body string `json:"body,omitempty"`
	// Note is what the operator most needs to know and cannot see:
	// requesting another code REPLACES this one. That rotation silently
	// invalidated a code mid-handover during v0.1.9-alpha testing.
	Note string `json:"note"`
}

func (s *Server) identityCode(r *http.Request, _ principal.Identity) (any, int, error) {
	node := r.PathValue("node")
	if node == "" {
		return nil, http.StatusBadRequest, fmt.Errorf("node required")
	}
	if s.Agents == nil {
		return nil, http.StatusServiceUnavailable, fmt.Errorf("no agent connection is configured")
	}
	res, err := s.Agents.Apply(r.Context(), node, &wsv1.Operation{
		Op: &wsv1.Operation_ReadManagedFile{
			ReadManagedFile: &wsv1.ReadManagedFile{Path: identityNotificationPath()},
		},
	})
	if err != nil {
		return nil, http.StatusBadGateway, fmt.Errorf("reading %s on %s: %w", identityNotificationPath(), node, err)
	}
	body := string(res.GetReadManagedFile().GetContent())
	out := identityNotification{
		Body: body,
		Note: "Requesting another code replaces this one. Read it, hand it over, and do not request again until it has been used.",
	}
	if m := oneTimeCodeRegexp.FindString(strings.TrimSpace(body)); m != "" {
		out.Code = m
	}
	out.ResetLink = resetLinkFrom(body)
	return out, http.StatusOK, nil
}

// redacted is this response as writeJSON sends it: redacted like any
// other body EXCEPT ResetLink (#463). The link's token is exactly the
// shape redaction catches, and a redacted link cannot complete a reset,
// so an admin had nothing to hand over. It reaches only VerbManage
// callers, the same people who can already mint tokens. The rest — the
// free-text body in particular — is still redacted.
func (n identityNotification) redacted() ([]byte, error) {
	link := n.ResetLink
	n.ResetLink = ""
	raw, err := json.Marshal(n)
	if err != nil {
		return nil, err
	}
	var clean identityNotification
	if err := json.Unmarshal([]byte(redact.String(string(raw))), &clean); err != nil {
		return nil, err
	}
	clean.ResetLink = link
	return json.Marshal(clean)
}
