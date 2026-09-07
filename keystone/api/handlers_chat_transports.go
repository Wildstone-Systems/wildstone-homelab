package api

// Which transports this deployment can actually invite somebody on
// (ADR-W026 A3).
//
// A claim code names a transport, and until now the operator typed one.
// Typing "telegram" into a deployment with no Telegram adapter installed
// produces a code that is valid, unredeemable and indistinguishable from
// a working one: there is no bot to send it to. The set of real answers
// is not open — it is the chat capabilities this deployment has
// installed, whose names carry both halves of the answer
// (chat-<transport>-<surface>, the convention capabilitySurface already
// depends on).
//
// The catalog half matters as much as the installed half: an operator
// with no adapter needs to be told which package to install, not shown
// an empty list.

import (
	"net/http"
	"sort"
	"strings"

	"github.com/chickenrat17/wildstone-homelab/keystone/principal"
)

// chatCapabilityPrefix is the first half of the catalog convention.
const chatCapabilityPrefix = "chat-"

// chatTransport is one way in or out of the house, per surface.
type chatTransport struct {
	// Transport is what a binding records and an adapter reports, e.g.
	// "telegram". It is the value a claim code carries.
	Transport string `json:"transport"`
	Surface   string `json:"surface"`
	// Capability is the package that provides it.
	Capability string `json:"capability"`
	// Installed is whether this deployment has that capability.
	Installed bool `json:"installed"`
	// Credentialed is whether the adapter has been given its Keystone
	// token (`wild chat adapter`, or the Services screen). An installed
	// adapter without one is running and cannot talk to Keystone, so an
	// invite issued for it will never be redeemed.
	Credentialed bool `json:"credentialed"`
}

// listChatTransports enumerates the chat capabilities in the catalog,
// marking which are installed and credentialed. Manage verb: it is read
// beside the binding list, by the same person deciding who may talk to
// the house.
func (s *Server) listChatTransports(_ *http.Request, _ principal.Identity) (any, int, error) {
	out, err := s.chatTransports()
	if err != nil {
		return nil, http.StatusInternalServerError, err
	}
	return out, http.StatusOK, nil
}

// chatTransports is the same answer as the endpoint, for the callers
// inside the process — issuing a code checks an invite against exactly
// what the Console offered.
func (s *Server) chatTransports() ([]chatTransport, error) {
	enabled, err := s.enabledCapabilities()
	if err != nil {
		return nil, err
	}

	// Start from the catalog when there is one, so an operator with
	// nothing installed still learns what to install. Fall back to the
	// installed set alone: a deployment running without -catalog-dir
	// still has adapters, and they still work.
	names := map[string]bool{}
	for _, p := range s.Catalog.List() {
		if strings.HasPrefix(p.Metadata.Capability, chatCapabilityPrefix) {
			names[p.Metadata.Capability] = true
		}
	}
	for name := range enabled {
		if strings.HasPrefix(name, chatCapabilityPrefix) {
			names[name] = true
		}
	}

	out := []chatTransport{}
	for name := range names {
		surface, ok := capabilitySurface(name)
		if !ok {
			// Not a chat adapter package by the naming convention. It
			// is skipped rather than guessed at: minting a credential
			// for it would be refused for the same reason.
			continue
		}
		transport := strings.TrimSuffix(strings.TrimPrefix(name, chatCapabilityPrefix), "-"+string(surface))
		if transport == "" {
			continue
		}
		out = append(out, chatTransport{
			Transport:    transport,
			Surface:      string(surface),
			Capability:   name,
			Installed:    enabled[name],
			Credentialed: s.adapterCredentialed(name),
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Transport != out[j].Transport {
			return out[i].Transport < out[j].Transport
		}
		return out[i].Surface < out[j].Surface
	})
	return out, nil
}

// adapterCredentialed reports whether the adapter principal for a chat
// capability exists and is live. A disabled one counts as absent: its
// token is refused, which is the same thing from the invite's point of
// view.
func (s *Server) adapterCredentialed(capability string) bool {
	p, err := s.DB.GetPrincipal(chatAdapterPrincipalPrefix + capability)
	if err != nil {
		return false
	}
	return !p.Disabled
}
