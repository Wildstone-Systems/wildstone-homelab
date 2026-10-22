package api

// Pairing with PantryOS (ADR-W044 A6, #384).
//
// An Owner types PantryOS's address and the one-time code PantryOS shows.
// Keystone then, in one call:
//
//  1. mints a fresh publisher token for the pantry capability — without
//     revoking the current one yet;
//  2. hands it to PantryOS's pairing exchange with the code and Keystone's
//     own address, and receives an agent key back;
//  3. checks that key against PantryOS's pantry-1 capability list;
//  4. seals the key in the runtime DB (never returned by any route), and
//     only then revokes the publisher tokens the pairing replaces.
//
// A failure at any step revokes the token minted in step 1 and leaves the
// previous pairing, if any, exactly as it was. Neither secret is ever in a
// response: this is how a PantryOS on another machine receives a
// publisher token at all, since /household/publishers never shows one.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/Wildstone-Systems/wildstone-homelab/keystone/adapter"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/adapter/pantry"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/capability"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/principal"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/resource"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/runtimedb"
	wsv1 "github.com/Wildstone-Systems/wildstone-homelab/proto/gen/wardstone/v1"
)

// PantryCapability is the capability a PantryOS pairing is recorded under.
const PantryCapability = "pantry"

const pantryExchangePath = "/api/integrations/wildstone/pairing/exchange"

type pantryPairRequest struct {
	// Address is PantryOS, e.g. https://pantry.example.internal.
	Address string `json:"address"`
	// Code is the one-time code PantryOS showed.
	Code string `json:"code"`
	// KeystoneURL is where PantryOS should send household events: this
	// Keystone's address as PantryOS's machine reaches it.
	KeystoneURL string `json:"keystoneUrl"`
}

// pairingURL accepts an http(s) address with a host and nothing a pairing
// has no use for: no credentials, query or fragment.
func pairingURL(field, raw string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" ||
		u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return "", fmt.Errorf("%s must be an http(s) address such as https://pantry.example.internal", field)
	}
	// Both long-lived credentials cross this address: the publisher token
	// to PantryOS, the agent key back on every call, the token again on
	// every event. Plain http is accepted only on the household's own
	// network — this machine, the LAN, the tailnet (itself encrypted) or a
	// local name; anywhere else it must be https.
	if u.Scheme == "http" && !privateHost(u.Hostname()) {
		return "", fmt.Errorf("%s must use https: %s is not on this network, and pairing sends credentials to it", field, u.Hostname())
	}
	return strings.TrimRight(u.String(), "/"), nil
}

// pairingClient never follows a redirect: a publisher token must reach
// the address the Owner typed, or nowhere.
func (s *Server) pairingClient() *http.Client {
	c := http.Client{Timeout: 15 * time.Second}
	if s.PairingHTTP != nil {
		c = *s.PairingHTTP
	}
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &c
}

