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
	"strconv"
	"time"

	"github.com/chickenrat17/wildstone-homelab/keystone/principal"
	"github.com/chickenrat17/wildstone-homelab/keystone/runtimedb"
	wsv1 "github.com/chickenrat17/wildstone-homelab/proto/gen/wardstone/v1"
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

// containerLogs (ADR-W015 A2): GET /api/v1/nodes/{node}/containers/
// {name}/logs?tail=N — dispatches ContainerLogs and demuxes docker's
// framed stream into text. The text rides the ordinary redacting
// encoder, so a secret echoed into a log comes back redacted.
func (s *Server) containerLogs(r *http.Request, _ principal.Identity) (any, int, error) {
	if s.Agents == nil {
		return nil, http.StatusServiceUnavailable, fmt.Errorf("agent plane not wired")
	}
	node, name := r.PathValue("node"), r.PathValue("name")
	if node == "" || name == "" {
		return nil, http.StatusBadRequest, fmt.Errorf("node and container name required")
	}
	var tail uint64
	if t := r.URL.Query().Get("tail"); t != "" {
		var err error
		if tail, err = strconv.ParseUint(t, 10, 32); err != nil {
			return nil, http.StatusBadRequest, fmt.Errorf("tail must be a number")
		}
	}
	res, err := s.Agents.Apply(r.Context(), node, &wsv1.Operation{Op: &wsv1.Operation_ContainerLogs{
		ContainerLogs: &wsv1.ContainerLogs{Name: name, Tail: uint32(tail)},
	}})
	if err != nil {
		return nil, http.StatusBadGateway, fmt.Errorf("node %s: %w", node, err)
	}
	if oe := res.GetError(); oe != nil {
		return nil, opStatus(oe), fmt.Errorf("node %s: %s", node, oe.GetMessage())
	}
	return map[string]string{
		"node": node, "name": name,
		"logs": demuxDockerStream(res.GetContainerLogs().GetLogs()),
	}, http.StatusOK, nil
}

// demuxDockerStream strips docker's multiplexed-stream framing (8-byte
// headers: stream type, 3 zero bytes, big-endian length) and returns the
// concatenated payload text. Input without valid framing (a TTY
// container's raw stream) is returned as-is.
func demuxDockerStream(raw []byte) string {
	var out []byte
	rest := raw
	for len(rest) >= 8 {
		typ := rest[0]
		if typ > 2 || rest[1] != 0 || rest[2] != 0 || rest[3] != 0 {
			return string(raw) // not framed
		}
		n := int(rest[4])<<24 | int(rest[5])<<16 | int(rest[6])<<8 | int(rest[7])
		if n < 0 || n > len(rest)-8 {
			return string(raw)
		}
		out = append(out, rest[8:8+n]...)
		rest = rest[8+n:]
	}
	if len(rest) != 0 {
		return string(raw)
	}
	return string(out)
}
