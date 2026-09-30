package api

// Rendering a Personal connection's adapter placement into desired state
// (epic #326; ADR-W030 A5.2, A5.4, A5.5).
//
// capability.Engine.ExpandAdapter renders one adapter for one connection
// on one node. This is what decides WHEN that rendering is desired and
// gets it there, and it goes where every other change to host state
// goes: a Plan, through the one proposal pipeline, that a human Owner
// approves. Nothing here writes the Statefile.
//
// An adapter is desired on a placement node only while ALL of these hold
// for its connection:
//
//   - an installed package declares the connection's adapter, and the
//     connection's recorded executable is that package's program at that
//     version (a Person's authorization is bound to it);
//   - every credential the adapter declares has an active version;
//   - the owning Person's background authorization is live, bound to
//     that executable, and the subscription it names is approved and
//     enabled; and
//   - the node's own adapter principal exists and is not disabled.
//
// When any stops holding — the credential is revoked or replaced, the
// authorization revoked, the subscription disabled, the Person
// offboarded — the same computation yields "nothing desired", and the
// Plan it files removes the resources. S02 has already stopped the
// containers by then (a running adapter holds the credential in its
// environment); the Plan brings desired state into line, and until it is
// approved the reconciler cannot bring the old container back, because
// the credential secret it declares is erased and a missing provided
// secret holds its owner from converging.
//
// Each lifecycle handler re-files this after it changes one of those
// facts, superseding any earlier render Plan for the connection still
// awaiting a decision, so an Owner is never asked to approve a rendering
// that no longer matches the connection.
//
// Installing a package that bundles an adapter renders nothing: there is
// no connection until a Person creates one, and this runs per connection.

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"

	"github.com/chickenrat17/wildstone-homelab/keystone/capability"
	"github.com/chickenrat17/wildstone-homelab/keystone/pkgspec"
	"github.com/chickenrat17/wildstone-homelab/keystone/principal"
	"github.com/chickenrat17/wildstone-homelab/keystone/resource"
	"github.com/chickenrat17/wildstone-homelab/keystone/runtimedb"
)

// adapterRenderPrincipal files the render Plans a lifecycle change
// triggers. It never approves anything: every Plan it files waits for an
// Owner like any other.
const adapterRenderPrincipal = "keystone"

// adapterPlanPrefix starts the summary of every render Plan for one
// connection; it is how an earlier undecided one is found and
// superseded.
func adapterPlanPrefix(connection string) string {
	return "adapter placement for integration/" + connection + ": "
}

// adapterRender is what one connection's placement should be now.
type adapterRender struct {
	Connection string
	Domain     string
	Package    *pkgspec.Package
	Program    string
	Executable string
	// Held: the rendering cannot be decided at all (no catalog, no
	// installed package, no address a node reaches Keystone at). Nothing
	// is proposed either way.
	Held bool
	// Eligible: the adapter is desired on Nodes. Otherwise Reason says
	// why not, and whatever is rendered is proposed for removal.
	Eligible bool
	Reason   string
	Nodes    []string
	// Waiting are placement nodes that have no adapter principal yet; an
	// Owner mints one by rendering the placement.
	Waiting []string
	Put     []resource.Resource
	Delete  []deletionRef
}

// adapterDomainOf is the integration domain a connection's adapter
// field names: "wild-mail-adapter" (the program) or "mail" (the domain).
func adapterDomainOf(adapter string) string {
	if d := adapterDomain(adapter); d != "" {
		return d
	}
	return adapter
}

// adapterPackage finds the one INSTALLED package that declares the
// connection's adapter.
func (s *Server) adapterPackage(conn runtimedb.IntegrationConnection, current []resource.Resource) (*pkgspec.Package, string, string, error) {
	if s.Catalog == nil {
		return nil, "", "", errors.New("no catalog is configured, so no adapter can be rendered")
	}
	domain := adapterDomainOf(conn.Adapter)
	var found []*pkgspec.Package
	var program string
	for _, pkg := range s.Catalog.List() {
		in, ok := pkg.AdapterFor(domain)
		if !ok {
			continue
		}
		if strings.HasPrefix(conn.Adapter, "wild-") && in.Adapter.Program != conn.Adapter {
			continue
		}
		if !packageIsInstalled(pkg, current) {
			continue
		}
		found = append(found, pkg)
		program = in.Adapter.Program
	}
	switch len(found) {
	case 0:
		return nil, "", "", fmt.Errorf("no installed package declares the %s adapter %q", domain, conn.Adapter)
	case 1:
		return found[0], domain, program, nil
	default:
		names := make([]string, 0, len(found))
		for _, p := range found {
			names = append(names, p.Metadata.Name)
		}
		sort.Strings(names)
		return nil, "", "", fmt.Errorf("more than one installed package declares the %s adapter (%s); refusing to guess",
			domain, strings.Join(names, ", "))
	}
}

