package api

// The OAuth MCP presentation from ADR-W049 §2.4. Authentication, closed-route
// classification, RBAC, rate limiting, and audit all remain in Server.handle;
// this file only validates Streamable HTTP framing and translates the two tool
// methods onto the canonical Agent Gateway.

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strings"

	"github.com/Wildstone-Systems/wildstone-homelab/keystone/agentapi"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/agentmcp"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/oauthresource"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/principal"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/runtimedb"
)

const (
	mcpProtocolHeader = "MCP-Protocol-Version"
	mcpSessionHeader  = "Mcp-Session-Id"
)

type oauthMCPRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type oauthMCPResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Result  any             `json:"result,omitempty"`
	Error   *oauthMCPError  `json:"error,omitempty"`
}

type oauthMCPError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type oauthMCPInitializeParams struct {
	ProtocolVersion string `json:"protocolVersion"`
}

type oauthMCPInitializeResult struct {
	ProtocolVersion string         `json:"protocolVersion"`
	Capabilities    map[string]any `json:"capabilities"`
	ServerInfo      struct {
		Name    string `json:"name"`
		Version string `json:"version"`
	} `json:"serverInfo"`
}

type oauthMCPToolsCallParams struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

func (s *Server) oauthMCP(w http.ResponseWriter, r *http.Request, id principal.Identity) (any, int, error) {
	auth, ok := oauthresource.FromContext(r.Context())
	if !ok {
		return nil, http.StatusForbidden, errors.New("MCP requires the OAuth profile")
	}
	if !s.validMCPOrigin(r.Header.Get("Origin")) {
		return nil, http.StatusForbidden, errors.New("MCP origin refused")
	}
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		return nil, http.StatusUnsupportedMediaType, errors.New("MCP requires application/json")
	}
	var req oauthMCPRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxAgentBodyBytes))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		return oauthMCPResponse{JSONRPC: "2.0", Error: &oauthMCPError{Code: -32700, Message: "parse error"}}, http.StatusOK, nil
	}
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		return oauthMCPResponse{JSONRPC: "2.0", Error: &oauthMCPError{Code: -32700, Message: "parse error"}}, http.StatusOK, nil
	}
	if req.JSONRPC != "2.0" || req.Method == "" {
		return oauthMCPResponse{JSONRPC: "2.0", ID: req.ID, Error: &oauthMCPError{Code: -32600, Message: "invalid request"}}, http.StatusOK, nil
	}
	if req.Method != "initialize" && r.Header.Get(mcpProtocolHeader) != agentmcp.ProtocolVersion {
		return nil, http.StatusBadRequest, errors.New("unsupported or missing MCP protocol version")
	}
	w.Header().Set(mcpProtocolHeader, agentmcp.ProtocolVersion)
	response := oauthMCPResponse{JSONRPC: "2.0", ID: req.ID}

	switch req.Method {
	case "initialize":
		var params oauthMCPInitializeParams
		if json.Unmarshal(req.Params, &params) != nil || params.ProtocolVersion != agentmcp.ProtocolVersion {
			response.Error = &oauthMCPError{Code: -32602, Message: "unsupported protocol version"}
			return response, http.StatusOK, nil
		}
		result := oauthMCPInitializeResult{ProtocolVersion: agentmcp.ProtocolVersion,
			Capabilities: map[string]any{"tools": map[string]any{}}}
		result.ServerInfo.Name = "wildstone-keystone"
		result.ServerInfo.Version = "1"
		response.Result = result
	case "ping":
		response.Result = map[string]any{}
	case "notifications/initialized":
		if len(req.ID) != 0 {
			response.Result = map[string]any{}
			return response, http.StatusOK, nil
		}
		w.WriteHeader(http.StatusAccepted)
		return nil, http.StatusAccepted, nil
	case "tools/list":
		ctx, current, code, err := s.oauthMCPContext(w, r, id, auth)
		if err != nil {
			return nil, code, err
		}
		if session := r.Header.Get(mcpSessionHeader); session != "" {
			admitted, err := s.SecretVault.ResolveExternalMCPSession(externalAdmissionIdentity(current), session, s.now())
			if err != nil {
				return nil, http.StatusForbidden, errors.New("MCP task session refused")
			}
			setMCPSessionResponse(w, session)
			manifest, err := s.AgentGateway.Manifest(ctx, agentapi.Handle(admitted.Handle))
			if err != nil {
				return agentErrorResponse(err)
			}
			response.Result = agentmcp.ToolsListResult{Tools: agentmcp.ToolsFor(manifest.Tools)}
			break
		}
		toolsClient, ok := s.AgentGateway.(agentapi.ToolsClient)
		if !ok {
			return nil, http.StatusNotImplemented, errors.New("MCP discovery is unavailable")
		}
		tools, err := toolsClient.Tools(ctx)
		if err != nil {
			return agentErrorResponse(err)
		}
		response.Result = agentmcp.ToolsListResult{Tools: agentmcp.ToolsFor(tools)}
	case "tools/call":
		session := r.Header.Get(mcpSessionHeader)
		if session == "" {
			return nil, http.StatusForbidden, errors.New("MCP task session required")
		}
		ctx, current, code, err := s.oauthMCPContext(w, r, id, auth)
		if err != nil {
			return nil, code, err
		}
		admitted, err := s.SecretVault.ResolveExternalMCPSession(externalAdmissionIdentity(current), session, s.now())
		if err != nil {
			return nil, http.StatusForbidden, errors.New("MCP task session refused")
		}
		var params oauthMCPToolsCallParams
		if json.Unmarshal(req.Params, &params) != nil || params.Name == "" {
			response.Error = &oauthMCPError{Code: -32602, Message: "invalid tool call"}
			return response, http.StatusOK, nil
		}
		result, err := s.AgentGateway.Invoke(ctx, agentapi.InvokeRequest{
			Handle: agentapi.Handle(admitted.Handle), Tool: params.Name, Args: params.Arguments,
		})
		if err != nil {
			return agentErrorResponse(err)
		}
		setMCPSessionResponse(w, session)
		response.Result = agentmcp.ToolResultFor(result)
	default:
		response.Error = &oauthMCPError{Code: -32601, Message: "method not found"}
	}
	return response, http.StatusOK, nil
}

