package api

// "Watch this mailbox": the setup read model and the one filing step the
// Person's side of the flow was missing (epic #326 S07, #460).
//
// S02 gave a Person the credential intake and the authorization, and S03
// the subscription's health. What joined them was a test calling the
// store directly: nothing on the public API let the mailbox's owner file
// the subscription their authorization names, and nothing told a setup
// screen, after a refresh, where the setup had got to. Both are here,
// and neither decides anything:
//
//   - GET  .../mail/setup is a read model assembled from rows other
//     handlers already own — the inherited provider settings, the
//     caller's OWN Personal mail connections with their credential,
//     authorization, subscription and pending Plans, the Agents the
//     caller may use and the calendars they may read. A setup screen
//     resumes from it; nothing about a setup lives in the browser.
//   - POST .../integrations/{name}/subscription files, for the
//     connection's owning Person only, the scheduled mail.changes
//     subscription that names the exact authorization they previewed.
//     It is a Plan an Owner approves like any other. Filing it again
//     after an edit revises the existing subscription (which stops it
//     dispatching at once) rather than adding a second one.
//
// Owner approval and the Person's authorization stay two artifacts: this
// files the first's Plan, and the Person still authorizes afterwards,
// naming the subscription the approval created.

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/chickenrat17/wildstone-homelab/keystone/mailmonitor"
	"github.com/chickenrat17/wildstone-homelab/keystone/pkgspec"
	"github.com/chickenrat17/wildstone-homelab/keystone/plan"
	"github.com/chickenrat17/wildstone-homelab/keystone/principal"
	"github.com/chickenrat17/wildstone-homelab/keystone/proposals"
	"github.com/chickenrat17/wildstone-homelab/keystone/resource"
	"github.com/chickenrat17/wildstone-homelab/keystone/runtimedb"
)

// mailDomain is the integration domain a mail connection's adapter
// serves.
const mailDomain = "mail"

// maxConflictCalendars bounds the calendars a monitor may check for
// conflicts. A household has a handful.
const maxConflictCalendars = 20

// The match_json keys a Watch-this-mailbox subscription carries beside
// BackgroundSubscriptionMatch's two. They are what an Owner reads when
// approving it, and what a setup screen resumes the form from; the
// authorization digest already binds every one that is material to the
// Person's authorization, and the conflict calendars are bound by the
// subscription's own content digest.
const (
	mailMatchAgent              = "agent"
	mailMatchScope              = "scope"
	mailMatchProcessingLocation = "processingLocation"
	mailMatchTargetCalendar     = "targetCalendar"
	mailMatchConflictCalendars  = "conflictCalendars"
)

// mailProvider is the provider endpoint the installed mail package
// configured, as the adapter inherits it. Never a credential: pkgspec
// refuses to let an adapter inherit one.
type mailProvider struct {
	Package  string            `json:"package"`
	Version  string            `json:"version"`
	Settings map[string]string `json:"settings"`
}

// mailCalendar is one calendar the caller may read, and whether they may
// write it (a target calendar must be writable).
type mailCalendar struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Writable bool   `json:"writable"`
}

type mailAgent struct {
	Seq  int64  `json:"seq"`
	Name string `json:"name"`
}

// mailPendingPlan is an undecided Plan the setup is waiting on.
type mailPendingPlan struct {
	ID      string `json:"id"`
	Kind    string `json:"kind"` // "subscription" | "render"
	Summary string `json:"summary"`
	// Match is a pending subscription Plan's match, so a screen can
	// resume the form it was filed from.
	Match json.RawMessage `json:"match,omitempty"`
}

// mailMonitorView is one of the caller's own mail connections, with
// everything a setup or health screen needs about it and nothing that
// could carry a credential value.
type mailMonitorView struct {
	Connection    runtimedb.IntegrationConnection    `json:"connection"`
	Nodes         []string                           `json:"nodes"`
	Credential    *runtimedb.PersonalCredential      `json:"credential,omitempty"`
	Authorization *runtimedb.BackgroundAuthorization `json:"authorization,omitempty"`
	Subscriptions []subscriptionView                 `json:"subscriptions"`
	PendingPlans  []mailPendingPlan                  `json:"pendingPlans"`
	// UnverifiedErasures are copies of a fenced credential whose
	// erasure a node has not proven: the connection is degraded
	// (erasure_unverified) until an Owner's retry proves them gone.
	UnverifiedErasures []runtimedb.CredentialCopy `json:"unverifiedErasures,omitempty"`
}

