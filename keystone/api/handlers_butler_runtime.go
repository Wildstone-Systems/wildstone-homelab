package api

// The Agent's runtime record and its runtime principal (ADR-W039 §3.3,
// §3.7, §4.1; Phase 1, #336).
//
// Every Agent has a runtime — today always the native, in-process
// runner — and every runtime authenticates as a real Keystone
// principal bound to that Agent for the principal's life. The native
// runtime's credential is minted rather than synthesised (§3.3), so an
// Agent that was made yesterday and an Agent that will run on Hermes
// next year are credentialed the same way: `runtime-<agent>-<n>`, role
// butler, acting for nobody, bound once.
//
// Creating an Agent mints the principal and issues no bearer for it:
// nothing calls in as the native runtime yet, and a token nobody
// presents is only a secret waiting to leak. A bearer exists from the
// moment an Owner asks for one — rotation, below — and that is the one
// moment the secret is ever shown. Rotation, revocation and replacing
// the runtime are consequential (ADR-W033 A4.1: minting any bearer
// credential, and withdrawing authority). The gateway that serves
// calls made under the principal is Phase 2.

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/chickenrat17/wildstone-homelab/keystone/agentapi"
	"github.com/chickenrat17/wildstone-homelab/keystone/capability"
	"github.com/chickenrat17/wildstone-homelab/keystone/pkgspec"
	"github.com/chickenrat17/wildstone-homelab/keystone/plan"
	"github.com/chickenrat17/wildstone-homelab/keystone/principal"
	"github.com/chickenrat17/wildstone-homelab/keystone/proposals"
	"github.com/chickenrat17/wildstone-homelab/keystone/resource"
	"github.com/chickenrat17/wildstone-homelab/keystone/runtimedb"
)

// runtimePrincipalPrefix names the principal minted for an Agent's
// runtime, as chatAdapterPrincipalPrefix does for a chat adapter.
const runtimePrincipalPrefix = "runtime-"

// runtimePrincipalName is `runtime-<agent>-<serial>` in the principal
// name discipline (lowercase [a-z0-9._-], 2–63 chars). A Butler's name
// is a person's word for it — "Kitchen Helper" — so it is slugged, not
// used raw; and because two names can slug alike ("Health", "health"),
// the caller passes disambiguate=true to fold the Butler's seq in when
// the plain form is already somebody else's.
func runtimePrincipalName(b runtimedb.Butler, serial int, disambiguate bool) string {
	var slug strings.Builder
	dash := false
	for _, r := range strings.ToLower(b.Name) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			slug.WriteRune(r)
			dash = false
		case !dash && slug.Len() > 0:
			slug.WriteByte('-')
			dash = true
		}
	}
	s := strings.TrimRight(slug.String(), "-")
	// Leave room for the prefix, a seq, and a serial inside 63 chars.
	if len(s) > 36 {
		s = strings.TrimRight(s[:36], "-")
	}
	if s == "" {
		s = fmt.Sprintf("agent%d", b.Seq)
	} else if disambiguate {
		s = fmt.Sprintf("%s.%d", s, b.Seq)
	}
	return fmt.Sprintf("%s%s-%d", runtimePrincipalPrefix, s, serial)
}

// runtimePrincipalStem is a runtime principal's name without its
// serial: `runtime-health-3` → `runtime-health`. It is what every
// credential the same Agent holds has in common, and what the secret a
// packaged runtime reads its token from is named after, so that name
// survives rotation.
func runtimePrincipalStem(name string) string {
	if i := strings.LastIndexByte(name, '-'); i > 0 && strings.Trim(name[i+1:], "0123456789") == "" {
		return name[:i]
	}
	return name
}

// runtimeSecretName is the node secret a packaged runtime's container
// reads its bearer from: `runtime-<agent>-token`, per Agent and stable
// across rotations, as a chat adapter's is per capability.
func runtimeSecretName(principalName string) string {
	return runtimePrincipalStem(principalName) + "-token"
}

// ensureNativeRuntime gives an Agent the native runtime record and a
// live, bound runtime principal, and marks the runtime connected.
//
// It converges rather than creates: an Agent with a record keeps it,
// an Agent with a credential keeps that credential (re-enabled, since
// the one path that disables it is retiring), and only an Agent with
// none is minted one. That makes it the single routine for a new
// Agent, a duplicated one, a restored one, and the startup backfill of
// Agents that predate 0081 — four callers, one set of invariants.
//
// `connected` is truthful for native and only for native: the runner
// is loaded with the process and has nothing to register. An Agent
// whose runtime is something else is left alone; bringing an external
// runtime back after a retirement is a re-registration, and the
// credential it needs is minted by that flow, not this one.
func (s *Server) ensureNativeRuntime(b runtimedb.Butler, id principal.Identity) error {
	by := actingMember(id)
	rt, err := s.DB.GetButlerRuntime(b.Seq)
	switch {
	case errors.Is(err, runtimedb.ErrNotFound):
		if err := s.DB.SetButlerRuntime(b.Seq, runtimedb.ButlerRuntime{Type: agentapi.RuntimeNative}, by); err != nil {
			return err
		}
	case err != nil:
		return err
	case rt.Type != agentapi.RuntimeNative:
		return nil
	}

	// The gateway-side disclosure filter (ADR-W039 §3.5 rule 3,
	// #345) keys on this row's processing_class rather than
	// wrapping the provider object; a native runtime declares itself
	// from its assigned model's kind, never by asking anyone.
	if err := s.DB.SetNativeRuntimeProcessingClass(b.Seq, b.ModelKind, by); err != nil {
		return err
	}

	bound, err := s.DB.ListRuntimePrincipals(b.Seq)
	if err != nil {
		return err
	}
	var name string
	if len(bound) > 0 {
		name = bound[len(bound)-1]
		if err := s.DB.SetPrincipalDisabled(name, false); err != nil {
			return fmt.Errorf("re-enabling %s: %w", name, err)
		}
	} else {
		name, err = s.mintRuntimePrincipal(b, 1, id)
		if err != nil {
			return err
		}
	}
	if err := s.DB.BindRuntimePrincipal(name, b.Seq); err != nil {
		return err
	}
	return s.DB.SetRuntimeHealth(b.Seq, agentapi.HealthConnected)
}