// adapterExecutable is the executable a Person's authorization binds:
// "<package>@<version>/<program>".
func adapterExecutable(pkg *pkgspec.Package, program string) string {
	return fmt.Sprintf("%s@%s/%s", pkg.Metadata.Name, pkg.Metadata.Version, program)
}

// renderAdapter computes what the connection's placement should be and
// how desired state must change to get there. It writes nothing.
func (s *Server) renderAdapter(name string) (adapterRender, error) {
	conn, err := s.DB.GetConnection(name)
	if err != nil {
		return adapterRender{}, err
	}
	out := adapterRender{Connection: name}
	hold := func(format string, args ...any) (adapterRender, error) {
		out.Held, out.Reason = true, fmt.Sprintf(format, args...)
		return out, nil
	}
	if conn.Ownership != runtimedb.OwnershipPersonal {
		return hold("%s is not a Personal connection; only a Personal connection's adapter is rendered here", name)
	}
	current, err := s.State.Load()
	if err != nil {
		return adapterRender{}, fmt.Errorf("loading statefile: %w", err)
	}
	pkg, domain, program, err := s.adapterPackage(conn, current)
	if err != nil {
		return hold("%v", err)
	}
	out.Package, out.Domain, out.Program = pkg, domain, program
	out.Executable = adapterExecutable(pkg, program)

	owner := capability.AdapterOwner(name)
	var existing []resource.Resource
	for _, r := range current {
		if r.Metadata.Owner == owner {
			existing = append(existing, r)
		}
	}
	desired, reason, held, err := s.desiredAdapter(conn, &out)
	if err != nil {
		return adapterRender{}, err
	}
	if held != "" {
		return hold("%s", held)
	}
	out.Eligible, out.Reason = reason == "", reason
	// A rendered name another owner already holds is somebody else's
	// resource; proposing over it would take it.
	for _, d := range desired {
		for _, c := range current {
			if c.Kind == d.Kind && c.Metadata.Name == d.Metadata.Name && c.Metadata.Owner != owner {
				return hold("%s/%s is already declared by %s", d.Kind, d.Metadata.Name, c.Metadata.Owner)
			}
		}
	}
	out.Put, out.Delete, err = diffAdapter(existing, desired)
	if err != nil {
		return adapterRender{}, err
	}
	return out, nil
}

// desiredAdapter renders the adapter on every node it is desired on.
// reason is why it is desired nowhere; held is why nothing can be
// decided.
func (s *Server) desiredAdapter(conn runtimedb.IntegrationConnection, out *adapterRender) (desired []resource.Resource, reason, held string, err error) {
	if conn.Executable != out.Executable {
		return nil, fmt.Sprintf("the recorded adapter executable %q is not the installed %q; an Owner renders the placement to record it",
			conn.Executable, out.Executable), "", nil
	}
	in, _ := out.Package.AdapterFor(out.Domain)
	env, secrets, err := s.DB.AdapterPlacementEnv(conn.Name)
	if err != nil {
		return nil, "", "", err
	}
	for _, need := range in.Adapter.Needs.Secrets {
		if secrets[need.Name] == "" {
			return nil, fmt.Sprintf("%s has no active %s", conn.Name, need.Name), "", nil
		}
	}
	a, err := s.DB.LiveBackgroundAuthorization(conn.Name)
	switch {
	case errors.Is(err, runtimedb.ErrNotFound):
		return nil, fmt.Sprintf("%s has not authorized unattended use of %s", conn.Owner, conn.Name), "", nil
	case err != nil:
		return nil, "", "", err
	}
	if a.Executable != conn.Executable {
		return nil, "the live authorization was given for another adapter executable", "", nil
	}
	sub, err := s.DB.GetAgentSubscription(a.Subscription)
	if err != nil {
		return nil, fmt.Sprintf("the subscription the authorization names (%d) does not exist", a.Subscription), "", nil
	}
	if !sub.Enabled || sub.ApprovedVersion == 0 {
		return nil, fmt.Sprintf("subscription %d is not approved and enabled", sub.ID), "", nil
	}
	placements, err := s.DB.ListPlacements(conn.Name)
	if err != nil {
		return nil, "", "", err
	}
	var nodes []string
	for _, p := range placements {
		if !p.Removing {
			nodes = append(nodes, p.Node)
		}
	}
	sort.Strings(nodes)
	if len(nodes) == 0 {
		return nil, fmt.Sprintf("%s has no placement node", conn.Name), "", nil
	}
	keystoneURL := s.gatherFacts().keystoneURL
	if keystoneURL == "" {
		return nil, "", "Keystone does not know an address a node can reach it at; start it with -api-listen on the tailnet or LAN address", nil
	}
	params := map[string]string{}
	if _, installed := s.installedFromPlans(); installed[out.Package.Metadata.Name] != nil {
		params = installed[out.Package.Metadata.Name]
	}
	for _, node := range nodes {
		p, perr := s.DB.GetPrincipal(capability.AdapterPrincipalName(out.Domain, conn.Name, node))
		if perr != nil || p.Disabled || p.Role != runtimedb.RoleIntegrationAdapter {
			out.Waiting = append(out.Waiting, node)
			continue
		}
		rendered, rerr := capability.NewEngine().ExpandAdapter(out.Package, params, out.Domain, capability.AdapterPlacement{
			Connection: conn.Name, Node: node, KeystoneURL: keystoneURL,
			TokenSecret: capability.AdapterTokenSecretName(out.Domain, conn.Name, node),
			Secrets:     secrets, Env: env,
		})
		if rerr != nil {
			return nil, "", rerr.Error(), nil
		}
		out.Nodes = append(out.Nodes, node)
		desired = append(desired, rendered...)
	}
	if len(out.Nodes) == 0 {
		return nil, "no placement node has an adapter token yet; an Owner renders the placement to mint one", "", nil
	}
	return desired, "", "", nil
}

