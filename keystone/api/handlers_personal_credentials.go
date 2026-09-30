package api

// Personal mailbox credentials and unattended-use authorization (epic
// #326 S02; ADR-W030 A5.1/A5.3, ADR-W033 D1 amendment).
//
// This is the protected secret-intake path. A Person's app password
// arrives in exactly one request body, is forwarded once over the mTLS
// agent plane to each of the connection's placement nodes (PutSecret
// into the write-only store), and is then dropped. It is not written to
// the runtime database, a log, an event, a plan, a response or an
// error: the audit chain records method and path only, every error
// here is composed from names, and there is no field in any response
// that could carry it. Chat, package params and task prompts have no
// path into this handler at all.
//
// The same handlers serve the Console (and CLI, via a token acting for
// the Person) and the Homepage (members, through the portal door), so
// the Console, CLI and a future Butler control share one API.
//
// Who may do what is decided in runtimedb, not here, and it is narrow:
// onboarding, replacing, inspecting and authorizing are for the Person
// who owns the connection alone. An Owner administering the household
// may REVOKE and ERASE — withdrawing access needs nobody's consent — but
// can never inspect, replace or authorize someone else's credential.

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"unicode"

	"github.com/chickenrat17/wildstone-homelab/keystone/capability"
	"github.com/chickenrat17/wildstone-homelab/keystone/principal"
	"github.com/chickenrat17/wildstone-homelab/keystone/resource"
	"github.com/chickenrat17/wildstone-homelab/keystone/runtimedb"
	wsv1 "github.com/chickenrat17/wildstone-homelab/proto/gen/wardstone/v1"
	"github.com/chickenrat17/wildstone-homelab/proto/secreterase"
)

// EraseSecretProtocolMinor is the Wardstone protocol minor that answers
// EraseSecret with evidence. An older agent cannot prove an erase, so a
// copy on it stays erasure_unverified rather than being assumed gone.
const EraseSecretProtocolMinor = 22

// maxCredentialBody bounds the intake body: a login, an app password and
// a need name, with room to spare and none for a document.
const maxCredentialBody = 8 << 10

// credentialIntake is the one request shape that carries a value.
type credentialIntake struct {
	Login    string `json:"login"`
	Password string `json:"password"`
	Need     string `json:"need"`
}

// decodeCredentialIntake reads the intake body. Its errors never quote
// the input: a JSON error message can echo what it choked on.
func decodeCredentialIntake(r *http.Request) (credentialIntake, error) {
	var in credentialIntake
	dec := json.NewDecoder(io.LimitReader(r.Body, maxCredentialBody+1))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil {
		return credentialIntake{}, errors.New("the credential request could not be read; send login and password only")
	}
	if dec.More() {
		return credentialIntake{}, errors.New("the credential request could not be read; send one object")
	}
	if in.Password == "" || len(in.Password) > 1024 || strings.IndexFunc(in.Password, unicode.IsControl) >= 0 {
		return credentialIntake{}, errors.New("an app password is one line of at most 1024 characters")
	}
	return in, nil
}

// credentialStatus maps the store's refusals to HTTP. Anybody who is not
// the owning Person gets the same 403 whether or not the connection has
// a credential, so a refusal discloses nothing about another Person's
// mailbox.
func credentialStatus(err error) int {
	switch {
	case errors.Is(err, runtimedb.ErrNotCredentialOwner):
		return http.StatusForbidden
	case errors.Is(err, runtimedb.ErrNotFound):
		return http.StatusNotFound
	case errors.Is(err, runtimedb.ErrNoActiveCredential), errors.Is(err, runtimedb.ErrDigestMismatch):
		return http.StatusConflict
	default:
		return http.StatusBadRequest
	}
}

// ---------------------------------------------------------------- routes

func (s *Server) consoleCredential(r *http.Request, id principal.Identity) (any, int, error) {
	return s.credentialView(r.PathValue("name"), actingMember(id))
}

func (s *Server) homeCredential(_ http.ResponseWriter, r *http.Request, member string, _ []resource.Resource) (any, int, error) {
	return s.credentialView(r.PathValue("name"), member)
}

