package api

import (
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/chickenrat17/wildstone-homelab/keystone/principal"
	"github.com/chickenrat17/wildstone-homelab/keystone/runtimedb"
	wsv1 "github.com/chickenrat17/wildstone-homelab/proto/gen/wardstone/v1"
)

// importNodeRecovery passes the offline private key and sealed bundle to a
// fresh Wardstone. Keystone never opens the bundle and the protocol result
// contains only a count; there remains no value-reveal route.
func (s *Server) importNodeRecovery(r *http.Request, id principal.Identity) (any, int, error) {
	if s.Agents == nil {
		return nil, http.StatusServiceUnavailable, fmt.Errorf("agent plane not wired")
	}
	node := r.PathValue("node")
	if node == "" {
		return nil, http.StatusBadRequest, fmt.Errorf("node required")
	}
	var body struct {
		PrivateKey   string `json:"privateKey"`
		SealedBundle []byte `json:"sealedBundle"`
	}
	// The Wardstone envelope is capped at roughly 17 MiB. JSON base64 adds
	// one third; keep malformed recovery requests from becoming unbounded.
	if err := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 24<<20)).Decode(&body); err != nil || body.PrivateKey == "" || len(body.SealedBundle) == 0 {
		return nil, http.StatusBadRequest, fmt.Errorf("privateKey and sealedBundle are required")
	}
	res, err := s.Agents.Apply(r.Context(), node, &wsv1.Operation{Op: &wsv1.Operation_ImportSecretBundle{
		ImportSecretBundle: &wsv1.ImportSecretBundle{RecoveryPrivateKey: body.PrivateKey, SealedBundle: body.SealedBundle},
	}})
	body.PrivateKey = ""
	if err != nil {
		return nil, http.StatusBadGateway, fmt.Errorf("node %s: %w", node, err)
	}
	if oerr := res.GetError(); oerr != nil {
		return nil, opStatus(oerr), fmt.Errorf("node %s: %s", node, oerr.GetMessage())
	}
	count := res.GetImportSecretBundle().GetSecretCount()
	s.appendEvent(runtimedb.RecoveryNodeSecretsImportedEvent, node, "recovery", id.Name)
	return map[string]any{"node": node, "secretCount": count}, http.StatusOK, nil
}