// mailSetupView is GET .../mail/setup.
type mailSetupView struct {
	Member    string            `json:"member"`
	Owner     bool              `json:"owner"`
	Provider  *mailProvider     `json:"provider,omitempty"`
	Monitors  []mailMonitorView `json:"monitors"`
	Agents    []mailAgent       `json:"agents"`
	Calendars []mailCalendar    `json:"calendars"`
	Cadence   struct {
		Default string `json:"default"`
		Min     string `json:"min"`
		Max     string `json:"max"`
	} `json:"cadence"`
	CalendarsError string `json:"calendarsError,omitempty"`
}

// ---------------------------------------------------------------- routes

func (s *Server) consoleMailSetup(r *http.Request, id principal.Identity) (any, int, error) {
	member := actingMember(id)
	desired, err := s.State.Load()
	if err != nil {
		return nil, http.StatusInternalServerError, err
	}
	groups, err := s.actorCalendarGroups(desired, member)
	if err != nil {
		return nil, http.StatusInternalServerError, err
	}
	return s.mailSetup(member, groups, desired)
}

func (s *Server) homeMailSetup(_ http.ResponseWriter, r *http.Request, member string, desired []resource.Resource) (any, int, error) {
	return s.mailSetup(member, forwardedGroups(r), desired)
}

func (s *Server) consoleFileMailSubscription(r *http.Request, id principal.Identity) (any, int, error) {
	member := actingMember(id)
	desired, err := s.State.Load()
	if err != nil {
		return nil, http.StatusInternalServerError, err
	}
	groups, err := s.actorCalendarGroups(desired, member)
	if err != nil {
		return nil, http.StatusInternalServerError, err
	}
	return s.fileMailSubscription(r, r.PathValue("name"), member, id, groups, desired)
}

func (s *Server) homeFileMailSubscription(_ http.ResponseWriter, r *http.Request, member string, desired []resource.Resource) (any, int, error) {
	// The portal door is a person, never a token: the Plan is filed by
	// Keystone acting for them, the render Plans' shape.
	id := principal.Identity{Name: adapterRenderPrincipal, ActingFor: member}
	return s.fileMailSubscription(r, r.PathValue("name"), member, id, forwardedGroups(r), desired)
}

// ---------------------------------------------------------------- read model

func (s *Server) mailSetup(member string, groups []string, desired []resource.Resource) (any, int, error) {
	out := mailSetupView{Member: member, Monitors: []mailMonitorView{}, Agents: []mailAgent{}, Calendars: []mailCalendar{}}
	out.Cadence.Default = runtimedb.DefaultSubscriptionCadence.String()
	out.Cadence.Min = runtimedb.MinSubscriptionCadence.String()
	out.Cadence.Max = runtimedb.MaxSubscriptionCadence.String()
	if m, err := s.DB.GetButlerMember(member); err == nil && m.Active() && m.Role == runtimedb.RoleOwner {
		out.Owner = true
	}
	out.Provider = s.mailProvider(desired)

	if butlers, err := s.DB.ButlersFor(member); err == nil {
		for _, b := range butlers {
			out.Agents = append(out.Agents, mailAgent{Seq: b.Seq, Name: b.Name})
		}
	}
	cals, err := s.mailCalendars(member, groups, desired)
	if err != nil {
		// The calendars being unreadable is not the setup being
		// unreadable: say so beside everything else.
		out.CalendarsError = err.Error()
	} else {
		out.Calendars = cals
	}

	conns, err := s.DB.ListConnections()
	if err != nil {
		return nil, http.StatusInternalServerError, err
	}
	var plans []runtimedb.PlanRecord
	for _, c := range conns {
		// The caller's OWN Personal mail connections, and nobody
		// else's — an Owner included: the household's monitors are on
		// the Integrations and Plans screens, not in anybody's setup.
		if c.Ownership != runtimedb.OwnershipPersonal || c.Owner != member || adapterDomainOf(c.Adapter) != mailDomain {
			continue
		}
		if plans == nil {
			if plans, err = s.DB.ListPlans(500); err != nil {
				return nil, http.StatusInternalServerError, err
			}
		}
		v, err := s.mailMonitor(c, plans)
		if err != nil {
			return nil, http.StatusInternalServerError, err
		}
		out.Monitors = append(out.Monitors, v)
	}
	return out, http.StatusOK, nil
}

