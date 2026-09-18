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
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/chickenrat17/wildstone-homelab/keystone/principal"
	"github.com/chickenrat17/wildstone-homelab/keystone/runtimedb"
)

// chatCapabilityPrefix is the first half of the catalog convention.
const chatCapabilityPrefix = "chat-"

// chatTransport is one way in or out of the house, per surface.
type chatTransport struct {
	// Transport is what a binding records and an adapter reports, e.g.
	// "telegram". It is the value a claim code carries.
	Transport string `json:"transport"`
	// Butler is the Agent this bot answers for, empty until it has been
	// credentialed and given an address. A bot serves one Agent
	// (ADR-W033 B7), so this is the question an operator is actually
	// asking when they look at the list — the surface it used to report
	// named a class of Agents and could not tell one from another.
	Butler string `json:"butler,omitempty"`
	// Capability is the package that provides it.
	Capability string `json:"capability"`
	// Installed is whether this deployment has that capability.
	Installed bool `json:"installed"`
	// Credentialed is whether the adapter holds a LIVE Keystone token
	// (`wild chat adapter`, or the Console). An installed adapter
	// without one is running and cannot talk to Keystone, so an invite
	// issued for it can never be redeemed.
	//
	// Deliberately not "the principal exists": credentialing creates the
	// principal, mints a token and places it on the node, and a
	// placement that fails revokes the token and leaves the principal.
	// Reading that as credentialed would mark a failed step complete.
	Credentialed bool `json:"credentialed"`
	// Node is where an installed adapter runs, from the resources the
	// capability owns. It is what a rotation or a bot-token write must
	// target: minting onto a different node revokes the running
	// adapter's token and places the replacement where nothing reads it,
	// leaving the container permanently unauthorized.
	Node string `json:"node,omitempty"`
	// Handle is the bot itself, as the adapter reported it: a Telegram
	// username, a Matrix MXID. Empty until an adapter has checked in,
	// which is also the honest answer — nothing else in the deployment
	// knows it.
	Handle string `json:"handle,omitempty"`
	// SeenAt is the adapter's last heartbeat, and Online is whether that
	// was recent enough to mean it is running now. A bot that has never
	// checked in and a bot that stopped an hour ago look identical
	// without this.
	SeenAt string `json:"seenAt,omitempty"`
	Online bool   `json:"online"`
}

// adapterOnlineWindow is how long a heartbeat counts for. The adapter
// re-registers well inside it, so a row older than this means the
// container is not running (or cannot reach Keystone, which the operator
// needs to know just as much).
const adapterOnlineWindow = 30 * time.Minute

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
	// Where each installed chat capability actually runs.
	nodes, err := s.capabilityNodes()
	if err != nil {
		return nil, err
	}

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
		transport, ok := capabilityTransport(name)
		if !ok {
			// Not a chat adapter package by the naming convention. It
			// is skipped rather than guessed at: minting a credential
			// for it would be refused for the same reason.
			continue
		}
		// Which Agent answers here, if any. Read from the address
		// rather than parsed out of the name, because the address is
		// where the assignment actually lives.
		butler := ""
		if at, err := s.DB.ChatConnectionAt(transport, name); err == nil {
			butler = at.Butler
		}
		t := chatTransport{
			Transport:    transport,
			Butler:       butler,
			Capability:   name,
			Installed:    enabled[name],
			Credentialed: s.adapterCredentialed(name),
			Node:         nodes[name],
		}
		if a, err := s.DB.GetChatAdapter(name); err == nil {
			t.Handle = a.Handle
			t.SeenAt = a.SeenAt.UTC().Format(time.RFC3339)
			t.Online = time.Since(a.SeenAt) < adapterOnlineWindow
		}
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Transport != out[j].Transport {
			return out[i].Transport < out[j].Transport
		}
		return out[i].Capability < out[j].Capability
	})
	return out, nil
}

// registerRequest is what an adapter says about itself on startup and on
// its heartbeat. It carries no claim about WHICH capability it is — that
// comes from its own principal — so the worst a compromised adapter can
// do here is lie about its own bot's handle.
type registerRequest struct {
	Transport   string `json:"transport"`
	Handle      string `json:"handle"`
	DisplayName string `json:"displayName,omitempty"`
}