// diffAdapter is what turns existing into desired: changed or new
// resources to put, and resources no longer desired to delete.
func diffAdapter(existing, desired []resource.Resource) ([]resource.Resource, []deletionRef, error) {
	type key struct {
		kind resource.Kind
		name string
	}
	have := map[key][]byte{}
	for _, r := range existing {
		b, err := resource.Marshal(r)
		if err != nil {
			return nil, nil, err
		}
		have[key{r.Kind, r.Metadata.Name}] = b
	}
	want := map[key]bool{}
	var put []resource.Resource
	for _, d := range desired {
		k := key{d.Kind, d.Metadata.Name}
		want[k] = true
		b, err := resource.Marshal(d)
		if err != nil {
			return nil, nil, err
		}
		if old, ok := have[k]; !ok || !bytes.Equal(old, b) {
			put = append(put, d)
		}
	}
	var del []deletionRef
	for _, r := range existing {
		if !want[key{r.Kind, r.Metadata.Name}] {
			del = append(del, deletionRef{Kind: string(r.Kind), Name: r.Metadata.Name})
		}
	}
	sort.Slice(del, func(i, j int) bool {
		if del[i].Kind != del[j].Kind {
			return del[i].Kind < del[j].Kind
		}
		return del[i].Name < del[j].Name
	})
	return put, del, nil
}

// fileAdapterRender files the Plan that brings the connection's adapter
// placement to what renderAdapter says, and supersedes every earlier
// undecided render Plan for it. It never approves.
func (s *Server) fileAdapterRender(name string, id principal.Identity, dryRun bool) (map[string]any, int, error) {
	rd, err := s.renderAdapter(name)
	if errors.Is(err, runtimedb.ErrNotFound) {
		return nil, http.StatusNotFound, fmt.Errorf("no connection named %q", name)
	}
	if err != nil {
		return nil, http.StatusInternalServerError, err
	}
	out := map[string]any{"connection": name, "eligible": rd.Eligible}
	if rd.Executable != "" {
		out["executable"] = rd.Executable
	}
	if rd.Reason != "" {
		out["reason"] = rd.Reason
	}
	if len(rd.Nodes) > 0 {
		out["nodes"] = rd.Nodes
	}
	if len(rd.Waiting) > 0 {
		out["waitingForToken"] = rd.Waiting
	}
	if rd.Held {
		out["held"] = true
		return out, http.StatusOK, nil
	}
	filed := ""
	if len(rd.Put) > 0 || len(rd.Delete) > 0 {
		docs := make([]string, 0, len(rd.Put))
		for _, r := range rd.Put {
			b, err := resource.Marshal(r)
			if err != nil {
				return nil, http.StatusInternalServerError, err
			}
			docs = append(docs, string(b))
		}
		summary := adapterPlanPrefix(name)
		if rd.Eligible {
			summary += fmt.Sprintf("run the %s adapter (%s) on %s", rd.Domain, rd.Executable, strings.Join(rd.Nodes, ", "))
		} else {
			summary += "remove the " + rd.Domain + " adapter: " + rd.Reason
		}
		view, code, err := s.submitProposalOpts(id, proposalRequest{Summary: summary, Put: docs, Delete: rd.Delete},
			proposalOpts{DryRun: dryRun})
		if err != nil {
			return nil, code, err
		}
		out["plan"] = view
		if v, ok := view.(planView); ok {
			filed = v.ID
		}
	} else {
		out["unchanged"] = true
	}
	if !dryRun {
		s.supersedeAdapterPlans(name, filed)
	}
	return out, http.StatusOK, nil
}

