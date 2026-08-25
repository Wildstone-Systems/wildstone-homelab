package api

// Node pairing (ADR-W014 A3): the admin mints a single-use join token
// through the gateway and gets it back together with the CA fingerprint
// and a ready-to-run install command — the scope doc's "pairing code" is
// this one response. The token is the second (and last) redaction
// carve-out; see writeJSON.

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/chickenrat17/wildstone-homelab/keystone/principal"
	"github.com/chickenrat17/wildstone-homelab/keystone/runtimedb"
)

// joinTokenResponse is the pairing reply. Like mintResponse it skips the
// redacting encoder — the join token is 64 hex chars, exactly the shape
// redaction exists to eat, and this is the only moment it appears in a
// response (the gateway stores only its hash-equivalent single-use
// record).
type joinTokenResponse struct {
	JoinToken     string `json:"joinToken"`
	CAFingerprint string `json:"caFingerprint,omitempty"`
	ExpiresInSec  int64  `json:"expiresInSec"`
	// Install is the ready-to-paste node install command (ADR-W014 A2);
	// the operator fills in --keystone with the gateway address reachable
	// from the new node.
	Install string `json:"install"`
}

const (
	defaultJoinTTL = 15 * time.Minute
	maxJoinTTL     = 24 * time.Hour
)

func (s *Server) mintJoinToken(r *http.Request, id principal.Identity) (any, int, error) {
	if s.MintJoin == nil {
		return nil, http.StatusServiceUnavailable, fmt.Errorf("gateway not wired")
	}
	var body struct {
		TTLSeconds int64 `json:"ttlSeconds"`
	}
	if r.Body != nil {
		if derr := json.NewDecoder(r.Body).Decode(&body); derr != nil && !errors.Is(derr, io.EOF) {
			return nil, http.StatusBadRequest, fmt.Errorf("body: %v", derr)
		}
	}
	ttl := defaultJoinTTL
	if body.TTLSeconds > 0 {
		ttl = time.Duration(body.TTLSeconds) * time.Second
	}
	if ttl > maxJoinTTL {
		return nil, http.StatusBadRequest, fmt.Errorf("ttlSeconds exceeds the 24h cap")
	}

	token, err := s.MintJoin(ttl)
	if err != nil {
		return nil, http.StatusInternalServerError, fmt.Errorf("minting join token: %w", err)
	}
	if _, err := s.DB.AppendEvent(runtimedb.Event{
		Type: "node.join-token-minted", Message: fmt.Sprintf("ttl %s", ttl),
		Attrs: map[string]string{"by": id.Name},
	}); err != nil {
		s.Log.Error("join-token event append failed", "err", err)
	}

	install := fmt.Sprintf(
		"curl -fsSL https://github.com/chickenrat17/wildstone-homelab/releases/latest/download/install.sh | sudo bash -s -- --role node --keystone <host:9440> --join-token %s",
		token)
	if s.CAFingerprint != "" {
		install += " --ca-fingerprint " + s.CAFingerprint
	}
	return joinTokenResponse{
		JoinToken:     token,
		CAFingerprint: s.CAFingerprint,
		ExpiresInSec:  int64(ttl.Seconds()),
		Install:       install,
	}, http.StatusCreated, nil
}