// mailProvider is the installed mail package's inherited settings.
func (s *Server) mailProvider(desired []resource.Resource) *mailProvider {
	if s.Catalog == nil {
		return nil
	}
	_, installed := s.installedFromPlans()
	for _, pkg := range s.Catalog.List() {
		in, ok := pkg.AdapterFor(mailDomain)
		if !ok || !packageIsInstalled(pkg, desired) {
			continue
		}
		params := installed[pkg.Metadata.Name]
		settings := map[string]string{}
		for _, name := range in.Adapter.Inherits {
			v, set := params[name]
			if !set {
				v = paramDefault(pkg, name)
			}
			settings[name] = v
		}
		return &mailProvider{Package: pkg.Metadata.Name, Version: pkg.Metadata.Version, Settings: settings}
	}
	return nil
}

func paramDefault(pkg *pkgspec.Package, name string) string {
	for _, p := range pkg.Params {
		if p.Name == name {
			return p.Default
		}
	}
	return ""
}

// mailCalendars is every DECLARED calendar the member may read. A
// member's own personal calendars are not offered: an Agent never
// reaches one (ADR-W034 A9), so it could neither check nor write it.
func (s *Server) mailCalendars(member string, groups []string, desired []resource.Resource) ([]mailCalendar, error) {
	choices, err := s.calendarChoices(member, groups, desired)
	if err != nil {
		return nil, err
	}
	out := []mailCalendar{}
	for _, c := range choices {
		if c.Personal {
			continue
		}
		out = append(out, mailCalendar{ID: c.ID, Name: c.Name, Writable: c.Writable})
	}
	return out, nil
}

// actorCalendarGroups is the directory groups a member holds for
// calendar access when there is no proxy header to carry them: declared
// directory groups, and the groups of the services their Person is
// entitled to. CalendarForActor's resolution, shared.
func (s *Server) actorCalendarGroups(desired []resource.Resource, member string) ([]string, error) {
	groups := []string{}
	for _, r := range desired {
		if r.LdapUser != nil && r.LdapUser.Username == member {
			groups = append(groups, r.LdapUser.Groups...)
		}
	}
	if s.DB == nil {
		return groups, nil
	}
	person, ambiguous, err := s.DB.PersonForDirectoryAccount(member)
	if err != nil {
		return nil, err
	}
	if ambiguous || person == "" {
		return groups, nil
	}
	services, err := s.DB.EntitledServices(person)
	if err != nil {
		return nil, err
	}
	for _, r := range desired {
		if r.LdapGroup != nil {
			for _, service := range services {
				if r.Metadata.Owner == "capability/"+service {
					groups = append(groups, r.LdapGroup.Name)
				}
			}
		}
	}
	return groups, nil
}

