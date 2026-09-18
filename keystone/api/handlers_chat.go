package api

// Chat bindings and claim codes (ADR-W026 A4, work package G2).
//
// This is the identity half of the messaging layer, and it is the half
// that decides whether the rest of it is safe. An adapter authenticates
// as itself and NAMES the chat account that spoke; these endpoints are
// the only way that name becomes a person, and the only way a binding is
// ever written is a human issuing a code and somebody redeeming it once.
//
// Issuing is a `manage` act: deciding who may talk to the homelab is
// principal administration, and it sits beside minting a token, which is
// what it effectively is. Redeeming is a `converse` act, because the
// caller is the adapter, and a concierge holds nothing else.

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/chickenrat17/wildstone-homelab/keystone/principal"
	"github.com/chickenrat17/wildstone-homelab/keystone/resource"
	"github.com/chickenrat17/wildstone-homelab/keystone/runtimedb"
)

// maxChatBodyBytes bounds every chat-plane request body. These carry an
// account id and a short code, never content.
const maxChatBodyBytes = 8 << 10

type claimRequest struct {
	// Butler is the Agent this invite admits somebody to. It replaced
	// Surface, and the difference is the whole of ADR-W033 B7: a
	// surface is a class of Agents, so an invite to `admin` admitted
	// somebody to every Owner Agent at once.
	Butler    string `json:"butler"`
	Identity  string `json:"identity"`
	Transport string `json:"transport"`
	// TTLSeconds overrides the default expiry; 0 uses it.
	TTLSeconds int `json:"ttlSeconds,omitempty"`
}

// claimResponse carries the code exactly once. It is not readable again
// from any endpoint — the store keeps only its hash — so the operator
// gets one chance to pass it on, the same contract a minted token has.
type claimResponse struct {
	Code      string `json:"code"`
	Butler    string `json:"butler"`
	Identity  string `json:"identity"`
	Transport string `json:"transport"`
	ExpiresAt string `json:"expiresAt"`
	Note      string `json:"note"`
	// Handle is the bot this code should be sent to, and Link opens a
	// chat with it. Both are empty until an adapter for that transport
	// has checked in: nothing else in the deployment knows which bot
	// the token belongs to (the token is write-only).
	//
	// On Telegram the link CARRIES the code, so the person taps it and
	// is bound without typing anything. Elsewhere it only opens the
	// conversation, and the code is still read out.
	Handle string `json:"handle,omitempty"`
	Link   string `json:"link,omitempty"`
}

// issueChatClaim mints a claim code (ADR-W026 A4). Manage verb: this is
// the decision about who is allowed to talk to a Butler at all.
func (s *Server) issueChatClaim(r *http.Request, id principal.Identity) (any, int, error) {
	var req claimRequest
	if st, err := decodeChatBody(r, &req); err != nil {
		return nil, st, err
	}
	identity := strings.TrimSpace(req.Identity)
	butler := strings.TrimSpace(req.Butler)
	transport := strings.TrimSpace(req.Transport)
	if butler == "" || transport == "" {
		return nil, http.StatusBadRequest,
			fmt.Errorf("an invite names the butler it admits somebody to, and the transport it arrives on")
	}
	at, st, err := s.connectionNamed(butler, transport)
	if err != nil {
		return nil, st, err
	}
	if st, err := s.checkClaimTarget(at, identity); err != nil {
		return nil, st, err
	}
	ttl := time.Duration(req.TTLSeconds) * time.Second
	code, rec, err := s.Principals.IssueChatClaim(at, identity, id.Name, ttl)
	if err != nil {
		return nil, http.StatusBadRequest, err
	}
	s.event("chat.claim_issued", rec.Identity, id,
		fmt.Sprintf("claim code issued for %s on %s, expires %s",
			at.Butler, at.Transport, rec.ExpiresAt.UTC().Format(time.RFC3339)))

	link, handle, carriesCode := s.linkForClaim(at, code)
	expires := rec.ExpiresAt.UTC().Format(time.RFC3339)
	note := fmt.Sprintf("Send this code to the %s bot as a direct message. It works once, expires %s, and admits %q to %s and to nothing else.",
		at.Transport, expires, rec.Identity, at.Butler)
	switch {
	case carriesCode:
		// The link IS the code in tappable form, which is worth saying
		// plainly: forwarding the link is forwarding the invite.
		note = fmt.Sprintf("Send this link to %s. It opens @%s and carries the code, so there is nothing to type — and anyone who has the link can use it. It works once, expires %s, and admits them to %s alone.",
			rec.Identity, handle, expires, at.Butler)
	case link != "":
		note = fmt.Sprintf("The link opens a chat with %s; %s still sends the code itself. It works once, expires %s, and admits them to %s alone.",
			handle, rec.Identity, expires, at.Butler)
	}
	return claimResponse{
		Code:      code,
		Butler:    at.Butler,
		Identity:  rec.Identity,
		Transport: at.Transport,
		ExpiresAt: rec.ExpiresAt.UTC().Format(time.RFC3339),
		Note:      note,
		Handle:    handle,
		Link:      link,
	}, http.StatusCreated, nil
}