func (s *Server) consoleSubmitCredential(r *http.Request, id principal.Identity) (any, int, error) {
	return s.submitCredential(r, r.PathValue("name"), actingMember(id))
}

func (s *Server) homeSubmitCredential(_ http.ResponseWriter, r *http.Request, member string, _ []resource.Resource) (any, int, error) {
	return s.submitCredential(r, r.PathValue("name"), member)
}

func (s *Server) consoleRevokeCredential(r *http.Request, id principal.Identity) (any, int, error) {
	// An Owner on the Console may withdraw anybody's credential; that is
	// administration, not ownership, and it only removes access.
	return s.revokeCredential(r, r.PathValue("name"), actingMember(id), true)
}

func (s *Server) homeRevokeCredential(_ http.ResponseWriter, r *http.Request, member string, _ []resource.Resource) (any, int, error) {
	return s.revokeCredential(r, r.PathValue("name"), member, false)
}

func (s *Server) consoleEraseCredential(r *http.Request, id principal.Identity) (any, int, error) {
	name := r.PathValue("name")
	if _, err := s.DB.GetConnection(name); err != nil {
		return nil, http.StatusNotFound, fmt.Errorf("no connection named %q", name)
	}
	outcomes := s.eraseCredentialCopies(r.Context(), name, true)
	conn, _ := s.DB.GetConnection(name)
	return map[string]any{"erasures": outcomes, "state": conn.State, "degradedReason": conn.DegradedReason}, http.StatusOK, nil
}

func (s *Server) consoleAuthorizeBackground(r *http.Request, id principal.Identity) (any, int, error) {
	return s.authorizeBackground(r, r.PathValue("name"), actingMember(id))
}

func (s *Server) homeAuthorizeBackground(_ http.ResponseWriter, r *http.Request, member string, _ []resource.Resource) (any, int, error) {
	return s.authorizeBackground(r, r.PathValue("name"), member)
}

func (s *Server) consoleRevokeBackground(r *http.Request, id principal.Identity) (any, int, error) {
	return s.revokeBackground(r, r.PathValue("name"), actingMember(id), true)
}

func (s *Server) homeRevokeBackground(_ http.ResponseWriter, r *http.Request, member string, _ []resource.Resource) (any, int, error) {
	return s.revokeBackground(r, r.PathValue("name"), member, false)
}

// setIntegrationExecutable records the adapter executable a connection's
// placements run. The placement renderer calls it; a change revokes the
// live background authorization (a material edit).
func (s *Server) setIntegrationExecutable(r *http.Request, id principal.Identity) (any, int, error) {
	name := r.PathValue("name")
	var req struct {
		Executable string `json:"executable"`
	}
	if code, err := decodeStrict(r, &req); err != nil {
		return nil, code, err
	}
	if err := s.DB.SetConnectionExecutable(name, req.Executable, actingMember(id)); err != nil {
		return nil, credentialStatus(err), err
	}
	return map[string]any{"executable": req.Executable}, http.StatusOK, nil
}

// ---------------------------------------------------------------- behaviour

// ownerOf returns the connection, refusing anybody but its owning
// Person with the same 403 the store gives.
func (s *Server) ownerOf(name, actor string) (runtimedb.IntegrationConnection, int, error) {
	conn, err := s.DB.GetConnection(name)
	if err != nil || conn.Ownership != runtimedb.OwnershipPersonal || conn.Owner != actor {
		return runtimedb.IntegrationConnection{}, http.StatusForbidden, runtimedb.ErrNotCredentialOwner
	}
	return conn, 0, nil
}

