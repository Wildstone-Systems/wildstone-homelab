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
	"slices"
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
	Surface   string `json:"surface"`
	Identity  string `json:"identity"`
	Transport string `json:"transport,omitempty"`
	// TTLSeconds overrides the default expiry; 0 uses it.
	TTLSeconds int `json:"ttlSeconds,omitempty"`
}

// claimResponse carries the code exactly once. It is not readable again
// from any endpoint — the store keeps only its hash — so the operator
// gets one chance to pass it on, the same contract a minted token has.
type claimResponse struct {
	Code      string `json:"code"`
	Surface   string `json:"surface"`
	Identity  string `json:"identity"`
	Transport string `json:"transport,omitempty"`
	ExpiresAt string `json:"expiresAt"`
	Note      string `json:"note"`
}

// issueChatClaim mints a claim code (ADR-W026 A4). Manage verb: this is
// the decision about who is allowed to talk to a Butler at all.
func (s *Server) issueChatClaim(r *http.Request, id principal.Identity) (any, int, error) {
	var req claimRequest
	if st, err := decodeChatBody(r, &req); err != nil {
		return nil, st, err
	}
	surface := runtimedb.ButlerSurface(strings.TrimSpace(req.Surface))
	if !runtimedb.ValidButlerSurface(surface) {
		return nil, http.StatusBadRequest, fmt.Errorf("surface must be admin or family")
	}
	identity := strings.TrimSpace(req.Identity)
	transport := strings.TrimSpace(req.Transport)
	if st, err := s.checkClaimTarget(surface, identity, transport); err != nil {
		return nil, st, err
	}
	ttl := time.Duration(req.TTLSeconds) * time.Second
	code, rec, err := s.Principals.IssueChatClaim(surface, identity, transport, id.Name, ttl)
	if err != nil {
		return nil, http.StatusBadRequest, err
	}
	s.event("chat.claim_issued", rec.Identity, id,
		fmt.Sprintf("claim code issued for the %s surface, expires %s", rec.Surface, rec.ExpiresAt.UTC().Format(time.RFC3339)))

	note := fmt.Sprintf("Send this code to the %s bot as a direct message. It works once, expires %s, and can only bind %q on the %s surface.",
		orAny(rec.Transport), rec.ExpiresAt.UTC().Format(time.RFC3339), rec.Identity, rec.Surface)
	return claimResponse{
		Code:      code,
		Surface:   string(rec.Surface),
		Identity:  rec.Identity,
		Transport: rec.Transport,
		ExpiresAt: rec.ExpiresAt.UTC().Format(time.RFC3339),
		Note:      note,
	}, http.StatusCreated, nil
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
func (s *Server) checkClaimTarget(surface runtimedb.ButlerSurface, identity, transport string) (int, error) {
	// The admin half is checked where it lives: IssueChatClaim resolves
	// the principal, and re-checking here would be a second copy to
	// drift from.
	if surface == runtimedb.ButlerSurfaceFamily {
		members, err := s.householdMemberNames()
		if err != nil {
			return http.StatusInternalServerError, err
		}
		if !members[identity] {
			return http.StatusBadRequest, fmt.Errorf(
				"%q is not a household member: add them first, then invite them", identity)
		}
	}

	transports, err := s.chatTransports()
	if err != nil {
		return http.StatusInternalServerError, err
	}
	installed := []string{}
	for _, t := range transports {
		if t.Installed && t.Surface == string(surface) {
			installed = append(installed, t.Transport)
		}
	}
	if len(installed) == 0 {
		return http.StatusBadRequest, fmt.Errorf(
			"no %s chat adapter is installed: there is no bot to send a code to (install chat-<transport>-%s)",
			surface, surface)
	}
	if transport == "" {
		// A code with no transport is redeemable on any of them, which
		// is only meaningful because at least one exists.
		return http.StatusOK, nil
	}
	if !slices.Contains(installed, transport) {
		return http.StatusBadRequest, fmt.Errorf(
			"no %s adapter on the %s surface: this deployment has %s",
			transport, surface, strings.Join(installed, ", "))
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
	Code        string `json:"code"`
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
	transport := strings.TrimSpace(req.Transport)
	account := strings.TrimSpace(req.Account)
	if transport == "" || account == "" {
		return nil, http.StatusBadRequest, fmt.Errorf("transport and account are required")
	}
	// The redeeming BOT's own surface, from its role. A code for the
	// other surface is then refused rather than burned: redeeming a
	// family code on the operator's bot used to succeed, spend the code,
	// and leave the person bound on a surface that bot never resolves —
	// so they read as unbound, with nothing left to retry.
	//
	// A human caller (the Console, `wild`) is pinned to no surface and
	// constrains nothing: they can redeem on behalf of whoever is
	// standing next to them, which is the whole point of a claim code.
	want, _ := adapterSurface(id)
	binding, err := s.Principals.RedeemChatClaim(req.Code, transport, account,
		trimDisplayName(req.DisplayName), strings.TrimSpace(req.Conversation), want)
	if errors.Is(err, runtimedb.ErrClaimCode) {
		// One refusal for every reason (unknown, expired, spent, wrong
		// transport): an adapter relaying codes for strangers must not be
		// able to tell them apart.
		s.event("chat.claim_refused", "", id, fmt.Sprintf("claim refused for %s account", transport))
		return nil, http.StatusForbidden, fmt.Errorf("that code is not valid")
	}
	if err != nil {
		return nil, http.StatusInternalServerError, err
	}
	s.event("chat.bound", binding.Identity, id,
		fmt.Sprintf("%s account bound to %q on the %s surface (binding %d)",
			binding.Transport, binding.Identity, binding.Surface, binding.ID))
	return binding, http.StatusCreated, nil
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