func (s *Server) oauthMCPContext(w http.ResponseWriter, r *http.Request, id principal.Identity, auth *oauthresource.RequestAuth) (context.Context, oauthresource.Identity, int, error) {
	current, err := auth.Recheck(r.Context())
	if err != nil {
		w.Header().Set("WWW-Authenticate", s.oauthChallenge("invalid_token"))
		return nil, oauthresource.Identity{}, http.StatusUnauthorized, oauthresource.ErrUnauthenticated
	}
	ctx, code, err := s.agentContext(r, id)
	return ctx, current, code, err
}

func setMCPSessionResponse(w http.ResponseWriter, session string) {
	w.Header().Set(mcpSessionHeader, session)
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Pragma", "no-cache")
}

func externalAdmissionIdentity(id oauthresource.Identity) runtimedb.ExternalTaskAdmissionIdentity {
	return runtimedb.ExternalTaskAdmissionIdentity{RegistrationID: id.RegistrationID, PersonID: id.PersonID,
		Butler: id.AgentID, Connection: id.ConnectionID, RuntimePrincipal: id.RuntimePrincipal,
		CredentialEpoch: id.CredentialEpoch}
}

func (s *Server) validMCPOrigin(origin string) bool {
	if origin == "" {
		return true
	}
	if s.OAuthResource == nil {
		return false
	}
	resource, err := url.Parse(s.OAuthResource.Resource)
	if err != nil || resource.Scheme == "" || resource.Host == "" {
		return false
	}
	return strings.EqualFold(origin, resource.Scheme+"://"+resource.Host)
}
