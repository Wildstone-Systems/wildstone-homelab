// Package plan implements the Plan artifact (ADR-W004 A3): the typed
// object that serves UX, safety, and AI-governance for every intentional
// change to the Statefile. Diff produces an ordered, risk-graded Plan from
// desired vs. observed resources. No apply behavior lives here (S2b/S2c).
package plan

import (
	"encoding/json"
	"fmt"
	"reflect"
	"sort"

	"github.com/chickenrat17/wildstone-homelab/keystone/resource"
)

// RiskGrade is the pinned risk heuristic from ADR-W004 A3.
type RiskGrade int

const (
	RiskLow RiskGrade = iota
	RiskMedium
	RiskHigh
)

// MarshalJSON emits the grade's name, not its ordinal. The plan is the
// screen a human approves a change on (ADR-W004 A3), and `"Risk":0` tells
// that human nothing — the outer plan record has always spelled risk
// "Low", and the steps inside it now agree.
func (g RiskGrade) MarshalJSON() ([]byte, error) { return json.Marshal(g.String()) }

// UnmarshalJSON accepts both the name and the ordinal: plan records
// written before the named form are still in the runtime DB, and approval
// re-reads exactly that stored JSON.
func (g *RiskGrade) UnmarshalJSON(b []byte) error {
	var n int
	if err := json.Unmarshal(b, &n); err == nil {
		*g = RiskGrade(n)
		return nil
	}
	var name string
	if err := json.Unmarshal(b, &name); err != nil {
		return fmt.Errorf("plan: risk grade must be a name or an ordinal: %w", err)
	}
	for _, c := range []RiskGrade{RiskLow, RiskMedium, RiskHigh} {
		if c.String() == name {
			*g = c
			return nil
		}
	}
	return fmt.Errorf("plan: unknown risk grade %q", name)
}

func (g RiskGrade) String() string {
	switch g {
	case RiskLow:
		return "Low"
	case RiskMedium:
		return "Medium"
	case RiskHigh:
		return "High"
	default:
		return "Unknown"
	}
}

// Action is the kind of change a PlanStep makes.
type Action int

const (
	ActionNoOp Action = iota
	ActionCreate
	ActionUpdate
	ActionDelete
)

// MarshalJSON / UnmarshalJSON: see RiskGrade — same reasoning, same
// tolerance for ordinals already on disk.
func (a Action) MarshalJSON() ([]byte, error) { return json.Marshal(a.String()) }

func (a *Action) UnmarshalJSON(b []byte) error {
	var n int
	if err := json.Unmarshal(b, &n); err == nil {
		*a = Action(n)
		return nil
	}
	var name string
	if err := json.Unmarshal(b, &name); err != nil {
		return fmt.Errorf("plan: action must be a name or an ordinal: %w", err)
	}
	for _, c := range []Action{ActionNoOp, ActionCreate, ActionUpdate, ActionDelete} {
		if c.String() == name {
			*a = c
			return nil
		}
	}
	return fmt.Errorf("plan: unknown action %q", name)
}

func (a Action) String() string {
	switch a {
	case ActionNoOp:
		return "NoOp"
	case ActionCreate:
		return "Create"
	case ActionUpdate:
		return "Update"
	case ActionDelete:
		return "Delete"
	default:
		return "Unknown"
	}
}

// ResourceOp is a placeholder for the typed operation(s) that apply a
// PlanStep. It captures just enough (kind, name, action) to be useful for
// S2a's diff output and tests; the real Wardstone/provider op payloads are
// wired in by the S2b (Applier over gateway.Dispatch) and S2c (Provider)
// work packages, which will populate richer fields here or replace this
// type with one that embeds the concrete op.
type ResourceOp struct {
	Kind   resource.Kind `json:"kind"`
	Name   string        `json:"name"`
	Action Action        `json:"action"`
}