// credentialView is the owning Person's view of their credential: every
// version, where each went and whether each copy's erasure is proven,
// and the authorizations they have given. Never the value.
func (s *Server) credentialView(name, actor string) (any, int, error) {
	conn, code, err := s.ownerOf(name, actor)
	if err != nil {
		return nil, code, err
	}
	creds, err := s.DB.ListPersonalCredentials(name)
	if err != nil {
		return nil, http.StatusInternalServerError, err
	}
	auths, err := s.DB.ListBackgroundAuthorizations(name)
	if err != nil {
		return nil, http.StatusInternalServerError, err
	}
	if creds == nil {
		creds = []runtimedb.PersonalCredential{}
	}
	if auths == nil {
		auths = []runtimedb.BackgroundAuthorization{}
	}
	return map[string]any{
		"connection":     conn.Name,
		"state":          conn.State,
		"degradedReason": conn.DegradedReason,
		"executable":     conn.Executable,
		"credentials":    creds,
		"authorizations": auths,
	}, http.StatusOK, nil
}

// submitCredential is the intake: onboarding when there is no credential
// yet, replacement when there is. Either way the new version is staged
// on EVERY placement node before anything is fenced, and only then does
// it become active and the old one get fenced and erased.
func (s *Server) submitCredential(r *http.Request, name, actor string) (any, int, error) {
	// Refuse before reading the value: a caller who may not onboard this
	// credential never gets as far as having it parsed.
	if _, code, err := s.ownerOf(name, actor); err != nil {
		return nil, code, err
	}
	if s.Agents == nil {
		return nil, http.StatusServiceUnavailable, fmt.Errorf("the node agent plane is not wired, so no credential can be stored")
	}
	in, err := decodeCredentialIntake(r)
	if err != nil {
		return nil, http.StatusBadRequest, err
	}
	placements, err := s.DB.ListPlacements(name)
	if err != nil {
		return nil, http.StatusInternalServerError, err
	}
	var nodes []string
	for _, p := range placements {
		if !p.Removing {
			nodes = append(nodes, p.Node)
		}
	}
	staged, err := s.DB.StagePersonalCredential(name, in.Need, in.Login, actor, nodes)
	if err != nil {
		in.Password = ""
		return nil, credentialStatus(err), err
	}

	// Fan the value out. It exists in this handler's memory and in the
	// PutSecret ops, and nowhere else Keystone keeps.
	var failed []string
	for _, node := range nodes {
		res, aerr := s.Agents.Apply(r.Context(), node, &wsv1.Operation{Op: &wsv1.Operation_PutSecret{
			PutSecret: &wsv1.PutSecret{Name: staged.SecretRef, Value: in.Password},
		}})
		switch {
		case aerr != nil:
			// Unknown whether it landed: the copy stays "sending" and is
			// erased with the rest if staging fails.
			failed = append(failed, node+" (unreachable)")
		case res.GetError() != nil:
			_ = s.DB.MarkCredentialCopy(staged.Seq, node, false, 0)
			failed = append(failed, fmt.Sprintf("%s (%s)", node, res.GetError().GetCode()))
		default:
			if err := s.DB.MarkCredentialCopy(staged.Seq, node, true, res.GetPutSecret().GetVersion()); err != nil {
				failed = append(failed, node+" (not recorded)")
			}
		}
	}
	in.Password = ""

	if len(failed) > 0 {
		if err := s.DB.FailStagedCredential(staged.Seq, actor, "not stored on "+strings.Join(failed, ", ")); err != nil {
			return nil, http.StatusInternalServerError, err
		}
		outcomes := s.eraseCredentialCopies(r.Context(), name, false)
		return map[string]any{"erasures": outcomes}, http.StatusBadGateway, fmt.Errorf(
			"the credential could not be stored on every placement node (%s); nothing changed, and what did "+
				"arrive is being erased. Your previous credential, if any, still works", strings.Join(failed, ", "))
	}
	active, superseded, err := s.DB.ActivatePersonalCredential(staged.Seq, actor)
	if err != nil {
		return nil, http.StatusInternalServerError, err
	}
	s.appendCredentialEvent("integration.credential-stored", name, actor)
	out := map[string]any{
		"credential": active,
		"note": "stored on the connection's placement nodes and nowhere else. The adapter reads it on " +
			"its node; Keystone does not keep it and cannot show it again",
	}
	if len(superseded) > 0 {
		// The old version is fenced already; now prove it gone, and take
		// down the adapter containers that were started with it.
		out["superseded"] = superseded
		out["erasures"] = s.eraseCredentialCopies(r.Context(), name, false)
		out["note"] = "replaced. The previous credential is fenced and its erasure is shown per node; " +
			"the adapter restarts with the new one when its placement is re-rendered"
	}
	return out, http.StatusOK, nil
}

