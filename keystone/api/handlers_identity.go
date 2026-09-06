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
	"fmt"
	"net/http"
	"regexp"
	"strings"

	"github.com/chickenrat17/wildstone-homelab/keystone/principal"
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

type identityNotification struct {
	// Code is the one-time code, when the file holds one.
	Code string `json:"code,omitempty"`
	// Body is the whole notification, because Authelia also sends
	// password-reset links through the same channel and an admin should
	// see what they are actually handing over.
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
	return out, http.StatusOK, nil
}