// supersedeAdapterPlans retires every undecided render Plan for the
// connection except keep: the connection has moved since they were
// computed, so approving one would render something nobody now intends.
func (s *Server) supersedeAdapterPlans(connection, keep string) {
	plans, err := s.DB.ListPlans(500)
	if err != nil {
		s.Log.Error("listing plans to supersede adapter renders", "connection", connection, "err", err)
		return
	}
	prefix := adapterPlanPrefix(connection)
	for _, p := range plans {
		if p.State != runtimedb.PlanProposed || p.ID == keep || !strings.HasPrefix(p.Summary, prefix) {
			continue
		}
		if err := s.DB.TransitionPlan(p.ID, runtimedb.PlanSuperseded, ""); err != nil {
			s.Log.Error("superseding an adapter render plan", "plan", p.ID, "err", err)
			continue
		}
		s.event("plan.superseded", p.ID, principal.Identity{Name: adapterRenderPrincipal}, "the connection changed since this adapter rendering was proposed")
	}
}

// refileAdapterRender is what a lifecycle handler calls after changing a
// fact the rendering depends on. Best effort: the change it follows has
// already committed, and a render that cannot be filed is logged and
// filed again by the next change or an Owner's explicit render.
func (s *Server) refileAdapterRender(connection, actor string) {
	if connection == "" || s.Catalog == nil || s.State == nil {
		return
	}
	id := principal.Identity{Name: adapterRenderPrincipal, ActingFor: actor}
	if _, _, err := s.fileAdapterRender(connection, id, false); err != nil {
		s.Log.Warn("adapter placement could not be re-rendered", "connection", connection, "err", err)
	}
}

// renderIntegrationAdapter is POST /api/v1/integrations/{name}/adapter/render:
// an Owner prepares a Personal connection's adapter placement. It records
// the installed package's adapter executable (a change revokes the
// Person's authorization, which is bound to it), places a node-bound
// adapter token on each placement node that has none, and files the
// render Plan — which it does not approve.
func (s *Server) renderIntegrationAdapter(r *http.Request, id principal.Identity) (any, int, error) {
	if code, err := planDecider(id); err != nil {
		return nil, code, err
	}
	name := r.PathValue("name")
	var req struct {
		DryRun bool `json:"dryRun"`
		// RotateTokens re-mints every node's adapter token.
		RotateTokens bool `json:"rotateTokens"`
	}
	if r.ContentLength != 0 {
		if code, err := decodeStrict(r, &req); err != nil {
			return nil, code, err
		}
	}
	conn, err := s.DB.GetConnection(name)
	if err != nil {
		return nil, http.StatusNotFound, fmt.Errorf("no connection named %q", name)
	}
	if conn.Ownership != runtimedb.OwnershipPersonal {
		return nil, http.StatusBadRequest, fmt.Errorf("%s is not a Personal connection", name)
	}
	current, err := s.State.Load()
	if err != nil {
		return nil, http.StatusInternalServerError, fmt.Errorf("loading statefile: %w", err)
	}
	pkg, domain, program, err := s.adapterPackage(conn, current)
	if err != nil {
		return nil, http.StatusConflict, err
	}
	exe := adapterExecutable(pkg, program)
	extra := map[string]any{}
	if conn.Executable != exe {
		if req.DryRun {
			extra["executableWouldBe"] = exe
		} else {
			if err := s.DB.SetConnectionExecutable(name, exe, actingMember(id)); err != nil {
				return nil, credentialStatus(err), err
			}
			extra["executableRecorded"] = exe
			if conn.Executable != "" {
				extra["note"] = fmt.Sprintf("the adapter changed from %s, so %s's authorization for it was revoked; "+
					"they authorize the new one before it runs unattended", conn.Executable, conn.Owner)
			}
		}
	}
	if !req.DryRun {
		minted, code, err := s.ensureAdapterTokens(r, conn, domain, id, req.RotateTokens)
		if err != nil {
			return nil, code, err
		}
		if len(minted) > 0 {
			extra["tokensPlaced"] = minted
		}
	}
	out, code, err := s.fileAdapterRender(name, id, req.DryRun)
	if err != nil {
		return nil, code, err
	}
	for k, v := range extra {
		out[k] = v
	}
	return out, code, nil
}