func (s *Server) pairPantry(r *http.Request, id principal.Identity) (any, int, error) {
	if s.SecretVault == nil || s.Principals == nil {
		return nil, http.StatusServiceUnavailable, errors.New("the secret store is not wired on this control plane")
	}
	var req pantryPairRequest
	if err := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 4096)).Decode(&req); err != nil {
		return nil, http.StatusBadRequest, fmt.Errorf("parsing request: %w", err)
	}
	address, err := pairingURL("address", req.Address)
	if err != nil {
		return nil, http.StatusBadRequest, err
	}
	keystoneURL, err := pairingURL("keystoneUrl", req.KeystoneURL)
	if err != nil {
		return nil, http.StatusBadRequest, err
	}
	code := strings.TrimSpace(req.Code)
	if code == "" || len(code) > 32 {
		return nil, http.StatusBadRequest, errors.New("code is the one-time code PantryOS shows (Wildstone → Pair)")
	}

	// One pairing at a time, end to end: a second one waits for the first
	// to settle, so its rotation cannot revoke the token the first one just
	// handed PantryOS (or the other way round).
	s.pantryPairingMu.Lock()
	defer s.pantryPairingMu.Unlock()

	name := publisherPrincipalPrefix + PantryCapability
	if err := s.ensurePublisherPrincipal(name, id); err != nil {
		return nil, http.StatusInternalServerError, err
	}
	token, err := s.Principals.Mint(name, "", 0)
	if err != nil {
		return nil, http.StatusInternalServerError, err
	}
	// Until PantryOS may hold the minted token, it is the only thing to
	// undo; the current pairing and its token stay as they are.
	fail := func(status int, err error) (any, int, error) {
		if rerr := s.Principals.Revoke(token); rerr != nil && s.Log != nil {
			s.Log.Error("pantry pairing: revoking the unused token", "err", rerr)
		}
		s.appendEvent("integration.pair-failed", "", PantryCapability, id.Name)
		return nil, status, err
	}
	// Once PantryOS may have stored it (it accepted the code, or the answer
	// was lost on the way back), it has replaced its old token with this
	// one: revoking it now would stop PantryOS's events while Keystone
	// still shows the old pairing. It stays valid; pairing again finishes
	// the job and revokes whatever is left over.
	failAfterHandoff := func(status int, err error) (any, int, error) {
		s.appendEvent("integration.pair-incomplete", "", PantryCapability, id.Name)
		return nil, status, fmt.Errorf("%w — PantryOS may already be using its new event token, which was kept working; pair again to finish", err)
	}

	agentKey, handedOver, err := s.exchangeWithPantry(r.Context(), address, code, keystoneURL, token)
	if err != nil {
		if handedOver {
			return failAfterHandoff(http.StatusBadGateway, err)
		}
		return fail(http.StatusBadGateway, err)
	}
	caps, err := (&pantry.HTTP{BaseURL: address, AgentKey: agentKey, HTTP: s.pairingClient()}).Capabilities(r.Context())
	if err != nil || !anyAvailable(caps) {
		detail := "it offered no pantry-1 capabilities"
		if err != nil {
			detail = err.Error()
		} else if len(caps) > 0 && caps[0].Detail != "" {
			detail = caps[0].Detail
		}
		return failAfterHandoff(http.StatusBadGateway, fmt.Errorf("PantryOS paired but its key does not work: %s", detail))
	}
	if err := s.SecretVault.SavePairing(PantryCapability, address, agentKey, id.Name); err != nil {
		return failAfterHandoff(http.StatusInternalServerError, err)
	}
	if _, err := s.Principals.RevokeOthers(name, token); err != nil {
		// The new pairing is saved and works, but a token it replaced may
		// still be live. That is not a success to report: say so, and let
		// pairing again (which rotates again) finish the job.
		s.appendEvent("integration.pair-incomplete", "", PantryCapability, id.Name)
		return nil, http.StatusInternalServerError, fmt.Errorf(
			"PantryOS is paired, but the publisher tokens it replaced could not be revoked (%v) — pair again to finish", err)
	}
	s.appendEvent("integration.paired", "", PantryCapability, id.Name)
	placed := s.placePantryPublisherSecret(r, token, id)
	info, err := s.DB.Pairing(PantryCapability)
	if err != nil {
		return nil, http.StatusInternalServerError, err
	}
	return map[string]any{"paired": true, "pairing": info, "publisherPlacement": placed}, http.StatusOK, nil
}

func anyAvailable(caps []adapter.Capability) bool {
	for _, c := range caps {
		if c.Available {
			return true
		}
	}
	return false
}

// exchangeWithPantry performs PantryOS's half of the handshake and returns
// the agent key it answers with.
// exchangeWithPantry hands PantryOS the publisher token and takes its agent
// key. handedOver reports whether PantryOS may now hold the token: true
// once the request went out, unless PantryOS clearly refused it.
func (s *Server) exchangeWithPantry(ctx context.Context, address, code, keystoneURL, token string) (agentKey string, handedOver bool, err error) {
	body, _ := json.Marshal(map[string]string{"code": code, "keystone_url": keystoneURL, "publisher_token": token})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, address+pantryExchangePath, bytes.NewReader(body))
	if err != nil {
		return "", false, err
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := s.pairingClient().Do(req)
	if err != nil {
		// The request may have arrived and only the answer been lost.
		return "", true, fmt.Errorf("PantryOS could not be reached at %s: %v", address, err)
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(res.Body, 64<<10))
	var detail struct {
		Detail string `json:"detail"`
	}
	_ = json.Unmarshal(raw, &detail)
	switch {
	case res.StatusCode == http.StatusUnauthorized:
		return "", false, errors.New("PantryOS refused the code: it is wrong, expired, or already used — ask PantryOS for a new one")
	case res.StatusCode == http.StatusTooManyRequests:
		return "", false, errors.New("PantryOS has seen too many wrong codes; wait a few minutes and try again")
	case res.StatusCode != http.StatusOK:
		msg := strings.TrimSpace(detail.Detail)
		if msg == "" {
			msg = res.Status
		}
		return "", false, fmt.Errorf("PantryOS answered %d: %s", res.StatusCode, msg)
	}
	var out struct {
		AgentKey string `json:"agent_key"`
		Contract string `json:"contract"`
	}
	if err := json.Unmarshal(raw, &out); err != nil || out.AgentKey == "" {
		return "", true, errors.New("PantryOS accepted the code but sent no agent key")
	}
	if out.Contract != pantry.Contract {
		return "", true, fmt.Errorf("PantryOS speaks contract %q; this Keystone speaks %q — update one of them", out.Contract, pantry.Contract)
	}
	return out.AgentKey, true, nil
}