// PlanStep is one resource-level change within a Plan (ADR-W004 A3).
type PlanStep struct {
	// Description is a human one-liner, e.g. "restart Jellyfin (~2 min, no
	// data affected)".
	Description string        `json:"description"`
	Kind        resource.Kind `json:"kind"`
	Name        string        `json:"name"`
	Action      Action        `json:"action"`
	// Op is the typed operation(s) to apply this step. nil in S2a; S2b/S2c
	// populate it when wiring the reconcile engine.
	Op *ResourceOp `json:"op,omitempty"`
	// Reverse is the reverse op for rollback, where defined. nil means no
	// auto-reverse. nil in S2a for the same reason as Op.
	Reverse     *ResourceOp `json:"reverse,omitempty"`
	Risk        RiskGrade   `json:"risk"`
	BlastRadius string      `json:"blastRadius"`
}

// Plan is the artifact produced by Diff: an ordered list of steps plus an
// overall risk grade, the max over all step risks.
type Plan struct {
	ID          string     `json:"id,omitempty"`
	Summary     string     `json:"summary,omitempty"`
	Steps       []PlanStep `json:"steps"`
	OverallRisk RiskGrade  `json:"overallRisk"`
}

// kindOrder is the dependency order for creates/updates, per ADR-W004 A3
// plus the ADR-W005 S3a identity-wiring addendum. Deletes use the reverse
// order.
//
// ADR-W005 S3a ordering choice (flagged for planner review): LdapGroup and
// OidcClient are placed immediately after Volume and BEFORE Container —
// ahead of Route/DnsRecord as before, but also ahead of Container, which
// S2/S3's Volume->Container->Route->DnsRecord order did not need to
// consider. Rationale: an app Container's entrypoint/env may assume its
// identity wiring (its LDAP group, its registered OIDC client secret ref)
// already exists — e.g. Immich reading its OIDC client id/secret at
// startup — so identity resources should land before the container that
// depends on them, the same "dependency before dependent" logic that
// already puts Volume before Container. LdapGroup is ordered before
// OidcClient (declared group membership conceptually precedes registering
// an OIDC client that may reference group-based authorization), though the
// two rarely interact directly in S3's scope; this sub-ordering is a
// pragmatic default, not a proven dependency, and should be revisited if a
// package ever needs the reverse.
//
// ConfigFile sits with Volume, ahead of Container, for the same reason
// Volume does: a container mounts it, so it has to be on disk before the
// container that reads it starts. It was missing from this table
// entirely until the Authelia config landed, and the omission was not
// visible because the two readers of this map disagreed about what a
// missing kind means — the sort read the zero value and put ConfigFile
// FIRST, while KindOrder below returned len(kindOrder) and reported it
// LAST. Right answer, wrong reason, and a different wrong answer in the
// Butler's diagnose walker. Both now go through KindOrder, and
// TestEveryKindHasADeclaredOrder makes the omission impossible to repeat.
var kindOrder = map[resource.Kind]int{
	// Secret first, ahead of everything. A ConfigFile can carry a secret
	// reference the node resolves at write time, and a Container can
	// carry one in its env, so a secret has to exist before either. Note
	// that this position is necessary and NOT sufficient: the managed
	// files Wildstone writes before any plan step can carry references
	// too, which is why the reconciler ensures desired secrets before it
	// repairs those files (ensureNodeSecrets).
	resource.KindSecret: 0,
	// Ingress before everything: the base Caddyfile and every Route
	// fragment are rendered from its posture, so a node that gains an
	// ingress must have it recorded before anything reads it.
	resource.KindIngress:    0,
	resource.KindVolume:     1,
	resource.KindConfigFile: 2,
	// SeedFile with ConfigFile: after Volume, because it writes INTO one,
	// and before Container, because the whole point is to land before the
	// application first reads its config.
	resource.KindSeedFile:  2,
	resource.KindLdapGroup: 3,
	// LdapUser shares OidcClient's tier: after LdapGroup, because a
	// membership needs its group to exist, and before Container/Job,
	// because the provisioning job is what reaches the directory.
	resource.KindLdapUser:   4,
	resource.KindOidcClient: 4,
	// MatrixAppService with OidcClient, and for the same reason: it is
	// identity wiring the homeserver loads at start, so it is declared
	// before the Job that installs it and the Container that reads it.
	resource.KindMatrixAppService: 4,
	resource.KindContainer:        5,
	// Job after Container, because a job provisions a service that has
	// to be running to be provisioned. It is NOT a dependency edge and
	// deliberately not modelled as one: a job whose service is not ready
	// exits non-zero, observe sees that, and the next tick runs it
	// again. Level-triggered convergence is the ordering mechanism; this
	// position only saves the first tick from a guaranteed failure.
	resource.KindJob:       5,
	resource.KindRoute:     6,
	resource.KindDnsRecord: 7,
	// Automation after every node-applied kind: a rule may name a
	// capability's tool, so the capability lands first. Nothing depends
	// on a rule. Keystone-owned, so the position only orders the plan's
	// listing; there is no apply to sequence (ADR-W024 A1).
	resource.KindAutomation: 8,
	// Meter beside Automation and before it in the listing: a commitment
	// reads a meter, and a household reading a plan wants the number
	// declared before the thing that comes due against it. Keystone-owned,
	// so this orders the listing and nothing else (ADR-W035 A1).
	resource.KindMeter: 8,
	// Commitment after the Meter it reads, for the same reason: a
	// household reading a plan wants the number declared before the
	// thing that comes due against it.
	resource.KindCommitment: 9,
	// BackupPlan last: a backup declaration depends on the volumes it
	// names, and nothing depends on it (ADR-W011 A4).
	resource.KindBackupPlan: 9,
}

