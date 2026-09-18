package resource

import "testing"

// TestNodeScopeIsEnforcedBothWays pins the invariant the reconcile loop's
// bucketing depends on. A node-scoped resource with no metadata.node is
// bucketed under "" by groupByNode, where there is no agent session, and
// used to fail deep inside observe with a transport error naming neither
// the resource nor the missing field — so every OidcClient and LdapGroup
// the shipped packages produced was silently undeliverable.
func TestNodeScopeIsEnforcedBothWays(t *testing.T) {
	for _, k := range Kinds() {
		t.Run(string(k), func(t *testing.T) {
			r := validResourceOfKind(t, k)

			r.Metadata.Node = ""
			err := r.Validate()
			if NodeScoped(k) {
				if err == nil {
					t.Fatalf("%s validated with no metadata.node", k)
				}
				var ve *ValidationError
				if !asValidationError(err, &ve) || ve.Field != "metadata.node" {
					t.Fatalf("%s: want metadata.node validation error, got %v", k, err)
				}
			} else if err != nil {
				t.Fatalf("%s is not node-scoped but was rejected with no node: %v", k, err)
			}

			r.Metadata.Node = "node-a"
			err = r.Validate()
			if NodeScoped(k) {
				if err != nil {
					t.Fatalf("%s rejected with a node: %v", k, err)
				}
			} else {
				if err == nil {
					t.Fatalf("%s validated with a metadata.node it cannot use", k)
				}
				var ve *ValidationError
				if !asValidationError(err, &ve) || ve.Field != "metadata.node" {
					t.Fatalf("%s: want metadata.node validation error, got %v", k, err)
				}
			}
		})
	}
}

// TestNodeScopedMatchesDispatch is the coupling check: NodeScoped must
// agree with how the reconciler actually dispatches each kind. DnsRecord
// is provider-backed (ADR-W004 A5) and Automation is Keystone-owned
// (ADR-W024 A1) — neither reaches a node agent; everything else in the
// closed registry does. Adding a kind without deciding this fails here
// rather than at runtime on the operator's node.
func TestNodeScopedMatchesDispatch(t *testing.T) {
	notAtANode := map[Kind]bool{KindDnsRecord: true, KindAutomation: true, KindMeter: true, KindCommitment: true}
	for _, k := range Kinds() {
		if NodeScoped(k) == notAtANode[k] {
			t.Errorf("%s: NodeScoped=%v but dispatched-off-node=%v — they must be opposites",
				k, NodeScoped(k), notAtANode[k])
		}
	}
}

func asValidationError(err error, out **ValidationError) bool {
	ve, ok := err.(*ValidationError)
	if ok {
		*out = ve
	}
	return ok
}

// validResourceOfKind returns a resource that passes Validate once its
// metadata.node is set correctly for the kind — the minimum needed to
// isolate the node rule from every other rule.
func validResourceOfKind(t *testing.T, k Kind) Resource {
	t.Helper()
	r := Resource{ApiVersion: ApiVersion, Kind: k, Metadata: Metadata{Name: "r1", Owner: "system"}}
	switch k {
	case KindContainer:
		r.Container = &ContainerSpec{Image: "docker.io/library/nginx@sha256:" + "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"}
	case KindVolume:
		r.Volume = &VolumeSpec{Name: "r1"}
	case KindRoute:
		r.Route = &RouteSpec{Host: "r1.example.com", Upstream: "container/r1:80", Auth: "none", Classification: "family_public"}
	case KindDnsRecord:
		r.DnsRecord = &DnsRecordSpec{Zone: "example.com", Name: "r1.example.com", Type: "A", Content: "203.0.113.10"}
	case KindOidcClient:
		r.OidcClient = &OidcClientSpec{ClientId: "r1", ClientName: "R1",
			RedirectUris: []string{"https://r1.example.com/cb"}, SecretRef: "secret/r1-oidc", AuthPolicy: "two_factor"}
	case KindLdapGroup:
		r.LdapGroup = &LdapGroupSpec{Name: "r1_users"}
	case KindIngress:
		r.Ingress = &IngressSpec{TLS: "internal", TrustedProxies: []string{"private_ranges"}}
	case KindLdapUser:
		r.LdapUser = &LdapUserSpec{Username: "r1", DisplayName: "R One",
			Email: "r1@example.com", Groups: []string{"r1_users"}}
	case KindBackupPlan:
		r.BackupPlan = &BackupPlanSpec{Volumes: []string{"r1"}, Repository: "r1-repo",
			PasswordSecret: "secret/restic-r1", Schedule: "daily"}
	case KindConfigFile:
		// A ConfigFile's path is derived from its owning capability, so
		// unlike every other kind it cannot be owned by "system".
		r.Metadata.Owner = "capability/r1"
		r.ConfigFile = &ConfigFileSpec{Filename: "config.yml", Content: "a: b\n"}
	case KindSeedFile:
		// Like ConfigFile, a seed is bounded by its owning capability:
		// that is what says which volume it may write into.
		r.Metadata.Owner = "capability/r1"
		r.SeedFile = &SeedFileSpec{Volume: "r1-config", Path: "configuration.yaml", Content: "a: b\n"}
	case KindSecret:
		r.Secret = &SecretSpec{Name: "r1-oidc", Kind: "rsa_private_key"}
	case KindJob:
		// No restart policy and no ports: validateJob refuses both, and
		// this fixture has to pass validation to reach the scope check.
		r.Job = &JobSpec{Run: ContainerSpec{Image: "docker.io/library/nginx@sha256:" + "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"}}
	case KindAutomation:
		// A rule belongs to the household or a capability, never to
		// "system" (ADR-W024 A1): someone must be there to be asked.
		r.Metadata.Owner = "household"
		r.Automation = &AutomationSpec{Description: "r1",
			Trigger: AutomationTrigger{EventType: "person.left_home"},
			Action:  AutomationAction{Tool: "notifications.notify", Tier: "domestic"}}
	case KindMeter:
		// A meter belongs to the household or a capability, for the same
		// reason a rule does (ADR-W034 A1).
		r.Metadata.Owner = "household"
		r.Meter = &MeterSpec{Description: "odometer", Unit: "miles"}
	case KindCommitment:
		r.Metadata.Owner = "household"
		r.Commitment = &CommitmentSpec{Description: "oil", Rollover: RolloverFromCompletion,
			Anchor: "2026-03-01", Due: []CommitmentDue{{Meter: "odo", Every: "5000"}}}
	default:
		t.Fatalf("kind %s has no fixture here — a kind was added without deciding its node scope", k)
	}
	return r
}