func (s *Server) revokeCredential(r *http.Request, name, actor string, administering bool) (any, int, error) {
	var req struct {
		Reason string `json:"reason"`
	}
	if r.ContentLength != 0 {
		if code, err := decodeStrict(r, &req); err != nil {
			return nil, code, err
		}
	}
	if !administering {
		if _, code, err := s.ownerOf(name, actor); err != nil {
			return nil, code, err
		}
	}
	if req.Reason == "" {
		req.Reason = "revoked by " + actor
	}
	n, err := s.DB.RevokePersonalCredentials(name, actor, req.Reason)
	if err != nil {
		return nil, credentialStatus(err), err
	}
	s.appendCredentialEvent("integration.credential-revoked", name, actor)
	outcomes := s.eraseCredentialCopies(r.Context(), name, false)
	conn, _ := s.DB.GetConnection(name)
	return map[string]any{
		"revoked":        n,
		"erasures":       outcomes,
		"state":          conn.State,
		"degradedReason": conn.DegradedReason,
		"note": "access ended before anything was erased. Revoke the app password at your mail provider too: " +
			"only the provider can make it stop working everywhere",
	}, http.StatusOK, nil
}

func (s *Server) authorizeBackground(r *http.Request, name, actor string) (any, int, error) {
	var req struct {
		runtimedb.BackgroundAuthorizationRequest
		DryRun bool `json:"dryRun"`
	}
	if code, err := decodeStrict(r, &req); err != nil {
		return nil, code, err
	}
	req.Connection = name
	if req.DryRun || req.Subscription == 0 {
		req.Subscription = 0
		a, err := s.DB.PrepareBackgroundAuthorization(actor, req.BackgroundAuthorizationRequest)
		if err != nil {
			return nil, credentialStatus(err), err
		}
		return map[string]any{
			"preview":           a,
			"subscriptionMatch": runtimedb.BackgroundSubscriptionMatch(name, a.Digest),
			"note": "nothing is authorized yet. File the subscription with this match, then authorize " +
				"again naming it; an Owner's approval of that exact subscription completes it",
		}, http.StatusOK, nil
	}
	a, err := s.DB.AuthorizeBackgroundUseAsPerson(actor, req.BackgroundAuthorizationRequest)
	if err != nil {
		code := credentialStatus(err)
		if errors.Is(err, runtimedb.ErrDigestMismatch) {
			return map[string]any{"subscriptionMatch": runtimedb.BackgroundSubscriptionMatch(name, a.Digest)}, code, err
		}
		return nil, code, err
	}
	s.appendCredentialEvent("integration.background-authorized", name, actor)
	return map[string]any{
		"authorization": a,
		"note": "authorized for this exact scope, Agent, calendar, credential and adapter version. It runs " +
			"only while an Owner's approval of the same subscription also holds; any change needs both again",
	}, http.StatusOK, nil
}

func (s *Server) revokeBackground(r *http.Request, name, actor string, administering bool) (any, int, error) {
	var req struct {
		Reason string `json:"reason"`
	}
	if r.ContentLength != 0 {
		if code, err := decodeStrict(r, &req); err != nil {
			return nil, code, err
		}
	}
	if !administering {
		if _, code, err := s.ownerOf(name, actor); err != nil {
			return nil, code, err
		}
	}
	if req.Reason == "" {
		req.Reason = "revoked by " + actor
	}
	if err := s.DB.RevokeBackgroundAuthorization(name, actor, req.Reason); err != nil {
		return nil, credentialStatus(err), err
	}
	s.appendCredentialEvent("integration.background-revoked", name, actor)
	return map[string]any{"revoked": true, "note": "no new mail is read from now on; queued reads were cancelled"},
		http.StatusOK, nil
}

// ---------------------------------------------------------------- erasure