// key identifies a resource for matching desired against observed:
// matching is by (kind, name) per ADR-W004 A3.
type key struct {
	kind resource.Kind
	name string
}

// Diff produces an ordered, risk-graded Plan from desired vs. observed
// resources (ADR-W004 A3, level-triggered: diff(desired, observed) ->
// []PlanStep).
//
// Matching is by (kind, name). A resource present in both with an
// unchanged spec produces no step (see NoOp handling below). Creates and
// updates are ordered Volume -> Container -> Route -> DnsRecord; deletes
// are ordered in reverse (DnsRecord -> Route -> Container -> Volume) so
// dependents are torn down before their dependencies.
//
// NoOp handling: this implementation OMITS unchanged resources from
// Plan.Steps entirely rather than emitting explicit ActionNoOp steps —
// there is nothing actionable or risk-bearing about them, and omission
// keeps Plan.Steps == the literal list of changes to review/approve.
// ActionNoOp remains part of the Action enum for callers that want to
// represent a step, e.g. the reconcile engine's health-gate bookkeeping in
// S2b, as explicitly-inert.
func Diff(desired, observed []resource.Resource) *Plan {
	desiredByKey := make(map[key]resource.Resource, len(desired))
	for _, r := range desired {
		desiredByKey[key{r.Kind, r.Metadata.Name}] = r
	}
	observedByKey := make(map[key]resource.Resource, len(observed))
	for _, r := range observed {
		observedByKey[key{r.Kind, r.Metadata.Name}] = r
	}

	var creates, updates, deletes []PlanStep

	for k, want := range desiredByKey {
		have, exists := observedByKey[k]
		if !exists {
			creates = append(creates, newStep(ActionCreate, want, resource.Resource{}))
			continue
		}
		if !specEqual(want, have) {
			updates = append(updates, newStep(ActionUpdate, want, have))
		}
	}
	for k, have := range observedByKey {
		if _, exists := desiredByKey[k]; !exists {
			deletes = append(deletes, newStep(ActionDelete, resource.Resource{}, have))
		}
	}

	sortByKindOrder(creates, false)
	sortByKindOrder(updates, false)
	sortByKindOrder(deletes, true)

	steps := make([]PlanStep, 0, len(creates)+len(updates)+len(deletes))
	steps = append(steps, creates...)
	steps = append(steps, updates...)
	steps = append(steps, deletes...)

	p := &Plan{
		Summary: fmt.Sprintf("%d create, %d update, %d delete", len(creates), len(updates), len(deletes)),
		Steps:   steps,
	}
	p.OverallRisk = overallRisk(steps)
	return p
}