// mintRuntimePrincipal creates `runtime-<agent>-<serial>` with the
// butler role and nobody as its acting identity. The Person on any
// call the runtime makes comes from the task handle Wildstone issued
// (§4.1), never from the credential, so the credential carries none.
//
// An Agent that already holds credentials keeps their stem: the
// successor to `runtime-health.7-1` is `runtime-health.7-2`, not
// whatever the plain form happens to be free for this serial, so one
// Agent's credentials never read as two Agents' and the secret named
// after the stem stays put across rotations.
//
// No token is issued here. Creating an Agent needs none, and the one
// caller that does (rotation) issues it under the consequential gate.
func (s *Server) mintRuntimePrincipal(b runtimedb.Butler, serial int, id principal.Identity) (string, error) {
	name := runtimePrincipalName(b, serial, false)
	if bound, err := s.DB.ListRuntimePrincipals(b.Seq); err != nil {
		return "", err
	} else if len(bound) > 0 {
		name = fmt.Sprintf("%s-%d", runtimePrincipalStem(bound[len(bound)-1]), serial)
	}
	if existing, err := s.DB.GetPrincipal(name); err == nil {
		// The name is taken. It is ours only if it is bound to this
		// Agent — which ensureNativeRuntime would have found — so this
		// is a slug collision with another Agent, or a name an operator
		// made by hand. Either way, fold the seq in.
		if owner, ok, err := s.DB.RuntimePrincipalButler(existing.Name); err != nil {
			return "", err
		} else if !ok || owner != b.Seq {
			name = runtimePrincipalName(b, serial, true)
		}
	} else if !errors.Is(err, runtimedb.ErrNotFound) {
		return "", err
	}
	if _, err := s.DB.GetPrincipal(name); errors.Is(err, runtimedb.ErrNotFound) {
		if err := s.Principals.Create(name, runtimedb.RoleButler, ""); err != nil {
			return "", fmt.Errorf("minting the runtime principal for %s: %w", b.Name, err)
		}
		s.event("principal.created", "", id, fmt.Sprintf("%s (%s, runtime of %s)", name, runtimedb.RoleButler, b.Name))
	} else if err != nil {
		return "", err
	}
	return name, nil
}

// revokeAgentRuntime is §3.7's immediate revocation: every credential
// the Agent ever held is disabled and every bearer issued for it is
// revoked, so each fails closed on its next request (Authenticate
// checks both per request); in-flight attempts are fenced; and the
// runtime is marked revoked. No runtime cooperation is needed.
//
// Tokens are revoked as well as the principal disabled because
// re-enabling is a thing that happens — restoring a retired Agent,
// moving one back to native — and a principal that came back with its
// old bearers still live would hand yesterday's runtime today's Agent.
func (s *Server) revokeAgentRuntime(b runtimedb.Butler) error {
	bound, err := s.DB.ListRuntimePrincipals(b.Seq)
	if err != nil {
		return err
	}
	for _, name := range bound {
		if err := s.DB.SetPrincipalDisabled(name, true); err != nil {
			return fmt.Errorf("disabling %s: %w", name, err)
		}
		if _, err := s.DB.RevokeTokensFor(name); err != nil {
			return err
		}
	}
	if err := s.fenceRuntimeTasks(b); err != nil {
		return err
	}
	err = s.DB.SetRuntimeHealth(b.Seq, agentapi.HealthRevoked)
	if errors.Is(err, runtimedb.ErrNotFound) {
		// An Agent with no runtime record has nothing to revoke. It
		// cannot happen after 0081, but a retire must not fail on it.
		return nil
	}
	return err
}

// fenceRuntimeTasks is where revocation fences the Agent's in-flight
// attempts (§3.7): every task of this Butler that has not yet finished
// has its in-flight attempts fenced (a late result from the old runtime
// is refused, the way butler.TaskLedger.FenceAttempts already does for
// a superseded attempt) and its live task handle withdrawn — the two
// things Ledger.FenceTask does together — so a revoked runtime cannot
// reach the gateway again with material it was already holding, over
// HTTP or in-process alike.
//
// nil when this deployment has no Ledger wired: a control plane that
// has never built one has no gateway-served task to fence either, and
// this must not fail retiring or revoking an Agent that predates it.
func (s *Server) fenceRuntimeTasks(b runtimedb.Butler) error {
	if s.Ledger == nil {
		return nil
	}
	open, err := s.DB.ListOpenButlerTasksByButler(b.Name)
	if err != nil {
		return err
	}
	for _, t := range open {
		if _, err := s.Ledger.FenceTask(t.ID); err != nil {
			return fmt.Errorf("fencing %s of %s: %w", t.ID, b.Name, err)
		}
	}
	return nil
}