// connectionNamed resolves the Connection an operator asked for by
// Agent and transport.
//
// Ambiguity is an error rather than a choice: B7 lets an Agent hold
// several Connections on one transport, and an invite issued for the
// wrong one of somebody's two Telegram bots is an invite that works and
// admits them somewhere they did not mean to go.
func (s *Server) connectionNamed(butler, transport string) (runtimedb.Connection, int, error) {
	at, err := s.DB.ChatConnectionForButler(butler, transport)
	switch {
	case errors.Is(err, runtimedb.ErrNoConnection):
		return runtimedb.Connection{}, http.StatusBadRequest, fmt.Errorf(
			"%s has no %s connection: give it one first (`wild butler address`), then invite somebody to it",
			butler, transport)
	case errors.Is(err, runtimedb.ErrAmbiguousConnection):
		return runtimedb.Connection{}, http.StatusBadRequest, err
	case err != nil:
		return runtimedb.Connection{}, http.StatusInternalServerError, err
	}
	return at, 0, nil
}

// checkClaimTarget refuses an invite that could never be redeemed
// (ADR-W026 A3/A4). Both halves of a code — who it binds and where it
// is redeemed — have a closed set of real answers, and a code naming
// anything else is not a small mistake: it looks exactly like a working
// one, and the operator finds out when somebody they invited says
// nothing ever happened.
//
// The family half is what changed with ADR-W020 A3. When a household
// member was an LDAP account this control plane did not own, a typo
// could only be caught by a person; now a member IS a declared
// `LdapUser`, so the roster is right here to check against.
func (s *Server) checkClaimTarget(at runtimedb.Connection, identity string) (int, error) {
	// Whether the named Person may be admitted is checked where it
	// lives: IssueChatClaim resolves them, requires them admitted, and
	// requires an Owner where the Connection belongs to an Owner Agent
	// (ADR-W033 B9). Re-checking here would be a second copy to drift
	// from.
	//
	// A family-surface Connection additionally wants the declared
	// roster, because since ADR-W020 A3 a household member IS a declared
	// LdapUser and a typo is catchable here rather than by a person.
	if !at.AdmitsOwnersOnly() {
		members, err := s.householdMemberNames()
		if err != nil {
			return http.StatusInternalServerError, err
		}
		if !members[identity] {
			return http.StatusBadRequest, fmt.Errorf(
				"%q is not a household member: add them first, then invite them", identity)
		}
	}

	// An adapter with no live Keystone token cannot redeem anything, so
	// a code issued for its Connection is exactly the unredeemable
	// invite this check exists to prevent. That state is reachable —
	// installing through Services without credentialing, or a setup
	// interrupted between the two.
	//
	// The capability is the Connection's own external id: the bot a
	// message arrives at IS the address (ADR-W031 A7), so the address
	// names the bot whose credential this is.
	if !s.adapterCredentialed(at.ExternalID) {
		return http.StatusBadRequest, fmt.Errorf(
			"the %s adapter %s has no Keystone token, so it cannot redeem a code: credential it first (wild chat adapter %s -node <node>)",
			at.Transport, at.ExternalID, at.ExternalID)
	}
	return http.StatusOK, nil
}