// erasureOutcome is one copy's result, safe to show to anybody who may
// see the connection: node, name, whether it is proven gone, and why not.
type erasureOutcome struct {
	Node      string `json:"node"`
	SecretRef string `json:"secretRef"`
	Verified  bool   `json:"verified"`
	Orphan    bool   `json:"orphan,omitempty"`
	Detail    string `json:"detail,omitempty"`
}

// eraseCredentialCopies proves every fenced copy of a connection's
// Personal credentials gone, node by node: the adapter container that
// could hold the value in memory is removed, the node erases the secret
// under a version fence and a fresh nonce, and the evidence it returns
// must verify. Anything short of that is recorded unverified, and the
// connection stays degraded (erasure_unverified) until a retry proves it.
//
// With orphans set it also asks each placement node what Personal
// credential names it holds for this connection and erases any Keystone
// does not know as live — the version a restored snapshot never heard
// of.
func (s *Server) eraseCredentialCopies(ctx context.Context, connection string, orphans bool) []erasureOutcome {
	out := []erasureOutcome{}
	pending, err := s.DB.OutstandingCredentialErasures(connection)
	if err != nil {
		s.Log.Error("listing credential erasures", "connection", connection, "err", err)
		return out
	}
	containersGone := map[string]string{} // node -> "" when removed, else why not
	for _, e := range pending {
		// A version that was ever active may be in a running adapter's
		// environment, so its erasure also needs that container gone. One
		// that failed staging never reached a container, and removing the
		// adapter for it would take down the credential that still works.
		if _, done := containersGone[e.Node]; !done && e.WasActive {
			containersGone[e.Node] = s.removeAdapterContainer(ctx, e.Adapter, connection, e.Node)
		}
		verified, evidence, detail := s.eraseOnNode(ctx, e.Node, e.SecretRef, e.MaxVersion)
		if why := containersGone[e.Node]; verified && e.WasActive && why != "" {
			verified, detail = false, why
		}
		if err := s.DB.RecordCredentialErasure(e.Secret, e.Node, verified, evidence, detail); err != nil {
			s.Log.Error("recording credential erasure", "connection", connection, "node", e.Node, "err", err)
		}
		out = append(out, erasureOutcome{Node: e.Node, SecretRef: e.SecretRef, Verified: verified, Detail: detail})
	}
	if orphans {
		out = append(out, s.eraseOrphanCredentials(ctx, connection)...)
	}
	return out
}

// eraseOrphanCredentials erases Personal credential names on the
// connection's placement nodes that Keystone does not hold live.
func (s *Server) eraseOrphanCredentials(ctx context.Context, connection string) []erasureOutcome {
	var out []erasureOutcome
	if s.Agents == nil {
		return out
	}
	known, err := s.DB.KnownPersonalCredentialRefs(connection)
	if err != nil {
		return out
	}
	placements, err := s.DB.ListPlacements(connection)
	if err != nil {
		return out
	}
	prefix := runtimedb.PersonalCredentialPrefix(connection)
	for _, p := range placements {
		res, err := s.Agents.Apply(ctx, p.Node, &wsv1.Operation{Op: &wsv1.Operation_ListSecrets{ListSecrets: &wsv1.ListSecrets{}}})
		if err != nil || res.GetError() != nil {
			continue
		}
		for _, info := range res.GetListSecrets().GetSecrets() {
			name := info.GetName()
			if !strings.HasPrefix(name, prefix) {
				continue
			}
			if state := known[name]; state == runtimedb.CredentialActive || state == runtimedb.CredentialStaged {
				continue
			}
			verified, _, detail := s.eraseOnNode(ctx, p.Node, name, info.GetVersion())
			out = append(out, erasureOutcome{Node: p.Node, SecretRef: name, Verified: verified, Orphan: true, Detail: detail})
		}
	}
	return out
}