// handlePatterns bound what a handle may look like, per transport. This
// is not cosmetic validation: the handle is untrusted text that ends up
// in a URL an operator is invited to tap, so anything that could carry a
// scheme, a host or a query is refused rather than escaped. A transport
// with no pattern here has no invite link and no registration.
var handlePatterns = map[string]*regexp.Regexp{
	// Telegram usernames: 5-32 of [A-Za-z0-9_], and the bot API returns
	// them without the leading @.
	"telegram": regexp.MustCompile(`^[A-Za-z0-9_]{5,32}$`),
	// Matrix user ids: @localpart:server[:port].
	"matrix": regexp.MustCompile(`^@[a-z0-9._=+/-]{1,255}:[A-Za-z0-9.-]{1,255}(:[0-9]{1,5})?$`),
}

// registerChatAdapter records which bot an adapter is (converse verb —
// it is the adapter speaking about itself, and it is the only thing it
// may say that is not a relayed message).
func (s *Server) registerChatAdapter(r *http.Request, id principal.Identity) (any, int, error) {
	var req registerRequest
	if st, err := decodeChatBody(r, &req); err != nil {
		return nil, st, err
	}
	// Which capability this is comes from the caller's own principal,
	// minted as chat-<capability> when the adapter was credentialed.
	capability, ok := strings.CutPrefix(id.Name, chatAdapterPrincipalPrefix)
	if !ok || capability == "" {
		return nil, http.StatusForbidden, fmt.Errorf(
			"only a chat adapter's own credential may register a bot")
	}
	want, ok := capabilityTransport(capability)
	if !ok {
		return nil, http.StatusBadRequest, fmt.Errorf("capability %q does not name a transport", capability)
	}
	transport := strings.TrimSpace(req.Transport)
	handle := strings.TrimSpace(strings.TrimPrefix(req.Handle, "@"))
	if transport == "matrix" {
		handle = "@" + handle
	}
	// The capability name already says which transport this is; a
	// registration that disagrees with it is refused rather than
	// believed, for the same reason an Agent is never taken from a
	// request body.
	if want != transport {
		return nil, http.StatusBadRequest, fmt.Errorf(
			"capability %q is the %s transport, but %q was reported", capability, want, transport)
	}
	pattern, known := handlePatterns[transport]
	if !known {
		return nil, http.StatusBadRequest, fmt.Errorf("transport %q has no known handle format", transport)
	}
	if !pattern.MatchString(handle) {
		return nil, http.StatusBadRequest, fmt.Errorf("that is not a %s handle", transport)
	}
	// The ceiling comes from the Agent this bot answers for. A bot with
	// no Agent yet has none, and the commands it publishes are the ones
	// every Connection offers — it cannot advertise an Owner Agent's
	// menu before anybody has given it one.
	surface := runtimedb.ButlerSurfaceFamily
	if at, aerr := s.DB.ChatConnectionAt(transport, capability); aerr == nil {
		surface = at.Surface
	}
	rec := runtimedb.ChatAdapter{
		Capability:  capability,
		Transport:   transport,
		Surface:     string(surface),
		Handle:      handle,
		DisplayName: trimDisplayName(req.DisplayName),
	}
	if err := s.DB.UpsertChatAdapter(rec); err != nil {
		return nil, http.StatusInternalServerError, err
	}
	// The commands this surface offers travel back with the
	// registration, so the adapter can publish them to the vendor's own
	// menu without carrying its own copy of the list. One table decides
	// what exists, what /help says, and what the menu shows — an
	// adapter with its own copy would drift, and the drift that matters
	// is a household bot advertising /status.
	//
	// registerLoop repeats on a heartbeat, so a command added here
	// reaches the menu on the next beat rather than on a redeploy.
	return map[string]any{
		"capability": capability,
		"handle":     handle,
		"surface":    string(surface),
		"commands":   commandsFor(surface),
		"butler":     butlerAt(s, transport, capability),
	}, http.StatusOK, nil
}

// inviteLink turns a claim code into something a person can tap.
//
// It is built HERE, from a validated handle and a code Keystone just
// minted, rather than from anything an adapter sent: a URL supplied by
// the adapter would be a link the operator trusts and the adapter chose,
// which is how a code ends up pasted into somebody else's bot.
//
// Telegram carries the code in the link (the bot receives it as
// "/start <code>"), so the person taps once and is bound. Matrix has no
// such payload, so the link only opens the conversation and the code is
// still sent by hand.
func inviteLink(transport, handle, code string) (url string, carriesCode bool) {
	if handle == "" {
		return "", false
	}
	switch transport {
	case "telegram":
		return "https://t.me/" + handle + "?start=" + code, true
	case "matrix":
		return "https://matrix.to/#/" + handle, false
	}
	return "", false
}