// ensurePublisherPrincipal creates the publisher principal on first use,
// as mintPublisher does, and refuses a name held by another role.
func (s *Server) ensurePublisherPrincipal(name string, id principal.Identity) error {
	existing, err := s.DB.GetPrincipal(name)
	switch {
	case errors.Is(err, runtimedb.ErrNotFound):
		if err := s.Principals.Create(name, runtimedb.RolePublisher, ""); err != nil {
			return err
		}
		s.event("principal.created", "", id, name+" (publisher)")
		return nil
	case err != nil:
		return err
	case existing.Role != runtimedb.RolePublisher:
		return fmt.Errorf("principal %s exists with role %s, not publisher", name, existing.Role)
	}
	return nil
}

func (s *Server) pantryPairingStatus(_ *http.Request, _ principal.Identity) (any, int, error) {
	info, err := s.DB.Pairing(PantryCapability)
	if errors.Is(err, runtimedb.ErrNotPaired) {
		return map[string]any{"paired": false}, http.StatusOK, nil
	}
	if err != nil {
		return nil, http.StatusInternalServerError, err
	}
	return map[string]any{"paired": true, "pairing": info}, http.StatusOK, nil
}

// unpairPantry forgets the pairing and revokes PantryOS's publisher
// tokens. PantryOS itself is untouched; its agent key is PantryOS's to
// revoke, and is useless without the address Keystone just forgot.
func (s *Server) unpairPantry(_ *http.Request, id principal.Identity) (any, int, error) {
	s.pantryPairingMu.Lock()
	defer s.pantryPairingMu.Unlock()
	revoked, err := s.DB.Unpair(PantryCapability, publisherPrincipalPrefix+PantryCapability)
	if err != nil {
		return nil, http.StatusInternalServerError, err
	}
	s.appendEvent("integration.unpaired", "", PantryCapability, id.Name)
	return map[string]any{"paired": false, "publisherTokensRevoked": revoked}, http.StatusOK, nil
}

// placePantryPublisherSecret puts the pairing's publisher token where an
// installed pantry-pantryos package expects it: the provided secret
// `pantry-publisher` on the machine its install names — exactly what the
// generic publisher route would do, so the capability is not left waiting
// on a secret nobody places. PantryOS itself uses the copy pairing handed
// it; this one only satisfies the install.
//
// Not installed yet: nothing to place (pairing again after installing
// places it). A placement that fails does not undo the pairing — events
// and tools already work — but it is recorded and reported.
func (s *Server) placePantryPublisherSecret(r *http.Request, token string, id principal.Identity) string {
	secretName := capability.PublisherSecretName(PantryCapability)
	current, err := s.State.Load()
	if err != nil {
		return "not placed: " + err.Error()
	}
	node := ""
	for _, res := range current {
		if res.Kind == resource.KindSecret && res.Metadata.Name == secretName && res.Metadata.Owner == "capability/"+PantryCapability {
			node = res.Metadata.Node
			break
		}
	}
	if node == "" {
		return "not installed"
	}
	if s.Agents == nil {
		return "not placed: agent plane not wired"
	}
	result, err := s.Agents.Apply(r.Context(), node, &wsv1.Operation{Op: &wsv1.Operation_PutSecret{
		PutSecret: &wsv1.PutSecret{Name: secretName, Value: token},
	}})
	if err == nil && result.GetError() != nil {
		err = errors.New(result.GetError().GetMessage())
	}
	if err != nil {
		s.appendEvent("publisher.mint-failed", node, secretName, id.Name)
		return fmt.Sprintf("not placed on %s: %v — pair again to retry", node, err)
	}
	s.appendEvent("publisher.minted", node, secretName, id.Name)
	return "placed on " + node
}
