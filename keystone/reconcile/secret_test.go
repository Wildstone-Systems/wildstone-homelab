package reconcile

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/chickenrat17/wildstone-homelab/keystone/plan"
	"github.com/chickenrat17/wildstone-homelab/keystone/resource"
	wsv1 "github.com/chickenrat17/wildstone-homelab/proto/gen/wardstone/v1"
)

func secretResource(name, kind string) resource.Resource {
	return resource.Resource{
		ApiVersion: resource.ApiVersion,
		Kind:       resource.KindSecret,
		Metadata:   resource.Metadata{Name: name, Owner: "capability/identity", Node: "node-a"},
		Secret:     &resource.SecretSpec{Name: name, Kind: kind},
	}
}

// identityProviderSecrets are the two secrets the OIDC provider itself
// needs, which renderAutheliaClients references whenever any client
// exists. In production the identity package declares them; a test that
// renders clients has to as well, or it is asserting against a file the
// node would refuse to write.
func identityProviderSecrets() []resource.Resource {
	return []resource.Resource{
		secretResource("authelia-oidc-hmac-secret", ""),
		secretResource("authelia-oidc-jwks", "rsa_private_key"),
	}
}

func secretReconciler(a Applier) *Reconciler {
	r := newReconciler(a)
	r.Approve = func(*plan.Plan) bool { return true }
	return r
}

// TestSecretIsEnsuredWithItsDeclaredKind is the whole point of the kind.
// An OIDC provider signs ID tokens with an RSA key; a random blob is not
// a key, and before this nothing in Keystone or the CLI ever set
// EnsureSecret.kind, so protocol minor 10's RSA support was unreachable
// from the control plane.
func TestSecretIsEnsuredWithItsDeclaredKind(t *testing.T) {
	h := startGateway(t, []string{"tok"})
	agent := newTestAgent(t, h, "node-a", "tok")
	if !h.gw.WaitConnected("node-a", 10*time.Second) {
		t.Fatal("agent never connected")
	}

	desired := []resource.Resource{
		secretResource("authelia-oidc-jwks", "rsa_private_key"),
		secretResource("authelia-session-secret", ""),
	}
	if _, err := secretReconciler(&GatewayApplier{Gateway: h.gw}).Reconcile(context.Background(), "node-a", desired); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	got, ok := agent.secretKind("authelia-oidc-jwks")
	if !ok {
		t.Fatal("the jwks secret was never ensured")
	}
	if got != wsv1.SecretKind_SECRET_KIND_RSA_PRIVATE_KEY {
		t.Errorf("jwks secret ensured as %v, want RSA — an OIDC provider cannot sign with a random blob", got)
	}
	// An empty kind means random, so a package that just needs a
	// password says nothing.
	if got, ok := agent.secretKind("authelia-session-secret"); !ok || got != wsv1.SecretKind_SECRET_KIND_RANDOM {
		t.Errorf("a kindless secret was ensured as %v (present=%v), want RANDOM", got, ok)
	}
}

// TestSecretsExistBeforeTheFilesThatReferenceThem is the ordering this
// kind lives or dies on, and the reason ensureNodeSecrets exists at all
// rather than relying on kindOrder.
//
// Wildstone writes managed files BEFORE any plan step. Those files carry
// secret references the node resolves at write time, so a secret created
// by a plan step is created too late: the prereq write fails with
// NOT_FOUND, a failed prereq aborts the tick, and the plan that would
// have created the secret never runs. That is a deadlock no retry
// escapes — which is why this asserts a single tick converges.
func TestSecretsExistBeforeTheFilesThatReferenceThem(t *testing.T) {
	h := startGateway(t, []string{"tok"})
	agent := newTestAgent(t, h, "node-a", "tok")
	if !h.gw.WaitConnected("node-a", 10*time.Second) {
		t.Fatal("agent never connected")
	}

	// An OidcClient puts a pbkdf2:secret/<ref> into the merged clients
	// file, which is a prereq — written before the plan runs.
	desired := append([]resource.Resource{
		oidcClientResource("photos", "photos", "secret/photos-oidc"),
		secretResource("photos-oidc", ""),
	}, identityProviderSecrets()...)
	out, err := secretReconciler(&GatewayApplier{Gateway: h.gw}).Reconcile(context.Background(), "node-a", desired)
	if err != nil {
		t.Fatalf("first tick failed: %v\n\nA secret the clients file references did not exist when that "+
			"file was written. Creating it from a plan step is too late: the prereq write aborts the "+
			"tick before the plan runs, so nothing ever creates it", err)
	}
	if !out.Applied {
		t.Fatalf("first tick did not apply: %+v", out)
	}
	if _, ok := agent.secretKind("photos-oidc"); !ok {
		t.Error("the referenced secret was never ensured")
	}
}