// ensureAdapterTokens gives every placement node its own node-bound
// adapter principal and places that principal's token in the node's
// secret store — the chat adapter's mint path, recorded as an applied
// secrets Plan because the Owner doing it is the approval. A node that
// already has a principal keeps its token unless rotate is set.
func (s *Server) ensureAdapterTokens(r *http.Request, conn runtimedb.IntegrationConnection, domain string, id principal.Identity, rotate bool) ([]string, int, error) {
	if s.Agents == nil || s.Principals == nil {
		return nil, http.StatusServiceUnavailable, fmt.Errorf("the node agent plane is not wired, so no adapter token can be placed")
	}
	placements, err := s.DB.ListPlacements(conn.Name)
	if err != nil {
		return nil, http.StatusInternalServerError, err
	}
	var minted []string
	for _, p := range placements {
		if p.Removing {
			continue
		}
		name := capability.AdapterPrincipalName(domain, conn.Name, p.Node)
		secret := capability.AdapterTokenSecretName(domain, conn.Name, p.Node)
		existing, err := s.DB.GetPrincipal(name)
		switch {
		case errors.Is(err, runtimedb.ErrNotFound):
			if err := s.Principals.Create(name, runtimedb.RoleIntegrationAdapter, ""); err != nil {
				return minted, http.StatusBadRequest, err
			}
			s.event("principal.created", "", id, fmt.Sprintf("%s (%s, for %s on %s)", name,
				runtimedb.RoleIntegrationAdapter, conn.Name, p.Node))
		case err != nil:
			return minted, http.StatusInternalServerError, err
		case existing.Role != runtimedb.RoleIntegrationAdapter:
			return minted, http.StatusConflict, fmt.Errorf("principal %s exists with role %s, not %s",
				name, existing.Role, runtimedb.RoleIntegrationAdapter)
		case existing.Disabled:
			return minted, http.StatusConflict, fmt.Errorf("adapter principal %s is disabled; it is not revived by a render", name)
		case !rotate:
			continue
		}
		if err := s.DB.BindAdapterPrincipalOnNode(name, conn.Name, p.Node, actingMember(id)); err != nil {
			return minted, http.StatusBadRequest, err
		}
		dependents, err := s.secretDependents(p.Node, secret)
		if err != nil {
			return minted, http.StatusInternalServerError, err
		}
		summary := fmt.Sprintf("mint the %s adapter token for %s on %s → secret/%s (%d dependent container(s) re-created)",
			domain, conn.Name, p.Node, secret, len(dependents))
		planID, err := s.recordAppliedSecretsPlan(id, summary, map[string]any{
			"secret": secret, "node": p.Node, "principal": name, "connection": conn.Name, "dependents": dependents})
		if err != nil {
			return minted, http.StatusInternalServerError, err
		}
		if _, failure := s.executePublisherMint(r, name, p.Node, secret, dependents); failure != nil {
			if terr := s.DB.TransitionPlan(planID, runtimedb.PlanFailed, id.Name); terr != nil {
				s.Log.Error("adapter token plan transition", "plan", planID, "err", terr)
			}
			return minted, http.StatusBadGateway, fmt.Errorf("placing %s's token on %s: %s (plan %s)", name, p.Node, failure, planID)
		}
		if terr := s.DB.TransitionPlan(planID, runtimedb.PlanApplied, id.Name); terr != nil {
			s.Log.Error("adapter token plan transition", "plan", planID, "err", terr)
		}
		minted = append(minted, p.Node)
	}
	return minted, http.StatusOK, nil
}

// recordAppliedSecretsPlan records a secrets-path act the Owner is
// performing right now (the chat adapter mint's shape): approved by the
// act itself, applying until the caller marks it applied or failed.
func (s *Server) recordAppliedSecretsPlan(id principal.Identity, summary string, detail map[string]any) (string, error) {
	detail["summary"] = summary
	stored, err := json.Marshal(detail)
	if err != nil {
		return "", err
	}
	planID, err := s.DB.InsertPlan(secretsPrincipal, id.Name, summary, "medium", string(stored))
	if err != nil {
		return "", err
	}
	for _, st := range []runtimedb.PlanState{runtimedb.PlanApproved, runtimedb.PlanApplying} {
		if err := s.DB.TransitionPlan(planID, st, id.Name); err != nil {
			return "", err
		}
	}
	return planID, nil
}