// householdMemberNames is the declared roster as a set.
func (s *Server) householdMemberNames() (map[string]bool, error) {
	current, err := s.State.Load()
	if err != nil {
		return nil, fmt.Errorf("loading statefile: %w", err)
	}
	names := map[string]bool{}
	for _, r := range current {
		if r.Kind == resource.KindLdapUser && r.LdapUser != nil && r.LdapUser.Username != "" {
			names[r.LdapUser.Username] = true
		}
	}
	return names, nil
}

func orAny(transport string) string {
	if transport == "" {
		return "chat"
	}
	return transport
}

type redeemRequest struct {
	Code string `json:"code"`
	// Butler names the Agent whose Connection this code is for, and is
	// for a HUMAN caller only — an adapter's own credential is its
	// Connection (ADR-W031 A7), so a bot that could name one could name
	// somebody else's.
	Butler      string `json:"butler,omitempty"`
	Transport   string `json:"transport"`
	Account     string `json:"account"`
	DisplayName string `json:"displayName,omitempty"`
	// Conversation is the direct chat the code arrived in — where a
	// household rule should reach this person later. On Telegram it
	// equals the account; on Matrix it is the room, and the account (an
	// MXID) cannot be sent to (ADR-W026 A8). Optional: a transport that
	// does not distinguish them can leave it out.
	Conversation string `json:"conversation,omitempty"`
}

// redeemChatClaim exchanges a code for a binding. The caller is an
// adapter holding `converse`; it supplies the transport and the account
// id it saw, and nothing it sends decides WHICH identity gets bound —
// that was fixed when the code was issued.
func (s *Server) redeemChatClaim(r *http.Request, id principal.Identity) (any, int, error) {
	var req redeemRequest
	if st, err := decodeChatBody(r, &req); err != nil {
		return nil, st, err
	}
	account := strings.TrimSpace(req.Account)
	if account == "" {
		return nil, http.StatusBadRequest, fmt.Errorf("an account is required")
	}
	// The redeeming BOT's own Connection. A code for a different Agent
	// is then refused rather than burned: redeeming Alfred's code on
	// Robert's bot would otherwise succeed, spend the code, and leave
	// the person admitted somewhere that bot never resolves — so they
	// read as unbound, with nothing left to retry.
	//
	// This used to compare SURFACES, which could not tell Alfred from
	// Robert at all: both are Owner Agents, so a code for either was
	// redeemable on either's bot.
	at, st, err := s.redeemingConnection(id, req)
	if err != nil {
		return nil, st, err
	}
	binding, err := s.Principals.RedeemChatClaim(req.Code, at, account,
		trimDisplayName(req.DisplayName), strings.TrimSpace(req.Conversation))
	if errors.Is(err, runtimedb.ErrClaimCode) {
		// One refusal for every reason (unknown, expired, spent, wrong
		// transport): an adapter relaying codes for strangers must not be
		// able to tell them apart.
		s.event("chat.claim_refused", "", id, fmt.Sprintf("claim refused for %s account", at.Transport))
		return nil, http.StatusForbidden, fmt.Errorf("that code is not valid")
	}
	if err != nil {
		return nil, http.StatusInternalServerError, err
	}
	s.event("chat.bound", binding.Identity, id,
		fmt.Sprintf("%s account admitted for %q to %s (binding %d)",
			binding.Transport, binding.Identity, binding.Butler, binding.ID))
	return binding, http.StatusCreated, nil
}