// BackfillRuntimePrincipals mints a runtime principal for every live
// Agent that has none. 0081 gives each existing Agent its native
// runtime record but cannot mint the credential — a principal name
// goes through keystone/principal's discipline, which SQL does not
// apply — so the process does it once at startup, as it enrols
// pre-0059 Agents in assigned tools. Idempotent: an Agent whose record
// already names a credential is skipped, and a retired one is left
// revoked.
func (s *Server) BackfillRuntimePrincipals() error {
	if s.DB == nil || s.Principals == nil {
		return nil
	}
	butlers, err := s.DB.ListButlers()
	if err != nil {
		return err
	}
	system := principal.Identity{Name: "system"}
	for _, b := range butlers {
		if !b.RetiredAt.IsZero() {
			continue
		}
		rt, err := s.DB.GetButlerRuntime(b.Seq)
		if err != nil && !errors.Is(err, runtimedb.ErrNotFound) {
			return err
		}
		if err == nil && (rt.Principal != "" || rt.Type != agentapi.RuntimeNative) {
			continue
		}
		if err := s.ensureNativeRuntime(b, system); err != nil {
			return fmt.Errorf("agent %s: %w", b.Name, err)
		}
	}
	return nil
}

// liveAgentRuntime resolves the Agent a runtime route names and its
// record, refusing a retired one: its runtime was revoked when it
// retired, and restoring is the one way back.
func (s *Server) liveAgentRuntime(name string) (runtimedb.Butler, runtimedb.ButlerRuntime, int, error) {
	b, err := s.DB.GetButler(name)
	if errors.Is(err, runtimedb.ErrNotFound) || (err == nil && !b.RetiredAt.IsZero()) {
		return runtimedb.Butler{}, runtimedb.ButlerRuntime{}, http.StatusNotFound,
			fmt.Errorf("no butler named %q, or it is retired", name)
	}
	if err != nil {
		return runtimedb.Butler{}, runtimedb.ButlerRuntime{}, http.StatusInternalServerError, err
	}
	rt, err := s.DB.GetButlerRuntime(b.Seq)
	if err != nil {
		return runtimedb.Butler{}, runtimedb.ButlerRuntime{}, http.StatusInternalServerError, err
	}
	return b, rt, http.StatusOK, nil
}

// butlerRuntime is GET /api/v1/butlers/{name}/runtime: the Agent's
// runtime record in the Agent API's shape (§3.3). A read, like the rest
// of what the Console shows about an Agent; the credential is a
// reference here and never a secret.
func (s *Server) butlerRuntime(r *http.Request, id principal.Identity) (any, int, error) {
	name := r.PathValue("name")
	rt, err := s.DB.GetButlerRuntimeByName(name)
	if errors.Is(err, runtimedb.ErrNotFound) {
		return nil, http.StatusNotFound, fmt.Errorf("no butler named %q", name)
	}
	if err != nil {
		return nil, http.StatusInternalServerError, err
	}
	out := butlerRuntimeResponse{RuntimeRecord: rt.Record()}
	if b, err := s.DB.GetButler(name); err == nil {
		if pr, ok := s.packagedRuntimeFor(b, rt, id); ok {
			out.Package = &pr
		}
	}
	return out, http.StatusOK, nil
}

// butlerRuntimeResponse is the runtime record, plus the package it runs
// from when Wildstone installed it, so the Console can say when that
// package is behind the catalog and offer to upgrade it.
type butlerRuntimeResponse struct {
	agentapi.RuntimeRecord
	Package *packagedRuntime `json:"package,omitempty"`
}

// packagedRuntime is the installed package behind an Agent's runtime.
type packagedRuntime struct {
	Name     string `json:"name"`
	Instance string `json:"instance"`
	// Installed is the version the install was last applied at, and
	// Available the catalog's current one.
	Installed string `json:"installedVersion"`
	Available string `json:"availableVersion,omitempty"`
	// Upgradable is whether upgrading to Available would change what
	// runs (a dry run of the upgrade Plan).
	Upgradable bool `json:"upgradable,omitempty"`
}

// packagedRuntimeFor describes the packaged install behind b's runtime,
// if it has one still in desired state. Upgradable is decided by a dry
// run of the upgrade Plan rather than by comparing versions: a release
// that changes nothing this install renders is not an upgrade to offer,
// and one offered anyway would fail as a no-op every time it was
// pressed while the version stayed behind forever.
func (s *Server) packagedRuntimeFor(b runtimedb.Butler, rt runtimedb.ButlerRuntime, id principal.Identity) (packagedRuntime, bool) {
	r, ok, err := s.renderPackagedRuntime(rt.Principal, rt)
	if err != nil || !ok {
		return packagedRuntime{}, false
	}
	out := packagedRuntime{
		Name: r.pkg.Metadata.Name, Instance: r.instance,
		Installed: r.installed, Available: r.pkg.Metadata.Version,
	}
	if out.Available != out.Installed {
		_, err := s.fileRuntimeRender(b, rt.Principal, rt, id, out.Installed, true)
		out.Upgradable = err == nil
	}
	return out, true
}

