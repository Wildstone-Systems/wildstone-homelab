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
	"sort"
	"strconv"
	"time"

	"github.com/Wildstone-Systems/wildstone-homelab/keystone/principal"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/runtimedb"
	wsv1 "github.com/Wildstone-Systems/wildstone-homelab/proto/gen/wardstone/v1"
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
	// Install is the ready-to-paste node install command (ADR-W014 A2),
	// with the gateway address, token and CA pin already filled in.
	Install string `json:"install"`
	// InstallNotes are the things the command itself cannot say: what
	// --node-id defaults to, and what to substitute if Keystone does not
	// know its own reachable address.
	InstallNotes []string `json:"installNotes,omitempty"`
}

// releaseRepo is where install.sh and the release assets live.
const releaseRepo = "Wildstone-Systems/wildstone-homelab"

// addrPlaceholder stands in when Keystone has no advertised gateway
// address to offer (-hosts left at its loopback default).
const addrPlaceholder = "<host:9440>"

const (
	defaultJoinTTL = 15 * time.Minute
	maxJoinTTL     = 24 * time.Hour
)

// LiveNode is one currently-connected agent session, as reported by the
// gateway. Declared here rather than imported so the public plane keeps no
// compile-time dependency on the agent plane; cmd/keystone adapts.
type LiveNode struct {
	NodeID        string
	AgentVersion  string
	ProtocolMinor uint32
	Since         time.Time
}

// nodeView is one row of the inventory: the durable record of a node that
// joined, joined with whether it is connected right now.
type nodeView struct {
	ID             string     `json:"id"`
	Connected      bool       `json:"connected"`
	AgentVersion   string     `json:"agentVersion,omitempty"`
	ProtocolMinor  uint32     `json:"protocolMinor,omitempty"`
	FirstJoinedAt  time.Time  `json:"firstJoinedAt"`
	LastSeenAt     time.Time  `json:"lastSeenAt"`
	ConnectedSince *time.Time `json:"connectedSince,omitempty"`
}

// listNodes answers "what nodes do I have, and are they connected?" (#30)
// — the question no surface in the product could answer before, which made
// every other node route require a name the caller already knew.
//
// The durable rows come from the runtime DB; liveness comes from the
// gateway's session registry at read time, never from stored state. With
// no gateway wired every node reports disconnected rather than the request
// failing: the inventory is still the useful half of the answer.
func (s *Server) listNodes(r *http.Request, id principal.Identity) (any, int, error) {
	recorded, err := s.DB.ListNodes()
	if err != nil {
		return nil, http.StatusInternalServerError, err
	}
	live := map[string]LiveNode{}
	if s.LiveNodes != nil {
		for _, n := range s.LiveNodes() {
			live[n.NodeID] = n
		}
	}
	out := make([]nodeView, 0, len(recorded))
	seen := map[string]bool{}
	for _, n := range recorded {
		seen[n.NodeID] = true
		v := nodeView{
			ID:            n.NodeID,
			AgentVersion:  n.AgentVersion,
			ProtocolMinor: n.ProtocolMinor,
			FirstJoinedAt: n.FirstJoinedAt,
			LastSeenAt:    n.LastSeenAt,
		}
		if l, ok := live[n.NodeID]; ok {
			v.Connected = true
			v.AgentVersion = l.AgentVersion
			v.ProtocolMinor = l.ProtocolMinor
			since := l.Since
			v.ConnectedSince = &since
		}
		out = append(out, v)
	}
	// A session with no row (the hook was unwired, or the DB was restored
	// from an older backup) is still a node you have — showing it beats
	// pretending the fleet is smaller than it is.
	for _, l := range live {
		if seen[l.NodeID] {
			continue
		}
		since := l.Since
		out = append(out, nodeView{
			ID: l.NodeID, Connected: true, AgentVersion: l.AgentVersion,
			ProtocolMinor: l.ProtocolMinor, ConnectedSince: &since,
			FirstJoinedAt: l.Since, LastSeenAt: l.Since,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return map[string]any{"nodes": out}, http.StatusOK, nil
}

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

	install, notes := s.installCommand(token)
	return joinTokenResponse{
		JoinToken:     token,
		CAFingerprint: s.CAFingerprint,
		ExpiresInSec:  int64(ttl.Seconds()),
		Install:       install,
		InstallNotes:  notes,
	}, http.StatusCreated, nil
}

// installCommand composes the pairing instructions this endpoint hands
// back. The point of minting a token from Keystone is that Keystone knows
// everything the node install needs — the address, the token, the CA
// fingerprint — so anything left for the operator to fill in is a step
// where pairing can fail. The release repository is public, so one
// anonymous one-liner is enough.
func (s *Server) installCommand(token string) (install string, notes []string) {
	addr := s.GatewayAddr
	if addr == "" {
		addr = addrPlaceholder
		notes = append(notes, "Keystone has no advertised gateway address (-hosts is at its loopback default); replace "+
			addrPlaceholder+" with the address the new node can reach this host on.")
	}
	args := fmt.Sprintf("--role node --keystone %s --join-token %s", addr, token)
	if s.CAFingerprint != "" {
		args += " --ca-fingerprint " + s.CAFingerprint
	}
	install = fmt.Sprintf(
		"curl -fsSL https://github.com/%s/releases/latest/download/install.sh | sudo bash -s -- %s",
		releaseRepo, args)
	notes = append(notes,
		"--node-id defaults to the new host's short hostname; pass --node-id <id> to choose it.",
		"The join token is single-use and expires; mint a fresh one for each node.")
	return install, notes
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