// eraseOnNode sends one version-fenced EraseSecret and verifies the
// evidence. It returns the stored evidence form "nonce:evidence".
func (s *Server) eraseOnNode(ctx context.Context, node, name string, maxVersion uint32) (bool, string, string) {
	if s.Agents == nil {
		return false, "", "the node agent plane is not wired"
	}
	if minor, known := s.agentMinor(node); known && minor < EraseSecretProtocolMinor {
		return false, "", fmt.Sprintf("%s runs Wardstone protocol minor %d; a verified erase needs %d — update the node",
			node, minor, EraseSecretProtocolMinor)
	}
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return false, "", "no randomness for an erase nonce"
	}
	nonce := hex.EncodeToString(raw)
	res, err := s.Agents.Apply(ctx, node, &wsv1.Operation{Op: &wsv1.Operation_EraseSecret{
		EraseSecret: &wsv1.EraseSecret{Name: name, MaxVersion: maxVersion, Nonce: nonce},
	}})
	if err != nil {
		return false, "", fmt.Sprintf("%s could not be reached to erase it", node)
	}
	if oe := res.GetError(); oe != nil {
		return false, "", fmt.Sprintf("%s refused the erase (%s): %s", node, oe.GetCode(), oe.GetMessage())
	}
	er := res.GetEraseSecret()
	if er == nil || !secreterase.Verify(name, nonce, er.GetErasedVersion(), er.GetAbsent(), er.GetEvidence()) {
		return false, "", fmt.Sprintf("%s answered, but its erase evidence does not verify", node)
	}
	return true, nonce + ":" + hex.EncodeToString(er.GetEvidence()), ""
}

// removeAdapterContainer stops and removes the connection's adapter on
// one node, because a running adapter holds the credential in its
// environment whatever the store says. It returns "" once the container
// is gone (or was never there), otherwise why it could not be shown.
func (s *Server) removeAdapterContainer(ctx context.Context, adapter, connection, node string) string {
	if s.Agents == nil {
		return "the node agent plane is not wired"
	}
	domain := adapterDomain(adapter)
	if domain == "" {
		return fmt.Sprintf("the adapter container for %q on %s cannot be named, so its removal is unproven", adapter, node)
	}
	name := capability.AdapterContainerName(domain, connection)
	// Best-effort stop; the remove's own answer is authoritative.
	s.Agents.Apply(ctx, node, &wsv1.Operation{Op: &wsv1.Operation_StopContainer{ //nolint:errcheck
		StopContainer: &wsv1.StopContainer{Name: name},
	}})
	res, err := s.Agents.Apply(ctx, node, &wsv1.Operation{Op: &wsv1.Operation_RemoveContainer{
		RemoveContainer: &wsv1.RemoveContainer{Name: name},
	}})
	if err != nil {
		return fmt.Sprintf("%s could not be reached to remove the adapter container", node)
	}
	if oe := res.GetError(); oe != nil && oe.GetCode() != wsv1.ErrorCode_ERROR_CODE_NOT_FOUND {
		return fmt.Sprintf("%s did not remove the adapter container (%s)", node, oe.GetCode())
	}
	if err := s.DB.MarkPlacementContainerStopped(connection, node); err != nil && !errors.Is(err, runtimedb.ErrNotFound) {
		s.Log.Error("recording adapter removal", "connection", connection, "node", node, "err", err)
	}
	return ""
}

// adapterDomain names the integration domain of an adapter program:
// "wild-mail-adapter" is the mail domain's.
func adapterDomain(adapter string) string {
	if d, ok := strings.CutPrefix(adapter, "wild-"); ok {
		if d, ok = strings.CutSuffix(d, "-adapter"); ok && d != "" {
			return d
		}
	}
	return ""
}

func (s *Server) agentMinor(node string) (uint32, bool) {
	if s.LiveNodes == nil {
		return 0, false
	}
	for _, n := range s.LiveNodes() {
		if n.NodeID == node {
			return n.ProtocolMinor, true
		}
	}
	return 0, false
}

// appendCredentialEvent records THAT something happened to a
// connection's credential, by whom — never what the credential is.
func (s *Server) appendCredentialEvent(typ, connection, who string) {
	if _, err := s.DB.AppendEvent(runtimedb.Event{
		Type: typ, Name: connection, Message: connection,
		Attrs: map[string]string{"by": who},
	}); err != nil {
		s.Log.Error("credential event append failed", "type", typ, "err", err)
	}
}