// upgradeButlerRuntime is POST /api/v1/butlers/{name}/runtime/upgrade:
// file a Plan that re-renders the Agent's packaged runtime at the
// catalog's current version and restarts it (#389).
//
// Before this, a new agent-hermes version could not reach an installed
// runtime. The Runtime tab re-renders only when the profile or class
// changes, and the Packages screen's reconfigure expands the version the
// install was made from, so it found nothing to change. Nothing is
// applied here: the Owner approves the Plan, as for any install.
func (s *Server) upgradeButlerRuntime(r *http.Request, id principal.Identity) (any, int, error) {
	b, current, code, err := s.liveAgentRuntime(r.PathValue("name"))
	if err != nil {
		return nil, code, err
	}
	pr, ok := s.packagedRuntimeFor(b, current, id)
	if !ok {
		return nil, http.StatusNotFound, fmt.Errorf("%s's runtime is not a Wildstone-installed package, so there is nothing to upgrade", b.Name)
	}
	if pr.Available == pr.Installed {
		return nil, http.StatusConflict, fmt.Errorf("%s's runtime already runs %s %s, the catalog's current version", b.Name, pr.Name, pr.Installed)
	}
	planID, err := s.reRenderPackagedRuntime(b, current.Principal, current, id, pr.Installed)
	if errors.Is(err, proposals.ErrNoOp) {
		// The new version renders exactly what is running, so there is
		// nothing to restart. The install still reads as the old version
		// until something about it changes.
		return nil, http.StatusConflict, fmt.Errorf(
			"%s %s renders exactly what %s's runtime already runs, so there is nothing to restart", pr.Name, pr.Available, b.Name)
	}
	if err != nil {
		return nil, http.StatusBadGateway, fmt.Errorf("re-rendering %s's runtime at %s: %w", b.Name, pr.Available, err)
	}
	s.event("butler.runtime", planID, id, fmt.Sprintf("upgrade of %s's runtime filed: %s %s to %s",
		b.Name, pr.Name, pr.Installed, pr.Available))
	return map[string]any{"plan": planID, "from": pr.Installed, "to": pr.Available}, http.StatusOK, nil
}

// packagedRuntimePackage is the one Wildstone-packaged Agent runtime
// today (H5, #351). A second one (H8's split sandbox, or a future
// packaged OpenClaw) would add its own entry to a lookup here rather
// than replace this — what reRenderPackagedRuntime asks is "does
// agent-hermes name this Butler", not "does some package".
const packagedRuntimePackage = "agent-hermes"

// reRenderPackagedRuntime is H6 (#352, design doc §5.5): Hermes freezes
// a session's tool set at start, so a changed execution profile or
// processing class takes effect only by re-rendering the bound
// package's managed config.yaml and restarting the container — never
// in place, and never applied here. It re-expands the installed
// agent-hermes-<instance> capability bound to this Agent's runtime
// with the updated param values and files the result through the ONE
// proposal pipeline (ADR-W017 A3), exactly as enableCapability does
// for a Console-driven install or reconfigure, under the acting
// Owner's own identity. Nothing is applied: the Owner still approves
// the Plan, and the reconciler recreates the container from there.
//
// planID is "" with a nil error for an Agent with no packaged runtime
// bound — a native Agent, or one on an externally-managed runtime
// (OpenClaw, or a Hermes somebody runs themselves), is unaffected by
// design: there is no Wildstone-owned container for a Plan to recreate.
//
// The bound install is found the way packages/agent-hermes/
// package.yaml documents it must be found: its "instance" param is
// this Butler's runtime-principal stem (runtimePrincipalStem, minus
// the "runtime-" prefix), which the Console flow that installs it (H4)
// guarantees by naming the install after the credential it just
// minted — the same convention chat-telegram's own instance uses, read
// back with installedFromPlans exactly as disableCapability's sibling
// check reads it.
//
// upgradeFrom, when set, is the version the install runs now: the Plan
// is filed as an upgrade to the catalog's current version rather than
// as a profile change (upgradeButlerRuntime, #389). The re-render is the
// same either way, because it always expands the catalog's current
// version with the install's own params.
func (s *Server) reRenderPackagedRuntime(b runtimedb.Butler, principalName string, next runtimedb.ButlerRuntime, id principal.Identity, upgradeFrom string) (string, error) {
	return s.fileRuntimeRender(b, principalName, next, id, upgradeFrom, false)
}

// runtimeRender is a packaged runtime's install, re-rendered at the
// catalog's current version with its own recorded params.
type runtimeRender struct {
	pkg      *pkgspec.Package
	params   map[string]string
	instance string
	// installed is the version the install was last applied at.
	installed string
	docs      []string
	deletes   []plan.DeletionRef
	capName   string
}