func sortByKindOrder(steps []PlanStep, reverse bool) {
	sort.SliceStable(steps, func(i, j int) bool {
		// KindOrder, not a raw map read: a kind missing from the table
		// must sort the SAME way for the planner and for every read-only
		// consumer of it, and "last" is the safer default of the two —
		// an undeclared kind has no declared dependents to come before.
		oi, oj := KindOrder(steps[i].Kind), KindOrder(steps[j].Kind)
		if reverse {
			if oi != oj {
				return oi > oj
			}
		} else if oi != oj {
			return oi < oj
		}
		return steps[i].Name < steps[j].Name
	})
}

func overallRisk(steps []PlanStep) RiskGrade {
	max := RiskLow
	for _, s := range steps {
		if s.Risk > max {
			max = s.Risk
		}
	}
	return max
}

func newStep(action Action, want, have resource.Resource) PlanStep {
	r := want
	if action == ActionDelete {
		r = have
	}
	risk, blast := grade(action, want, have)
	return PlanStep{
		Description: describe(action, r),
		Kind:        r.Kind,
		Name:        r.Metadata.Name,
		Action:      action,
		Risk:        risk,
		BlastRadius: blast,
	}
}

func describe(action Action, r resource.Resource) string {
	return fmt.Sprintf("%s %s/%s", action, r.Kind, r.Metadata.Name)
}

// specEqual reports whether two resources sharing a (kind, name) have an
// identical spec (ignoring status, which never exists on resource.Resource
// anyway).
func specEqual(a, b resource.Resource) bool {
	if a.Kind != b.Kind {
		return false
	}
	switch a.Kind {
	case resource.KindContainer:
		return containerEqual(a.Container, b.Container)
	case resource.KindVolume:
		return a.Volume != nil && b.Volume != nil && *a.Volume == *b.Volume
	case resource.KindRoute:
		return a.Route != nil && b.Route != nil && *a.Route == *b.Route
	case resource.KindDnsRecord:
		return a.DnsRecord != nil && b.DnsRecord != nil && *a.DnsRecord == *b.DnsRecord
	case resource.KindOidcClient:
		return oidcClientEqual(a.OidcClient, b.OidcClient)
	case resource.KindLdapGroup:
		return ldapGroupEqual(a.LdapGroup, b.LdapGroup)
	case resource.KindMatrixAppService:
		return a.MatrixAppService != nil && b.MatrixAppService != nil && *a.MatrixAppService == *b.MatrixAppService
	case resource.KindLdapUser:
		return ldapUserEqual(a.LdapUser, b.LdapUser)
	case resource.KindIngress:
		return ingressEqual(a.Ingress, b.Ingress)
	case resource.KindBackupPlan:
		return backupPlanEqual(a.BackupPlan, b.BackupPlan)
	case resource.KindConfigFile:
		return a.ConfigFile != nil && b.ConfigFile != nil && *a.ConfigFile == *b.ConfigFile
	case resource.KindSeedFile:
		return a.SeedFile != nil && b.SeedFile != nil && *a.SeedFile == *b.SeedFile
	case resource.KindSecret:
		return a.Secret != nil && b.Secret != nil && *a.Secret == *b.Secret
	case resource.KindJob:
		if a.Job == nil || b.Job == nil {
			return a.Job == b.Job
		}
		return containerEqual(&a.Job.Run, &b.Job.Run)
	case resource.KindAutomation:
		return a.Automation != nil && b.Automation != nil && reflect.DeepEqual(a.Automation, b.Automation)
	case resource.KindMeter:
		return a.Meter != nil && b.Meter != nil && *a.Meter == *b.Meter
	case resource.KindCommitment:
		return a.Commitment != nil && b.Commitment != nil && reflect.DeepEqual(a.Commitment, b.Commitment)
	default:
		return false
	}
}

func stringSliceEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func oidcClientEqual(a, b *resource.OidcClientSpec) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.ClientId == b.ClientId &&
		a.ClientName == b.ClientName &&
		a.AuthPolicy == b.AuthPolicy &&
		a.Public == b.Public &&
		a.SecretRef == b.SecretRef &&
		stringSliceEqual(a.RedirectUris, b.RedirectUris) &&
		stringSliceEqual(a.Scopes, b.Scopes)
}

// ldapUserEqual compares by value. Groups is order-insensitive: the
// directory has no order, so a package listing the same groups in a
// different order must not read as drift and replan every tick.
func ldapUserEqual(a, b *resource.LdapUserSpec) bool {
	if a == nil || b == nil {
		return a == b
	}
	if a.Username != b.Username || a.DisplayName != b.DisplayName || a.Email != b.Email {
		return false
	}
	if len(a.Groups) != len(b.Groups) {
		return false
	}
	ag := append([]string(nil), a.Groups...)
	bg := append([]string(nil), b.Groups...)
	sort.Strings(ag)
	sort.Strings(bg)
	for i := range ag {
		if ag[i] != bg[i] {
			return false
		}
	}
	return true
}

// ingressEqual compares by value; TrustedProxies is order-insensitive
// because a Caddyfile list has no order and a reordered param must not
// read as drift.
func ingressEqual(a, b *resource.IngressSpec) bool {
	if a == nil || b == nil {
		return a == b
	}
	if a.TLS != b.TLS || len(a.TrustedProxies) != len(b.TrustedProxies) {
		return false
	}
	ap := append([]string(nil), a.TrustedProxies...)
	bp := append([]string(nil), b.TrustedProxies...)
	sort.Strings(ap)
	sort.Strings(bp)
	for i := range ap {
		if ap[i] != bp[i] {
			return false
		}
	}
	return true
}

func ldapGroupEqual(a, b *resource.LdapGroupSpec) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.Name == b.Name
}

func backupPlanEqual(a, b *resource.BackupPlanSpec) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.Repository == b.Repository &&
		a.PasswordSecret == b.PasswordSecret &&
		a.Schedule == b.Schedule &&
		a.Retention == b.Retention &&
		stringSliceEqual(a.Volumes, b.Volumes)
}

func containerEqual(a, b *resource.ContainerSpec) bool {
	if a == nil || b == nil {
		return a == b
	}
	if a.Image != b.Image || a.Restart != b.Restart {
		return false
	}
	if len(a.Env) != len(b.Env) {
		return false
	}
	for k, v := range a.Env {
		if b.Env[k] != v {
			return false
		}
	}
	if !stringSliceEqual(a.Command, b.Command) {
		return false
	}
	if len(a.Ports) != len(b.Ports) {
		return false
	}
	for i := range a.Ports {
		if a.Ports[i] != b.Ports[i] {
			return false
		}
	}
	if len(a.Mounts) != len(b.Mounts) {
		return false
	}
	for i := range a.Mounts {
		if a.Mounts[i] != b.Mounts[i] {
			return false
		}
	}
	return true
}

// mountsChanged reports whether the mount list differs between two
// container specs (used by the risk heuristic: a mount change on update is
// data-affecting => High).
func mountsChanged(a, b *resource.ContainerSpec) bool {
	if a == nil || b == nil {
		return a != b
	}
	if len(a.Mounts) != len(b.Mounts) {
		return true
	}
	for i := range a.Mounts {
		if a.Mounts[i] != b.Mounts[i] {
			return true
		}
	}
	return false
}