func (s *Server) mailMonitor(c runtimedb.IntegrationConnection, plans []runtimedb.PlanRecord) (mailMonitorView, error) {
	v := mailMonitorView{Connection: c, Nodes: []string{}, Subscriptions: []subscriptionView{}, PendingPlans: []mailPendingPlan{}}
	placements, err := s.DB.ListPlacements(c.Name)
	if err != nil {
		return v, err
	}
	for _, p := range placements {
		if !p.Removing {
			v.Nodes = append(v.Nodes, p.Node)
		}
	}
	if cred, err := s.DB.ActivePersonalCredential(c.Name, runtimedb.MailboxCredentialNeed); err == nil {
		v.Credential = &cred
	} else if !errors.Is(err, runtimedb.ErrNoActiveCredential) {
		return v, err
	}
	if creds, err := s.DB.ListPersonalCredentials(c.Name); err == nil {
		for _, cr := range creds {
			if cr.Need != runtimedb.MailboxCredentialNeed {
				continue
			}
			for _, cp := range cr.Copies {
				if cp.EraseState == runtimedb.EraseUnverified {
					v.UnverifiedErasures = append(v.UnverifiedErasures, cp)
				}
			}
		}
	}
	if a, err := s.DB.LiveBackgroundAuthorization(c.Name); err == nil {
		v.Authorization = &a
	} else if !errors.Is(err, runtimedb.ErrNotFound) {
		return v, err
	}
	subs, err := s.DB.ListAllAgentSubscriptions()
	if err != nil {
		return v, err
	}
	for _, sub := range subs {
		if sub.SourceConnection != c.Name {
			continue
		}
		sv, err := s.viewSubscription(sub, true)
		if err != nil {
			return v, err
		}
		v.Subscriptions = append(v.Subscriptions, sv)
	}
	renderPrefix := adapterPlanPrefix(c.Name)
	for _, p := range plans {
		if p.State != runtimedb.PlanProposed {
			continue
		}
		if strings.HasPrefix(p.Summary, renderPrefix) {
			v.PendingPlans = append(v.PendingPlans, mailPendingPlan{ID: p.ID, Kind: "render", Summary: p.Summary})
			continue
		}
		sub, ok := s.pendingSubscriptionFor(p.ID, c.Name)
		if ok {
			v.PendingPlans = append(v.PendingPlans, mailPendingPlan{ID: p.ID, Kind: "subscription",
				Summary: p.Summary, Match: json.RawMessage(sub.MatchJSON)})
		}
	}
	return v, nil
}

// pendingSubscriptionFor reads an undecided Plan and reports the
// subscription change it proposes if that change is for connection.
func (s *Server) pendingSubscriptionFor(planID, connection string) (*plan.SubscriptionChange, bool) {
	rec, err := s.DB.GetPlan(planID)
	if err != nil || rec.State != runtimedb.PlanProposed {
		return nil, false
	}
	var stored storedProposal
	if json.Unmarshal([]byte(rec.PlanJSON), &stored) != nil || stored.Subscription == nil {
		return nil, false
	}
	sub := stored.Subscription
	source := sub.Source
	if sub.Revises != 0 {
		if row, err := s.DB.GetAgentSubscription(sub.Revises); err == nil {
			source = row.SourceConnection
		}
	}
	if source != connection {
		return nil, false
	}
	return sub, true
}

// ---------------------------------------------------------------- filing

// mailSubscriptionRequest is what the Person chose. Everything but the
// cadence and the conflict calendars is exactly the authorization
// request the digest is computed from.
type mailSubscriptionRequest struct {
	Agent              int64                     `json:"agent"`
	TargetCalendar     string                    `json:"targetCalendar"`
	Scope              runtimedb.BackgroundScope `json:"scope"`
	ProcessingLocation string                    `json:"processingLocation"`
	Cadence            string                    `json:"cadence,omitempty"`
	ConflictCalendars  []string                  `json:"conflictCalendars,omitempty"`
}