// TestSecretDeleteLeavesTheMaterial states the deliberate asymmetry. No
// op removes a secret, so a Secret leaving desired state records the
// declaration going away and nothing else. "The plan said delete" must
// not be read as "the key is gone".
func TestSecretDeleteLeavesTheMaterial(t *testing.T) {
	h := startGateway(t, []string{"tok"})
	agent := newTestAgent(t, h, "node-a", "tok")
	if !h.gw.WaitConnected("node-a", 10*time.Second) {
		t.Fatal("agent never connected")
	}
	r := secretReconciler(&GatewayApplier{Gateway: h.gw})

	desired := []resource.Resource{secretResource("going-away", "")}
	if _, err := r.Reconcile(context.Background(), "node-a", desired); err != nil {
		t.Fatalf("create tick: %v", err)
	}
	if _, ok := agent.secretKind("going-away"); !ok {
		t.Fatal("secret was not created")
	}

	out, err := r.Reconcile(context.Background(), "node-a", nil)
	if err != nil {
		t.Fatalf("delete tick: %v", err)
	}
	var sawDelete bool
	for _, s := range out.Steps {
		if s.Step.Kind == resource.KindSecret && s.Step.Action == plan.ActionDelete {
			sawDelete = true
			if !s.Applied || !s.HealthOK {
				t.Errorf("the delete step did not succeed: %+v", s)
			}
		}
	}
	if !sawDelete {
		t.Fatalf("no Secret delete was planned: %+v", out.Plan.Steps)
	}
	if _, ok := agent.secretKind("going-away"); !ok {
		t.Error("the secret's material was removed from the node. Nothing should do that: there is no " +
			"delete op, and a control plane that destroys key material on a state change is one bad " +
			"plan away from losing the only copy")
	}
}

// TestSecretOrderedBeforeEverythingThatCanReferenceOne pins the plan
// order. Both a ConfigFile (resolved at write time) and a Container (env
// references) can name a secret.
func TestSecretOrderedBeforeEverythingThatCanReferenceOne(t *testing.T) {
	for _, later := range []resource.Kind{
		resource.KindConfigFile, resource.KindContainer, resource.KindJob, resource.KindOidcClient,
	} {
		if plan.KindOrder(resource.KindSecret) >= plan.KindOrder(later) {
			t.Errorf("Secret (%d) must sort before %s (%d): %s can carry a secret reference, and the "+
				"node resolves those at apply time",
				plan.KindOrder(resource.KindSecret), later, plan.KindOrder(later), later)
		}
	}
}

// providedSecretResource declares a secret the node must not generate.
func providedSecretResource(name string) resource.Resource {
	r := secretResource(name, "")
	r.Secret = &resource.SecretSpec{Name: name, Provided: true}
	return r
}