// renderPackagedRuntime finds the packaged install bound to the runtime
// principal principalName and renders it at the catalog's version, with
// next's profile and class overriding the install's own. ok is false
// when there is none: a native Agent, a runtime Wildstone does not
// manage the container for, or an install that has since been removed
// (#389). Removal through the Services screen files a plan with no
// capability origin, so the install's origin outlives it; only desired
// state says whether it is still there.
func (s *Server) renderPackagedRuntime(principalName string, next runtimedb.ButlerRuntime) (runtimeRender, bool, error) {
	if s.Catalog == nil || principalName == "" {
		return runtimeRender{}, false, nil
	}
	instance := strings.TrimPrefix(runtimePrincipalStem(principalName), runtimePrincipalPrefix)
	if instance == "" {
		return runtimeRender{}, false, nil
	}
	origin, ok := s.installOrigin(packagedRuntimePackage, instance)
	if !ok {
		return runtimeRender{}, false, nil
	}
	pkg, err := s.Catalog.Get(packagedRuntimePackage, "")
	if err != nil {
		return runtimeRender{}, false, fmt.Errorf("%s is installed but is no longer in the catalog: %w", packagedRuntimePackage, err)
	}
	// The install's own recorded params, with only the two fields a
	// runtime change touches overridden — everything else about the
	// runtime (node, model, instance) is untouched, the same "replaces
	// one field" discipline setButlerRuntime itself follows.
	params := make(map[string]string, len(origin.Params)+2)
	for k, v := range origin.Params {
		params[k] = v
	}
	if next.ExecutionProfile != "" {
		params["execution_profile"] = next.ExecutionProfile
	}
	if next.ProcessingClass != "" {
		params["processing_class"] = string(next.ProcessingClass)
	}

	enabled, err := s.enabledCapabilities()
	if err != nil {
		return runtimeRender{}, false, err
	}
	providers := append([]*pkgspec.Package{pkg}, s.Catalog.ProvidersFor(enabled)...)
	current, err := s.State.Load()
	if err != nil {
		return runtimeRender{}, false, fmt.Errorf("loading statefile: %w", err)
	}
	rendered, err := capability.NewEngine().Expand(pkg, params, capability.NewCatalog(providers...).WithCurrent(current))
	if err != nil {
		return runtimeRender{}, false, fmt.Errorf("re-rendering %s: %w", pkg.Metadata.Name, err)
	}
	if !anyDesired(rendered, current) {
		return runtimeRender{}, false, nil
	}
	if err := claimedByAnotherCapability(pkg, current, rendered); err != nil {
		return runtimeRender{}, false, err
	}
	docs := make([]string, 0, len(rendered))
	for _, res := range rendered {
		data, err := resource.Marshal(res)
		if err != nil {
			return runtimeRender{}, false, err
		}
		docs = append(docs, string(data))
	}
	capName, _ := pkg.CapabilityFor(params) // Expand already refused a bad instance
	return runtimeRender{
		pkg: pkg, params: params, instance: instance, installed: origin.Version,
		docs: docs, deletes: droppedByUpgrade(pkg, current, rendered), capName: capName,
	}, true, nil
}

// anyDesired reports whether any container the install renders is in
// desired state. Containers, because removing an install keeps its data
// Volume (data is never proposed for deletion), and a kept Volume is not
// a runtime anybody could restart.
func anyDesired(rendered, current []resource.Resource) bool {
	have := map[string]bool{}
	for _, r := range current {
		if r.Kind == resource.KindContainer {
			have[r.Metadata.Name] = true
		}
	}
	for _, r := range rendered {
		if r.Kind == resource.KindContainer && have[r.Metadata.Name] {
			return true
		}
	}
	return false
}

// installOrigin is the capability origin one instance of a package was
// last applied with: its version and params.
func (s *Server) installOrigin(pkg, instance string) (plan.CapabilityOrigin, bool) {
	docs, err := s.DB.AppliedPlanJSONMentioning(`"package":` + strconv.Quote(pkg))
	if err != nil {
		return plan.CapabilityOrigin{}, false
	}
	for _, doc := range docs { // newest first
		var stored plan.StoredProposal
		if json.Unmarshal([]byte(doc), &stored) != nil || stored.Origin == nil {
			continue
		}
		if stored.Origin.Package == pkg && stored.Origin.Params[pkgspec.InstanceParam] == instance {
			return *stored.Origin, true
		}
	}
	return plan.CapabilityOrigin{}, false
}

// fileRuntimeRender files (or, with dryRun, only checks) the Plan that
// re-renders b's packaged runtime. planID is "" with a nil error when
// there is no packaged runtime; a dry run returns "" with a nil error
// when the Plan would change something, and proposals.ErrNoOp when not.
func (s *Server) fileRuntimeRender(b runtimedb.Butler, principalName string, next runtimedb.ButlerRuntime, id principal.Identity, upgradeFrom string, dryRun bool) (string, error) {
	r, ok, err := s.renderPackagedRuntime(principalName, next)
	if err != nil {
		return "", fmt.Errorf("%s's runtime: %w", b.Name, err)
	}
	if !ok {
		return "", nil
	}
	summary := fmt.Sprintf("restart %s's runtime (%s): execution profile %s, processing class %s",
		b.Name, r.capName, r.params["execution_profile"], r.params["processing_class"])
	if upgradeFrom != "" {
		summary = fmt.Sprintf("upgrade %s's runtime (%s) from %s %s to %s",
			b.Name, r.capName, r.pkg.Metadata.Name, upgradeFrom, r.pkg.Metadata.Version)
	}
	out, code, err := s.submitProposalOpts(id, proposalRequest{
		Summary: summary,
		Put:     r.docs,
		Delete:  r.deletes,
	}, proposalOpts{
		Origin: &plan.CapabilityOrigin{Package: r.pkg.Metadata.Name, Version: r.pkg.Metadata.Version, Params: r.params},
		DryRun: dryRun,
	})
	if err != nil {
		return "", fmt.Errorf("filing the restart plan (status %d): %w", code, err)
	}
	if dryRun {
		return "", nil
	}
	view, ok := out.(planView)
	if !ok {
		return "", fmt.Errorf("filing the restart plan: unexpected response type %T", out)
	}
	return view.ID, nil
}