// grade applies the ADR-W004 A3 risk heuristic: data-affecting ops (Volume
// delete, mount change on update, Container delete with a bound volume) =
// High; service interruption without data risk (Container image
// change/restart, Route change) = Medium; additive/no-interruption (new
// Container, new Volume, new DnsRecord, new Route) = Low.
func grade(action Action, want, have resource.Resource) (RiskGrade, string) {
	r := want
	if action == ActionDelete {
		r = have
	}
	switch r.Kind {
	case resource.KindVolume:
		switch action {
		case ActionDelete:
			return RiskHigh, "data loss: volume and its contents are removed"
		default:
			return RiskLow, "new volume, no existing service affected"
		}
	case resource.KindContainer:
		switch action {
		case ActionDelete:
			if r.Container != nil && len(r.Container.Mounts) > 0 {
				return RiskHigh, "container removed; bound mount(s)/volume(s) may hold data"
			}
			return RiskMedium, "container removed, no bound mounts"
		case ActionUpdate:
			if mountsChanged(want.Container, have.Container) {
				return RiskHigh, "mount configuration changed: data-affecting"
			}
			return RiskMedium, "container updated (image/env/restart): brief service interruption, no data affected"
		default:
			return RiskLow, "new container, no existing service affected"
		}
	case resource.KindRoute:
		switch action {
		case ActionCreate:
			return RiskLow, "new route, no existing service affected"
		default:
			return RiskMedium, "route change/removal: service interruption for its host"
		}
	case resource.KindDnsRecord:
		switch action {
		case ActionCreate:
			return RiskLow, "new DNS record, no existing service affected"
		default:
			return RiskMedium, "DNS record change/removal: may affect resolution for its name"
		}
	case resource.KindOidcClient:
		switch action {
		case ActionCreate:
			return RiskLow, "new OIDC client, no existing service affected"
		default:
			return RiskMedium, "OIDC client change/removal: batched Authelia reload affects all clients; app login may break"
		}
	case resource.KindConfigFile:
		switch action {
		case ActionCreate:
			return RiskLow, "new config file; nothing is reading it yet"
		default:
			// An app's configuration is how it behaves. Changing one can
			// stop a service starting, and removing one can stop it
			// authenticating anybody — neither visible until the app next
			// reads the file, which may be long after the plan applied.
			return RiskMedium, "configuration changed: the app may fail to start, or start behaving differently"
		}
	case resource.KindSeedFile:
		switch action {
		case ActionCreate:
			// Written only if absent, into a volume nothing has started
			// reading yet. The one case that cannot go wrong.
			return RiskLow, "seed file written before the app first starts, and only if it is not already there"
		default:
			// An update is a recorded no-op and a delete never diffs, so
			// neither touches the running app.
			return RiskLow, "the app owns this file now; Wildstone leaves it alone"
		}
	case resource.KindSecret:
		// Creating a secret is additive and affects nothing that exists:
		// material is generated on the node, and until something
		// references it, it is a file nobody reads.
		//
		// A DELETE is Low for a different and less comfortable reason —
		// there is no delete. No op removes a secret, so the step
		// records the declaration going away and the material stays on
		// the node. Said plainly rather than implied, because "the plan
		// said delete" must not be read as "the key is gone".
		switch action {
		case ActionDelete:
			return RiskLow, "secret no longer declared; the material stays on the node (no delete op exists)"
		default:
			return RiskLow, "generates material on the node; nothing reads it until something references it"
		}
	case resource.KindJob:
		// A job is not a declaration: applying one RUNS a program that
		// changes an external system, and unlike every other kind here
		// there is no reverse op that undoes what it did. Creating the
		// job that seeds a directory is as consequential as any change
		// in this table, and deleting the resource does not un-seed it.
		//
		// Medium on create as well as change, therefore — the only kind
		// whose creation is not Low. A first run and a re-run differ
		// only in that the second has more to collide with.
		switch action {
		case ActionDelete:
			return RiskLow, "job resource removed; the run it already performed is not undone"
		default:
			return RiskMedium, "running a provisioning job: it mutates an external system and nothing reverses it"
		}
	case resource.KindMatrixAppService:
		// The declaration touches nothing itself: the homeserver's own
		// Job installs the merged registrations and its container is
		// replaced to load them, and both are steps of their own. What
		// this step decides is whether a bot may speak on Matrix, so
		// taking that away is the direction worth a second look.
		if action == ActionDelete {
			return RiskMedium, "bot registration removed: the bot can no longer reach Matrix, and the people it serves lose that way to their Agent"
		}
		return RiskLow, "registers one bot user with the household's Matrix homeserver; it can act only as itself"
	case resource.KindLdapGroup:
		// The declaration itself still touches nothing: it is the
		// rendered group list plus the identity capability's
		// provisioning Job that reach LLDAP, and the Job carries its own
		// Medium risk for exactly that reason. Creating a group is also
		// the benign direction — a group nobody is in grants nothing.
		//
		// A delete is Low for a narrower reason: it removes the group
		// from the rendered list, and the provisioning job never deletes
		// a group. The group, and everyone in it, survives.
		return RiskLow, "declared group only; the identity capability's provisioning job is what creates it"
	case resource.KindBackupPlan:
		// The declaration itself moves no data — the backup engine's runs
		// do, and those carry their own provenance (ADR-W011 A4). Removing
		// one is Medium: nothing is deleted, but protection for its
		// volumes stops, which the owner should consciously confirm.
		if action == ActionDelete {
			return RiskMedium, "backup coverage removed: no data deleted, but the named volumes stop being backed up"
		}
		return RiskLow, "backup schedule declaration only; runs execute with their own provenance (ADR-W011 A4)"
	case resource.KindAutomation:
		// A rule acts on the household on its own once approved — that
		// approval is the whole point (ADR-W024 A1), so creating or
		// changing one is never Low: Low is the tier drift repair
		// applies unattended, and a rule must always pass a human.
		// Removing one is Low: the rule stops, and nothing else changes.
		if action == ActionDelete {
			return RiskLow, "rule removed: it stops firing; nothing else changes"
		}
		return RiskMedium, "a household rule that will act on its own once approved: review its trigger, conditions, and action"
	case resource.KindMeter:
		// Deleting a meter is the consequential direction, which is the
		// opposite way round from most kinds and worth the words. The
		// commitments that read it keep their thresholds and lose the
		// number those thresholds are measured against, so each of them
		// stops being able to say whether it is due — silently, unless
		// somebody is told. Creating or changing one only alters how a
		// number is read.
		if action == ActionDelete {
			return RiskMedium, "meter removed: every commitment measured against it can no longer say whether it is due"
		}
		return RiskLow, "a declaration of a number the household keeps; it acts on nothing by itself (ADR-W035 A1)"
	case resource.KindCommitment:
		// Approved once, then it asks on its own for as long as the
		// household keeps it — which is the whole point, and the reason
		// creating or changing one always passes a human. Removing one
		// is Low in the ordinary sense that nothing else changes, and
		// the household should still be told plainly that the thing
		// stops being tracked rather than becoming up to date.
		if action == ActionDelete {
			return RiskLow, "commitment removed: it stops being tracked — which is not the same as being done"
		}
		return RiskMedium, "a standing obligation that will ask the household on its own once approved: review what it is due against, and who it is for"
	default:
		return RiskMedium, "unrecognized kind"
	}
}

// KindOrder exposes kindOrder for read-only consumers (the Butler's
// diagnose walker, ADR-W017 A5) so dependency depth has ONE definition.
// Lower is more foundational; unknown kinds sort last.
func KindOrder(k resource.Kind) int {
	if o, ok := kindOrder[k]; ok {
		return o
	}
	return len(kindOrder)
}

// WithSteps returns p with a different step list and its OverallRisk
// recomputed.
//
// Exported so a caller that must drop steps — the reconciler setting a
// repeatedly-failing resource aside (F60) — cannot end up with a plan
// whose risk grade describes steps it no longer contains. The rule for
// deriving that grade lives here and should have exactly one
// implementation.
func WithSteps(p Plan, steps []PlanStep) Plan {
	p.Steps = steps
	p.OverallRisk = overallRisk(steps)
	return p
}
