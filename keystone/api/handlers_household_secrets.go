package api

import (
	"encoding/json"
	"errors"
	"github.com/chickenrat17/wildstone-homelab/keystone/principal"
	"github.com/chickenrat17/wildstone-homelab/keystone/resource"
	"github.com/chickenrat17/wildstone-homelab/keystone/runtimedb"
	"io"
	"net/http"
)

// secretRevealResponse is created only after a trusted-surface authorization
// check. It is never used for tool output or ordinary chat serialization.
type secretRevealResponse struct {
	Value string `json:"value"`
}
type secretRequest struct {
	runtimedb.SecretInput
	Agent   string                `json:"agent"`
	Purpose string                `json:"purpose"`
	Grant   runtimedb.SecretGrant `json:"grant"`
	Allow   bool                  `json:"allow"`
	Action  string                `json:"action"`
}

func secretDecode(w http.ResponseWriter, r *http.Request) (secretRequest, error) {
	var req secretRequest
	d := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16384))
	d.DisallowUnknownFields()
	if err := d.Decode(&req); err != nil {
		return req, errors.New("invalid secret request")
	}
	if d.Decode(new(any)) != io.EOF {
		return req, errors.New("invalid secret request")
	}
	return req, nil
}
func (s *Server) householdSecretConsole(r *http.Request, id principal.Identity) (any, int, error) {
	// Raw reveal never accepts API/service tokens as a trusted UI assertion.
	if id.AuthTime == nil || s.SecretVault == nil {
		return nil, http.StatusForbidden, runtimedb.ErrSecretDenied
	}
	return s.secretOperation(nil, r, actingMember(id), true)
}
func (s *Server) householdSecretHome(w http.ResponseWriter, r *http.Request, member string, _ []resource.Resource) (any, int, error) {
	return s.secretOperation(w, r, member, false)
}
func (s *Server) secretOperation(w http.ResponseWriter, r *http.Request, actor string, console bool) (any, int, error) {
	if s.SecretVault == nil {
		return nil, http.StatusServiceUnavailable, errors.New("secret storage unavailable")
	}
	// Listing does not decrypt or return any secret value.
	if r.Method == http.MethodGet {
		out, err := s.SecretVault.List(actor, console)
		if err != nil {
			return nil, http.StatusForbidden, runtimedb.ErrSecretDenied
		}
		resp := map[string]any{"secrets": out}
		if !console && s.DB != nil {
			// The Agents this person may name in a grant or a Household
			// reveal, so the page offers a choice rather than a text box
			// a mistyped name silently fails in. ButlersFor is the same
			// rule AgentGrant enforces — enabled, not retired, and granted
			// to this person — so the list cannot offer one it refuses.
			// Its class rides along because a reveal on this page is a
			// Member Agent's (ADR-W033 D4).
			agents := []map[string]string{}
			if bs, err := s.DB.ButlersFor(actor); err == nil {
				for _, b := range bs {
					agents = append(agents, map[string]string{"name": b.Name, "agentClass": b.AgentClass})
				}
			}
			resp["agents"] = agents
		}
		return resp, 200, nil
	}
	req, err := secretDecode(w, r)
	if err != nil {
		return nil, 400, err
	}
	id := r.PathValue("id")
	op := r.PathValue("operation")
	var result any = map[string]bool{"ok": true}
	switch op {
	case "":
		if !console && req.Scope == "household" && !req.Submit {
			return nil, 403, runtimedb.ErrSecretDenied
		}
		result, err = s.SecretVault.Create(actor, req.SecretInput)
		if err == nil {
			obj := result.(runtimedb.SecretObject)
			if obj.State == "active" {
				s.rememberSecretReference(actor, obj)
			}
		}

	case "submit":
		result, err = s.SecretVault.SubmitPersonalCopy(actor, id)
	case "agent":
		if !console {
			owned, e := s.SecretVault.List(actor, false)
			if e != nil {
				return nil, 403, runtimedb.ErrSecretDenied
			}
			personal := false
			for _, o := range owned {
				if o.ID == id && o.Scope == "personal" {
					personal = true
				}
			}
			if !personal {
				return nil, 403, runtimedb.ErrSecretDenied
			}
		}
		err = s.SecretVault.AgentGrant(actor, id, req.Agent, req.Purpose, req.Allow)
	case "grant":
		if !console {
			return nil, 403, runtimedb.ErrSecretDenied
		}
		err = s.SecretVault.Grant(actor, id, req.Grant, req.Allow)
	case "rotate":
		if !console {
			return nil, 403, runtimedb.ErrSecretDenied
		}
		err = s.SecretVault.Rotate(actor, id, req.Value)
	case "lifecycle":
		if !console {
			return nil, 403, runtimedb.ErrSecretDenied
		}
		err = s.SecretVault.Lifecycle(actor, id, req.Action)
		if err == nil && req.Action == "approve" {
			objects, e := s.SecretVault.List(actor, true)
			if e == nil {
				for _, obj := range objects {
					if obj.ID == id {
						s.rememberSecretReference(actor, obj)
					}
				}
			}
		}

	case "calendar":
		if !console {
			owned, e := s.SecretVault.List(actor, false)
			if e != nil {
				return nil, 403, runtimedb.ErrSecretDenied
			}
			personal := false
			for _, o := range owned {
				if o.ID == id && o.Scope == "personal" {
					personal = true
				}
			}
			if !personal {
				return nil, 403, runtimedb.ErrSecretDenied
			}
		}
		err = s.SecretVault.BindCalendar(actor, id)
	case "reveal", "review":
		var raw []byte
		if op == "review" {
			if !console {
				return nil, 403, runtimedb.ErrSecretDenied
			}
			raw, err = s.SecretVault.Review(actor, id)
		} else {
			surface := "homepage"
			if console {
				surface = "console"
			}
			read := s.DB.BindMemoryRead(actor, req.Agent, runtimedb.MemoryDestination{BuiltIn: true, ConsoleOwner: console})
			raw, err = s.SecretVault.Reveal(id, read, surface)
		}
		if err == nil {
			result = secretRevealResponse{Value: string(raw)}
		}
		clear(raw)
	default:
		return nil, 404, errors.New("unknown secret operation")
	}
	if err != nil {
		return nil, 403, runtimedb.ErrSecretDenied
	}
	return result, 200, nil
}