// setButlerRuntimeResponse is what PUT .../runtime answers with: the
// Agent's runtime record, plus — only when this change re-rendered a
// bound packaged runtime (H6, #352) — the Plan that re-render filed.
// The record above already carries the new profile, but Hermes will
// not run under it until the Plan is approved and the reconciler
// restarts the container (design doc §5.5); the Console shows that
// with the Plan ref rather than the profile looking live early.
type setButlerRuntimeResponse struct {
	agentapi.RuntimeRecord
	Plan string `json:"plan,omitempty"`
}

// setButlerRuntime is PUT /api/v1/butlers/{name}/runtime: the Owner
// decides which runtime backs the Agent and how it is reached (§3.3).
//
// A runtime change replaces the `runtime` field and nothing else
// (§3.7): the old credential is revoked, the new runtime registered,
// and identity, members, tools, Connections, routines and memory are
// untouched — none of them is read here, let alone written. Moving to
// native is complete in itself, because the native runner is loaded
// with the process: its credential is re-enabled and it is connected.
// Moving to anything else leaves the Agent `unregistered` with a
// disabled credential until the Owner rotates one for the new runtime
// and it authenticates.
//
// Changing the configuration of the runtime the Agent is already on —
// its endpoint, delivery, class, profile — revokes nothing. That is a
// different act with a different guard, and folding the two together
// would make every endpoint edit a credential reset.
//
// CONSEQUENTIAL (A4.1): a type change withdraws a credential, and the
// declared processing class decides what the gateway will let reach a
// model outside the house.
func (s *Server) setButlerRuntime(r *http.Request, id principal.Identity) (any, int, error) {
	name := r.PathValue("name")
	var req struct {
		Type       string `json:"type"`
		Endpoint   string `json:"endpoint"`
		Negotiated struct {
			Delivery        string `json:"delivery"`
			ProcessingClass string `json:"processingClass"`
		} `json:"negotiated"`
		ExecutionProfileRef string `json:"executionProfileRef"`
	}
	if code, err := decodeStrict(r, &req); err != nil {
		return nil, code, err
	}
	next := runtimedb.ButlerRuntime{
		Type:             agentapi.RuntimeType(req.Type),
		Endpoint:         strings.TrimSpace(req.Endpoint),
		Delivery:         agentapi.DeliveryMode(req.Negotiated.Delivery),
		ProcessingClass:  agentapi.ProcessingClass(req.Negotiated.ProcessingClass),
		ExecutionProfile: strings.TrimSpace(req.ExecutionProfileRef),
	}
	// Everything the request can get wrong is refused before anything
	// is revoked: a bad body must not cost the Agent its credential.
	if !next.Type.Valid() {
		return nil, http.StatusBadRequest, fmt.Errorf("runtime type must be one of native, hermes, openclaw")
	}
	if next.Type == agentapi.RuntimeNative && next.Endpoint != "" {
		return nil, http.StatusBadRequest, fmt.Errorf("the native runtime is in-process and has no endpoint")
	}
	if next.Delivery != "" && !next.Delivery.Valid() {
		return nil, http.StatusBadRequest, fmt.Errorf("delivery must be push or pull")
	}
	if next.ProcessingClass != "" && !next.ProcessingClass.Valid() {
		return nil, http.StatusBadRequest, fmt.Errorf("processingClass must be local_only or cloud")
	}
	if next.ExecutionProfile != "" && !agentapi.ExecutionProfileRef(next.ExecutionProfile).Valid() {
		return nil, http.StatusBadRequest, fmt.Errorf("executionProfileRef must be one of platform-only, sandboxed, host-admin")
	}
	b, current, code, err := s.liveAgentRuntime(name)
	if err != nil {
		return nil, code, err
	}
	// Host administration is Owner-class authority (design doc §5.3,
	// §5.4; DSN §6: a Member-class Agent cannot hold the `host` family at
	// all). A native Agent's Tier 3 comes from its own opt-in tools
	// (H7), never from this profile, so setting it here on a native,
	// Member-class Agent would promise a ceiling nothing raises.
	if agentapi.ExecutionProfileRef(next.ExecutionProfile) == agentapi.ExecutionProfileHostAdmin &&
		next.Type == agentapi.RuntimeNative && b.AgentClass != runtimedb.AgentClassOwner {
		return nil, http.StatusBadRequest, fmt.Errorf(
			"host administration needs a non-native runtime or an Owner-class Agent: a native Agent's Tier 3 comes from its own opt-in tools (H7), not from the execution profile")
	}

	var restartPlan string
	switch {
	case next.Type != current.Type:
		if err := s.revokeAgentRuntime(b); err != nil {
			return nil, http.StatusInternalServerError, err
		}
		if err := s.DB.SetButlerRuntime(b.Seq, next, actingMember(id)); err != nil {
			return nil, http.StatusBadRequest, err
		}
		if next.Type == agentapi.RuntimeNative {
			if err := s.ensureNativeRuntime(b, id); err != nil {
				return nil, http.StatusInternalServerError, err
			}
		} else if err := s.DB.SetRuntimeHealth(b.Seq, agentapi.HealthUnregistered); err != nil {
			return nil, http.StatusInternalServerError, err
		}
		s.event("butler.runtime", "", id, fmt.Sprintf("runtime of %s changed from %s to %s; previous credentials revoked",
			b.Name, current.Type, next.Type))
	default:
		if err := s.DB.SetButlerRuntime(b.Seq, next, actingMember(id)); err != nil {
			return nil, http.StatusBadRequest, err
		}
		s.event("butler.runtime", "", id, fmt.Sprintf("runtime of %s reconfigured (%s)", b.Name, next.Type))
		// H6 (#352, design doc §5.5): the runtime record above already
		// says the new profile, but a bound packaged runtime does not
		// actually run under it until its managed config is
		// re-rendered and its container restarted — Hermes freezes a
		// session's tool set at start, so this is "re-template +
		// restart", never a live change. Only a profile or class
		// change needs it: an endpoint or delivery edit does not touch
		// what the container renders.
		if next.ExecutionProfile != current.ExecutionProfile || next.ProcessingClass != current.ProcessingClass {
			planID, err := s.reRenderPackagedRuntime(b, current.Principal, next, id, "")
			if err != nil {
				return nil, http.StatusBadGateway, fmt.Errorf(
					"the runtime record was updated, but re-rendering its packaged runtime for the restart failed: %w", err)
			}
			restartPlan = planID
		}
	}
	rt, err := s.DB.GetButlerRuntime(b.Seq)
	if err != nil {
		return nil, http.StatusInternalServerError, err
	}
	return setButlerRuntimeResponse{RuntimeRecord: rt.Record(), Plan: restartPlan}, http.StatusOK, nil
}