// redeemingConnection is the Connection a redemption arrives at.
//
// A chat adapter IS one: its credential carries the capability it was
// minted for, a live address holds that capability as its external id,
// and that address names the Agent. So the bot cannot claim to be
// somebody else's Connection, and does not get to name one.
//
// A human — the Console holder, or `wild` — has no bot to arrive
// through, so they say which Agent they are redeeming for. That is not
// an escalation: issuing the code was the decision, and A10 gives the
// Console holder every Butler anyway.
func (s *Server) redeemingConnection(id principal.Identity, req redeemRequest) (runtimedb.Connection, int, error) {
	if capability, isAdapter := butlerAddressFor(id); isAdapter {
		if strings.TrimSpace(req.Butler) != "" {
			return runtimedb.Connection{}, http.StatusBadRequest, fmt.Errorf(
				"a chat adapter cannot name a butler; the bot a code arrives at is the connection (ADR-W031 A7)")
		}
		at, err := s.DB.ChatConnectionByExternalID(capability)
		if errors.Is(err, runtimedb.ErrNoConnection) {
			return runtimedb.Connection{}, http.StatusBadRequest, fmt.Errorf(
				"no butler answers at %s: give one that address before anybody is invited to it", capability)
		}
		if err != nil {
			return runtimedb.Connection{}, http.StatusInternalServerError, err
		}
		return at, 0, nil
	}
	butler, transport := strings.TrimSpace(req.Butler), strings.TrimSpace(req.Transport)
	if butler == "" || transport == "" {
		return runtimedb.Connection{}, http.StatusBadRequest, fmt.Errorf(
			"name the butler and transport: this credential is not a chat adapter, so there is no connection to resolve one from")
	}
	return s.connectionNamed(butler, transport)
}

// trimDisplayName bounds the transport-supplied label. It is untrusted
// text that an operator will read in a list, so it is kept short and is
// never used for anything but display.
func trimDisplayName(name string) string {
	const maxDisplayName = 64
	name = strings.TrimSpace(name)
	if len(name) > maxDisplayName {
		name = name[:maxDisplayName]
	}
	return name
}

// listChatBindings is the operator's answer to "who can talk to my
// homelab, and as whom". Manage verb: it is an access list.
func (s *Server) listChatBindings(r *http.Request, _ principal.Identity) (any, int, error) {
	withRevoked := r.URL.Query().Get("revoked") == "true"
	bindings, err := s.DB.ListChatBindings(withRevoked)
	if err != nil {
		return nil, http.StatusInternalServerError, err
	}
	if bindings == nil {
		bindings = []runtimedb.ChatBinding{}
	}
	return bindings, http.StatusOK, nil
}

// revokeChatBinding withdraws one binding; it takes effect on the next
// message, because every lookup filters revoked rows out.
func (s *Server) revokeChatBinding(r *http.Request, id principal.Identity) (any, int, error) {
	raw := r.PathValue("id")
	bindingID, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return nil, http.StatusBadRequest, fmt.Errorf("binding id must be a number")
	}
	if err := s.DB.RevokeChatBinding(bindingID); errors.Is(err, runtimedb.ErrNotFound) {
		return nil, http.StatusNotFound, fmt.Errorf("no live binding %d", bindingID)
	} else if err != nil {
		return nil, http.StatusInternalServerError, err
	}
	s.event("chat.revoked", raw, id, fmt.Sprintf("chat binding %s revoked", raw))
	return map[string]any{"revoked": bindingID}, http.StatusOK, nil
}

// decodeChatBody reads a bounded JSON body from a chat-plane request.
func decodeChatBody(r *http.Request, out any) (int, error) {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxChatBodyBytes+1))
	if err != nil {
		return http.StatusBadRequest, fmt.Errorf("reading body: %w", err)
	}
	if len(body) > maxChatBodyBytes {
		return http.StatusRequestEntityTooLarge, fmt.Errorf("body exceeds %d bytes", maxChatBodyBytes)
	}
	if err := json.Unmarshal(body, out); err != nil {
		return http.StatusBadRequest, fmt.Errorf("parsing request: %w", err)
	}
	return http.StatusOK, nil
}