func (s *Server) fileMailSubscription(r *http.Request, name, member string, id principal.Identity, groups []string, desired []resource.Resource) (any, int, error) {
	conn, code, err := s.ownerOf(name, member)
	if err != nil {
		return nil, code, err
	}
	if adapterDomainOf(conn.Adapter) != mailDomain {
		return nil, http.StatusBadRequest, fmt.Errorf("%s is not a mail connection", name)
	}
	var req mailSubscriptionRequest
	if code, err := decodeStrict(r, &req); err != nil {
		return nil, code, err
	}
	// The same digest the Person's authorization will carry, computed
	// here rather than taken from the caller.
	a, err := s.DB.PrepareBackgroundAuthorization(member, runtimedb.BackgroundAuthorizationRequest{
		Connection: name, Scope: req.Scope, ProcessingLocation: req.ProcessingLocation,
		Agent: req.Agent, TargetCalendar: req.TargetCalendar,
	})
	if err != nil {
		return nil, credentialStatus(err), err
	}
	cals, err := s.mailCalendars(member, groups, desired)
	if err != nil {
		return nil, http.StatusServiceUnavailable, fmt.Errorf("your calendars could not be read, so none can be chosen: %w", err)
	}
	readable := map[string]mailCalendar{}
	for _, c := range cals {
		readable[c.ID] = c
	}
	if c, ok := readable[a.TargetCalendar]; !ok || !c.Writable {
		return nil, http.StatusForbidden, fmt.Errorf("%q is not a calendar you may add events to", a.TargetCalendar)
	}
	conflicts, err := cleanConflictCalendars(req.ConflictCalendars, readable)
	if err != nil {
		return nil, http.StatusBadRequest, err
	}
	agent, err := s.DB.GetButlerBySeq(a.Agent)
	if err != nil {
		return nil, http.StatusBadRequest, fmt.Errorf("no Agent %d", a.Agent)
	}

	match := map[string]any{}
	for k, v := range runtimedb.BackgroundSubscriptionMatch(name, a.Digest) {
		match[k] = v
	}
	match[mailMatchAgent] = a.Agent
	match[mailMatchScope] = a.Scope
	match[mailMatchProcessingLocation] = a.ProcessingLocation
	match[mailMatchTargetCalendar] = a.TargetCalendar
	match[mailMatchConflictCalendars] = conflicts
	raw, err := json.Marshal(match)
	if err != nil {
		return nil, http.StatusInternalServerError, err
	}
	matchJSON := string(raw)

	// A monitor has one subscription. Filing again revises it: the edit
	// takes effect on the row at once, so the old version stops
	// dispatching, and the Plan asks for the new one's approval.
	existing, err := s.mailSubscriptionOf(name)
	if err != nil {
		return nil, http.StatusInternalServerError, err
	}
	summary := fmt.Sprintf("%s watches the mailbox %s for events (%s)", member, name, agent.Name)
	var view proposals.PlanView
	var subID int64
	if existing != nil {
		if existing.Butler != a.Agent {
			return nil, http.StatusConflict, fmt.Errorf("this monitor runs %s; to change the Agent, "+
				"disable it and set the mailbox up again", s.butlerName(existing.Butler))
		}
		version, err := s.DB.ReviseAgentSubscription(existing.ID, runtimedb.AgentSubscriptionRevision{MatchJSON: &matchJSON}, member)
		if err != nil {
			if errors.Is(err, runtimedb.ErrQuarantined) {
				return nil, http.StatusServiceUnavailable, err
			}
			return nil, http.StatusBadRequest, err
		}
		edited, err := s.DB.GetAgentSubscription(existing.ID)
		if err != nil {
			return nil, http.StatusInternalServerError, err
		}
		cadence := ""
		if edited.Cadence > 0 {
			cadence = edited.Cadence.String()
		}
		view, err = proposals.FileSubscription(s.proposalDeps(), id, proposals.SubscriptionRequest{
			Butler: agent.Name, MatchKind: edited.MatchKind, MatchJSON: edited.MatchJSON, Audience: edited.Audience,
			CapabilitySet: edited.CapabilitySet, BudgetJSON: edited.BudgetJSON,
			Summary: fmt.Sprintf("%s edits the mailbox monitor for %s (version %d)", member, name, version),
			Trigger: edited.TriggerKind, Cadence: cadence, Source: name, Revises: existing.ID, Version: version,
		})
		if err != nil {
			return nil, http.StatusInternalServerError, fmt.Errorf("the edit was saved but its approval Plan could not be filed: %w", err)
		}
		subID = existing.ID
	} else {
		if req.Cadence != "" {
			if _, err := time.ParseDuration(req.Cadence); err != nil {
				return nil, http.StatusBadRequest, fmt.Errorf("cadence must be a duration like 15m")
			}
		}
		view, err = proposals.FileSubscription(s.proposalDeps(), id, proposals.SubscriptionRequest{
			Butler: agent.Name, MatchKind: mailmonitor.MatchKind, MatchJSON: matchJSON,
			Audience: runtimedb.AudienceOwner, CapabilitySet: []string{mailmonitor.MatchKind},
			Summary: summary, Trigger: runtimedb.SubscriptionTriggerSchedule, Cadence: req.Cadence, Source: name,
		})
		if err != nil {
			return nil, http.StatusBadRequest, err
		}
	}
	// An earlier undecided filing for this connection names a digest
	// nobody now intends; approving it would enable a monitor that the
	// Person's next authorization does not match.
	s.supersedeMailSubscriptionPlans(name, view.ID)
	out := map[string]any{
		"plan":              view,
		"subscriptionMatch": runtimedb.BackgroundSubscriptionMatch(name, a.Digest),
		"note": "filed for an Owner's approval. Nothing reads your mail until an Owner approves this and you " +
			"authorize the approved subscription; either one alone does nothing",
	}
	if subID != 0 {
		out["subscription"] = subID
		out["note"] = "the monitor stopped at once and waits for an Owner to approve the edit; authorize it again " +
			"to run under the new settings"
	}
	return out, http.StatusOK, nil
}

