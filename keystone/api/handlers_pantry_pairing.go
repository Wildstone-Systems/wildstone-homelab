package api

// Pairing with PantryOS (ADR-W042 A6, #384).
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

	"github.com/chickenrat17/wildstone-homelab/keystone/adapter"
	"github.com/chickenrat17/wildstone-homelab/keystone/adapter/pantry"
	"github.com/chickenrat17/wildstone-homelab/keystone/principal"
	"github.com/chickenrat17/wildstone-homelab/keystone/runtimedb"
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

	name := publisherPrincipalPrefix + PantryCapability
	if err := s.ensurePublisherPrincipal(name, id); err != nil {
		return nil, http.StatusInternalServerError, err
	}
	token, err := s.Principals.Mint(name, "", 0)
	if err != nil {
		return nil, http.StatusInternalServerError, err
	}
	// Until the new pairing is proven, the minted token is the only thing
	// to undo; the current pairing and its token stay as they are.
	fail := func(status int, err error) (any, int, error) {
		if rerr := s.Principals.Revoke(token); rerr != nil && s.Log != nil {
			s.Log.Error("pantry pairing: revoking the unused token", "err", rerr)
		}
		s.appendEvent("integration.pair-failed", "", PantryCapability, id.Name)
		return nil, status, err
	}

	agentKey, err := s.exchangeWithPantry(r.Context(), address, code, keystoneURL, token)
	if err != nil {
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
		return fail(http.StatusBadGateway, fmt.Errorf("PantryOS paired but its key does not work: %s", detail))
	}
	if err := s.SecretVault.SavePairing(PantryCapability, address, agentKey, id.Name); err != nil {
		return fail(http.StatusInternalServerError, err)
	}
	if _, err := s.Principals.RevokeOthers(name, token); err != nil && s.Log != nil {
		s.Log.Error("pantry pairing: revoking the replaced publisher tokens", "err", err)
	}
	s.appendEvent("integration.paired", "", PantryCapability, id.Name)
	info, err := s.DB.Pairing(PantryCapability)
	if err != nil {
		return nil, http.StatusInternalServerError, err
	}
	return map[string]any{"paired": true, "pairing": info}, http.StatusOK, nil
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
func (s *Server) exchangeWithPantry(ctx context.Context, address, code, keystoneURL, token string) (string, error) {
	body, _ := json.Marshal(map[string]string{"code": code, "keystone_url": keystoneURL, "publisher_token": token})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, address+pantryExchangePath, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := s.pairingClient().Do(req)
	if err != nil {
		return "", fmt.Errorf("PantryOS could not be reached at %s: %v", address, err)
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(res.Body, 64<<10))
	var detail struct {
		Detail string `json:"detail"`
	}
	_ = json.Unmarshal(raw, &detail)
	switch {
	case res.StatusCode == http.StatusUnauthorized:
		return "", errors.New("PantryOS refused the code: it is wrong, expired, or already used — ask PantryOS for a new one")
	case res.StatusCode == http.StatusTooManyRequests:
		return "", errors.New("PantryOS has seen too many wrong codes; wait a few minutes and try again")
	case res.StatusCode != http.StatusOK:
		msg := strings.TrimSpace(detail.Detail)
		if msg == "" {
			msg = res.Status
		}
		return "", fmt.Errorf("PantryOS answered %d: %s", res.StatusCode, msg)
	}
	var out struct {
		AgentKey string `json:"agent_key"`
		Contract string `json:"contract"`
	}
	if err := json.Unmarshal(raw, &out); err != nil || out.AgentKey == "" {
		return "", errors.New("PantryOS accepted the code but sent no agent key")
	}
	if out.Contract != pantry.Contract {
		return "", fmt.Errorf("PantryOS speaks contract %q; this Keystone speaks %q — update one of them", out.Contract, pantry.Contract)
	}
	return out.AgentKey, nil
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
	if err := s.DB.DeletePairing(PantryCapability); err != nil {
		return nil, http.StatusInternalServerError, err
	}
	revoked, err := s.DB.RevokeTokensFor(publisherPrincipalPrefix + PantryCapability)
	if err != nil {
		return nil, http.StatusInternalServerError, err
	}
	s.appendEvent("integration.unpaired", "", PantryCapability, id.Name)
	return map[string]any{"paired": false, "publisherTokensRevoked": revoked}, http.StatusOK, nil
}