// TestProvidedSecretsAreNamedTogetherAndNeverGenerated pins the fix for
// F37. Before this, a secret the operator had to supply was indexed by
// the reconciler exactly like one the node could make: it was generated
// as random bytes, and the consuming service failed later for reasons
// that never named the secret. A Cloudflare tunnel token is the case
// that makes it obvious — random bytes there are not a weaker token,
// they are a broken one.
//
// Two claims, and the second is the one that was actually painful: they
// are reported ALL AT ONCE. Discovering missing secrets one per tick,
// after approving a plan, is the complaint F37 records.
func TestProvidedSecretsAreNamedTogetherAndNeverGenerated(t *testing.T) {
	h := startGateway(t, []string{"tok"})
	agent := newTestAgent(t, h, "node-a", "tok")
	if !h.gw.WaitConnected("node-a", 10*time.Second) {
		t.Fatal("agent never connected")
	}

	desired := []resource.Resource{
		providedSecretResource("cloudflared-tunnel-token"),
		providedSecretResource("paperless-oauth-providers"),
		secretResource("immich-postgres-password", ""),
	}
	// Since ADR-W028 this no longer fails the node: the capability
	// awaiting the secrets is set aside and everything else converges.
	// The reporting claim is unchanged and is what F37 was about — the
	// operator learns about BOTH at once, with the command for each.
	out, err := secretReconciler(&GatewayApplier{Gateway: h.gw}).Reconcile(context.Background(), "node-a", desired)
	if err != nil {
		t.Fatalf("a capability awaiting a secret failed the whole node: %v", err)
	}
	if len(out.Blocked) == 0 {
		t.Fatal("nothing was reported as blocked, so the operator is told nothing")
	}
	msg := out.Blocked[0].Reason
	for _, want := range []string{"cloudflared-tunnel-token", "paperless-oauth-providers", "wild secret set"} {
		if !strings.Contains(msg, want) {
			t.Errorf("the block did not name %q, so the operator cannot act on it:\n%s", want, msg)
		}
	}

	// Never generated. This is the claim that matters: a generated value
	// would look like success and fail somewhere else entirely.
	for _, n := range []string{"cloudflared-tunnel-token", "paperless-oauth-providers"} {
		if _, ok := agent.secretKind(n); ok {
			t.Errorf("%s was generated; a provided secret must never be", n)
		}
	}

	// Supply them, as an operator would, and the same desired state
	// converges — including generating the one that IS generatable.
	agent.putSecret("cloudflared-tunnel-token")
	agent.putSecret("paperless-oauth-providers")
	if _, err := secretReconciler(&GatewayApplier{Gateway: h.gw}).Reconcile(context.Background(), "node-a", desired); err != nil {
		t.Fatalf("reconcile still failed after the secrets were supplied: %v", err)
	}
	if _, ok := agent.secretKind("immich-postgres-password"); !ok {
		t.Error("the generatable secret was not generated")
	}
}

// ownedProvidedSecret is a provided secret filed under a capability, so
// a test can say which capability is waiting.
func ownedProvidedSecret(owner, name string) resource.Resource {
	r := providedSecretResource(name)
	r.Metadata.Owner = "capability/" + owner
	return r
}

func ownedSecret(owner, name string) resource.Resource {
	r := secretResource(name, "")
	r.Metadata.Owner = "capability/" + owner
	return r
}

// One capability enabled without its secrets used to stop every OTHER
// capability on the machine from converging, so an unrelated install
// approved afterwards failed too — with a message about secrets it had
// never heard of. That is ADR-W028, and it is the whole reason the
// scope of the consequence now matches the scope of the problem.
func TestOneCapabilityAwaitingASecretDoesNotStopTheOthers(t *testing.T) {
	h := startGateway(t, []string{"tok"})
	agent := newTestAgent(t, h, "node-a", "tok")
	if !h.gw.WaitConnected("node-a", 10*time.Second) {
		t.Fatal("agent never connected")
	}

	desired := []resource.Resource{
		ownedProvidedSecret("chat-telegram-family", "chat-telegram-family-bot"),
		ownedSecret("ai-chat", "open-webui-secret-key"),
		ownedSecret("identity", "authelia-jwt-secret"),
	}
	out, err := secretReconciler(&GatewayApplier{Gateway: h.gw}).Reconcile(context.Background(), "node-a", desired)
	if err != nil {
		t.Fatalf("one capability's missing secret failed the node: %v", err)
	}

	// The capabilities that CAN converge, did.
	for _, n := range []string{"open-webui-secret-key", "authelia-jwt-secret"} {
		if _, ok := agent.secretKind(n); !ok {
			t.Errorf("%s was not generated; an unrelated capability was held back", n)
		}
	}
	// And the one that cannot is named, with what to do about it.
	if len(out.Blocked) == 0 {
		t.Fatal("the waiting capability was not reported")
	}
	var found bool
	for _, b := range out.Blocked {
		if strings.Contains(b.Reason, "chat-telegram-family-bot") {
			found = true
			if !strings.Contains(b.Reason, "chat-telegram-family can converge") {
				t.Errorf("the block does not name the capability waiting:\n%s", b.Reason)
			}
		}
	}
	if !found {
		t.Fatalf("the missing secret was not named: %+v", out.Blocked)
	}
}