// cleanConflictCalendars bounds, de-duplicates and sorts the conflict
// calendars, refusing any the member may not read.
func cleanConflictCalendars(in []string, readable map[string]mailCalendar) ([]string, error) {
	if len(in) > maxConflictCalendars {
		return nil, fmt.Errorf("at most %d calendars can be checked for conflicts", maxConflictCalendars)
	}
	seen := map[string]bool{}
	out := []string{}
	for _, c := range in {
		c = strings.TrimSpace(c)
		if c == "" || seen[c] {
			continue
		}
		if strings.IndexFunc(c, unicode.IsControl) >= 0 {
			return nil, fmt.Errorf("a calendar name cannot contain control characters")
		}
		if _, ok := readable[c]; !ok {
			return nil, fmt.Errorf("%q is not a calendar you may read", c)
		}
		seen[c] = true
		out = append(out, c)
	}
	sort.Strings(out)
	return out, nil
}

// mailSubscriptionOf is the connection's current subscription: the
// newest one that is not disabled, else nil. A disabled monitor is set
// up afresh.
func (s *Server) mailSubscriptionOf(connection string) (*runtimedb.AgentSubscription, error) {
	subs, err := s.DB.ListAllAgentSubscriptions()
	if err != nil {
		return nil, err
	}
	var found *runtimedb.AgentSubscription
	for i := range subs {
		sub := subs[i]
		if sub.SourceConnection != connection || !sub.Enabled {
			continue
		}
		if found == nil || sub.ID > found.ID {
			found = &sub
		}
	}
	return found, nil
}

func (s *Server) butlerName(seq int64) string {
	if b, err := s.DB.GetButlerBySeq(seq); err == nil {
		return b.Name
	}
	return fmt.Sprintf("Agent %d", seq)
}

// supersedeMailSubscriptionPlans retires every undecided subscription
// Plan for the connection except keep.
func (s *Server) supersedeMailSubscriptionPlans(connection, keep string) {
	plans, err := s.DB.ListPlans(500)
	if err != nil {
		s.Log.Error("listing plans to supersede mail subscriptions", "connection", connection, "err", err)
		return
	}
	for _, p := range plans {
		if p.State != runtimedb.PlanProposed || p.ID == keep {
			continue
		}
		if _, ok := s.pendingSubscriptionFor(p.ID, connection); !ok {
			continue
		}
		if err := s.DB.TransitionPlan(p.ID, runtimedb.PlanSuperseded, ""); err != nil {
			s.Log.Error("superseding a mail subscription plan", "plan", p.ID, "err", err)
			continue
		}
		s.event("plan.superseded", p.ID, principal.Identity{Name: adapterRenderPrincipal},
			"the mailbox monitor was filed again with other settings")
	}
}