// linkForClaim finds the bot a code should be sent to, if one has
// checked in. An unknown transport, an adapter that has never
// registered, or a code that names no transport all yield no link — the
// code still works, and the note still says what to do with it.
func (s *Server) linkForClaim(at runtimedb.Connection, code string) (link, handle string, carriesCode bool) {
	// The Connection's external id is the capability the bot was minted
	// for, so there is no name to reconstruct from a transport and a
	// surface — which could only ever have produced the two bots the
	// catalog happens to ship.
	a, err := s.DB.GetChatAdapter(at.ExternalID)
	if errors.Is(err, runtimedb.ErrNotFound) || err != nil {
		return "", "", false
	}
	link, carriesCode = inviteLink(a.Transport, a.Handle, code)
	return link, a.Handle, carriesCode
}

// adapterCredentialed reports whether a chat capability's adapter can
// actually authenticate: its principal exists, is enabled, and holds at
// least one token that is neither revoked nor expired.
//
// A disabled principal and a revoked token count the same as no
// credential at all, because that is what they are from the adapter's
// side — the request is refused either way.
func (s *Server) adapterCredentialed(capability string) bool {
	name := chatAdapterPrincipalPrefix + capability
	p, err := s.DB.GetPrincipal(name)
	if err != nil || p.Disabled {
		return false
	}
	live, err := s.DB.LiveTokensFor(name)
	return err == nil && live > 0
}

// capabilityNodes maps each capability to the node its resources are on.
// A capability spanning nodes has no single answer, and reports none
// rather than an arbitrary one.
func (s *Server) capabilityNodes() (map[string]string, error) {
	current, err := s.State.Load()
	if err != nil {
		return nil, fmt.Errorf("loading statefile: %w", err)
	}
	nodes := map[string]string{}
	split := map[string]bool{}
	for _, r := range current {
		name, ok := strings.CutPrefix(r.Metadata.Owner, "capability/")
		if !ok || name == "" || r.Metadata.Node == "" {
			continue
		}
		switch {
		case split[name]:
		case nodes[name] == "":
			nodes[name] = r.Metadata.Node
		case nodes[name] != r.Metadata.Node:
			split[name] = true
			delete(nodes, name)
		}
	}
	return nodes, nil
}

// capabilityTransport reads the transport out of a chat capability's
// name.
//
// The catalog convention is chat-<transport>-<something>, and the
// transport is the part that still matters: it says which API the bot
// speaks. The trailing segment used to be the surface and was
// load-bearing — it decided which Butler the bot was wired to — but an
// Agent is named explicitly now, so the suffix is only a name.
func capabilityTransport(capability string) (string, bool) {
	rest, ok := strings.CutPrefix(capability, chatCapabilityPrefix)
	if !ok {
		return "", false
	}
	transport, _, ok := strings.Cut(rest, "-")
	if !ok || transport == "" {
		return "", false
	}
	return transport, true
}

// butlerAt names the Agent a bot answers for, or "" while it has none.
// It is reported back to the adapter so an operator reading its logs
// can see which Agent it came up as.
func butlerAt(s *Server, transport, capability string) string {
	at, err := s.DB.ChatConnectionAt(transport, capability)
	if err != nil {
		return ""
	}
	return at.Butler
}

// adapterConnection resolves the Connection a chat adapter's credential
// stands for.
//
// The transport is DERIVED from the capability rather than left out of
// the lookup. butler_addresses is unique on (transport, external_id),
// not on external_id alone — AddButlerAddress takes transport-local ids
// on purpose — so two live addresses on different transports may carry
// the same one. Resolving on the id by itself would hand an
// authenticated adapter whichever of them the query happened to return:
// the wrong Agent, and with it the wrong queue to poll and the wrong
// Butler to answer as.
//
// A capability whose name does not say its transport has no address to
// resolve, which is ErrNoConnection rather than a guess.
func (s *Server) adapterConnection(capability string) (runtimedb.Connection, error) {
	transport, ok := capabilityTransport(capability)
	if !ok {
		return runtimedb.Connection{}, runtimedb.ErrNoConnection
	}
	return s.DB.ChatConnectionAt(transport, capability)
}