// runtimeCredentialResponse is what rotating a runtime credential
// answers with. It is the one body on the runtime routes that may
// carry a secret, in the only moment it ever exists in a response
// (ADR-W007 A3/B3; §4.1 "shows the credential once"), and writeJSON
// exempts it from redaction by type. When the secret was placed on a
// node instead, Secret is empty and Node says where it went.
type runtimeCredentialResponse struct {
	CredentialRef string    `json:"credentialRef"`
	Secret        string    `json:"secret,omitempty"`
	Node          string    `json:"node,omitempty"`
	SecretName    string    `json:"secretName,omitempty"`
	ExpiresAt     time.Time `json:"expiresAt,omitzero"`
	Plan          string    `json:"plan,omitempty"`
}

// rotateRuntimeCredential is POST /api/v1/butlers/{name}/runtime/credential:
// issue the Agent's runtime credential (§4.1 "credential issue and
// rotation"). A successor principal `runtime-<agent>-<n+1>` is minted
// and bound, a bearer is issued for it, and every predecessor is
// disabled with its tokens revoked — rotation is minting a successor
// and disabling the predecessor, nothing subtler.
//
// The secret is returned exactly once and never stored or logged. With
// `node` in the body it is placed on that node as the provided secret
// `runtime-<agent>-token` instead, the way a chat adapter's token is
// (mintChatAdapter; ADR-W024 A10's executor), and the response names
// the secret rather than carrying it. That is the path for a
// Wildstone-packaged runtime; a runtime somebody runs elsewhere takes
// the secret from the response and pastes it into its own config.
//
// Bearers have no expiry, as a chat adapter's and a publisher's do
// not: a runtime is a standing thing, and the way to end its
// credential is here, not a clock.
//
// CONSEQUENTIAL (A4.1): minting any bearer credential.
func (s *Server) rotateRuntimeCredential(r *http.Request, id principal.Identity) (any, int, error) {
	name := r.PathValue("name")
	var req struct {
		Node string `json:"node"`
	}
	dec := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 4096))
	dec.DisallowUnknownFields()
	// An empty body is a valid "show me the secret" (io.EOF).
	if err := dec.Decode(&req); err != nil && !errors.Is(err, io.EOF) {
		return nil, http.StatusBadRequest, fmt.Errorf("parsing body: %w", err)
	}
	b, rt, code, err := s.liveAgentRuntime(name)
	if err != nil {
		return nil, code, err
	}
	if req.Node != "" && s.Agents == nil {
		return nil, http.StatusServiceUnavailable, fmt.Errorf("agent plane not wired")
	}
	previous, err := s.DB.ListRuntimePrincipals(b.Seq)
	if err != nil {
		return nil, http.StatusInternalServerError, err
	}
	next, err := s.mintRuntimePrincipal(b, len(previous)+1, id)
	if err != nil {
		return nil, http.StatusInternalServerError, err
	}
	if err := s.DB.BindRuntimePrincipal(next, b.Seq); err != nil {
		return nil, http.StatusInternalServerError, err
	}

	out := runtimeCredentialResponse{CredentialRef: next}
	if req.Node == "" {
		secret, err := s.Principals.Mint(next, "", 0)
		if err != nil {
			return nil, http.StatusInternalServerError, err
		}
		out.Secret = secret
		s.event("token.minted", "", id, "for "+next)
	} else {
		secretName := runtimeSecretName(next)
		dependents, err := s.secretDependents(req.Node, secretName)
		if err != nil {
			return nil, http.StatusInternalServerError, err
		}
		summary := fmt.Sprintf("mint a runtime token for %s → secret/%s on %s (%d dependent container(s) re-created)",
			b.Name, secretName, req.Node, len(dependents))
		stored, _ := json.Marshal(map[string]any{"summary": summary, "secret": secretName, "node": req.Node,
			"principal": next, "role": string(runtimedb.RoleButler), "butler": b.Name, "dependents": dependents})
		planID, err := s.DB.InsertPlan(secretsPrincipal, id.Name, summary, "medium", string(stored))
		if err != nil {
			return nil, http.StatusInternalServerError, err
		}
		for _, st := range []runtimedb.PlanState{runtimedb.PlanApproved, runtimedb.PlanApplying} {
			if err := s.DB.TransitionPlan(planID, st, id.Name); err != nil {
				return nil, http.StatusInternalServerError, err
			}
		}
		if _, failure := s.executePublisherMint(r, next, req.Node, secretName, dependents); failure != nil {
			if terr := s.DB.TransitionPlan(planID, runtimedb.PlanFailed, id.Name); terr != nil {
				s.Log.Error("runtime credential plan transition", "plan", planID, "err", terr)
			}
			s.appendEvent("runtime.mint-failed", req.Node, secretName, id.Name)
			// The token never reached the node and was revoked; the
			// predecessor is still the working credential, so it stays
			// current and the successor is closed off. Nothing was
			// rotated, and the record says so.
			if err := s.DB.SetPrincipalDisabled(next, true); err != nil {
				return nil, http.StatusInternalServerError, err
			}
			if len(previous) > 0 {
				if err := s.DB.BindRuntimePrincipal(previous[len(previous)-1], b.Seq); err != nil {
					return nil, http.StatusInternalServerError, err
				}
			}
			return nil, http.StatusBadGateway, fmt.Errorf("%s (plan %s)", failure, planID)
		}
		if terr := s.DB.TransitionPlan(planID, runtimedb.PlanApplied, id.Name); terr != nil {
			s.Log.Error("runtime credential plan transition", "plan", planID, "err", terr)
		}
		s.appendEvent("runtime.minted", req.Node, secretName, id.Name)
		if len(dependents) > 0 && s.OnApproved != nil {
			s.OnApproved()
		}
		out.Node, out.SecretName, out.Plan = req.Node, "secret/"+secretName, planID
	}

	// The successor holds; now the predecessors do not. In this order
	// so a mint that failed above left the Agent with the credential it
	// had rather than none.
	for _, old := range previous {
		if err := s.DB.SetPrincipalDisabled(old, true); err != nil {
			return nil, http.StatusInternalServerError, fmt.Errorf("disabling %s: %w", old, err)
		}
		if _, err := s.DB.RevokeTokensFor(old); err != nil {
			return nil, http.StatusInternalServerError, err
		}
	}
	// Health: the native runner is in-process and a fresh credential
	// brings a revoked one back, since re-registering is what a new
	// credential is for it. Any other runtime has to present the
	// credential before it is anything but unregistered.
	switch {
	case rt.Type == agentapi.RuntimeNative && rt.Health == agentapi.HealthRevoked:
		err = s.DB.SetRuntimeHealth(b.Seq, agentapi.HealthConnected)
	case rt.Type != agentapi.RuntimeNative:
		err = s.DB.SetRuntimeHealth(b.Seq, agentapi.HealthUnregistered)
	}
	if err != nil {
		return nil, http.StatusInternalServerError, err
	}
	s.event("butler.runtime", "", id, fmt.Sprintf("runtime credential of %s rotated to %s; %d predecessor(s) disabled",
		b.Name, next, len(previous)))
	return out, http.StatusOK, nil
}

// revokeButlerRuntime is POST /api/v1/butlers/{name}/runtime/revoke:
// §3.7's immediate revocation, from the Console. Every credential the
// Agent's runtime ever held stops working from the next request and the
// runtime is marked revoked; the Agent itself — people, tools,
// Connections, routines, memory — is untouched, and a rotation issues
// it a new credential when the Owner is ready.
//
// CONSEQUENTIAL (A4.1): it withdraws authority, and nothing that ran as
// the runtime can act again until somebody decides it may.
func (s *Server) revokeButlerRuntime(r *http.Request, id principal.Identity) (any, int, error) {
	name := r.PathValue("name")
	b, _, code, err := s.liveAgentRuntime(name)
	if err != nil {
		return nil, code, err
	}
	if err := s.revokeAgentRuntime(b); err != nil {
		return nil, http.StatusInternalServerError, err
	}
	s.event("butler.runtime", "", id, fmt.Sprintf("runtime of %s revoked; every runtime credential disabled", b.Name))
	rt, err := s.DB.GetButlerRuntime(b.Seq)
	if err != nil {
		return nil, http.StatusInternalServerError, err
	}
	return rt.Record(), http.StatusOK, nil
}
