package runtimedb

// Epic #326 S02's evidence at the store: a Personal mailbox credential
// is staged before it is fenced, revoked by fencing first, and erased
// only on proof; unattended use needs the intersection of the owning
// Person's authorization and the Owner's approval of the exact
// subscription version; and nobody else, nothing else, and no material
// edit gets through.

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"
)

const (
	testExecutable = "mail-roundcube@1.1.0/wild-mail-adapter"
	testPassword   = "REPLACED-app-password-value-7f3a"
)

// mailFixture: sam (Member) owns sam-mail, placed on n1 and n2; bo is
// another Member; mika is an Owner; hobson is an Agent sam and bo may
// both use.
type mailFixture struct {
	db     *DB
	agent  int64
	credV1 PersonalCredential
}

func newMailFixture(t *testing.T) *mailFixture {
	t.Helper()
	db := butlerFixture(t)
	record(t, db, "bo")
	if err := db.AdmitButlerMember("bo", "root-admin"); err != nil {
		t.Fatal(err)
	}
	agent, err := db.CreateButler(Butler{
		Name: "hobson", Directive: "watch the post", Surface: ButlerSurfaceFamily,
		AgentClass: AgentClassMember, Model: "kitchen-box",
	}, "root-admin")
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range []string{"sam", "bo", "mika"} {
		if err := db.GrantButlerAccess("hobson", m, "root-admin"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.CreateConnection(IntegrationConnection{
		Name: "sam-mail", Adapter: "wild-mail-adapter", Ownership: OwnershipPersonal,
		Owner: "sam", CreatedBy: "root-admin",
	}); err != nil {
		t.Fatal(err)
	}
	for i, n := range []string{"n1", "n2"} {
		role := PlacementPrimary
		if i > 0 {
			role = PlacementFallback
		}
		if err := db.AddPlacement("sam-mail", n, role, i, "root-admin"); err != nil {
			t.Fatal(err)
		}
		if err := db.MarkPlacementPrepared("sam-mail", n, true, true, true); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := db.PlaceConnection("sam-mail", time.Hour); err != nil {
		t.Fatal(err)
	}
	if err := db.SetConnectionExecutable("sam-mail", testExecutable, "root-admin"); err != nil {
		t.Fatal(err)
	}
	f := &mailFixture{db: db, agent: agent}
	f.credV1 = f.onboard(t, "sam@example.internal")
	return f
}

// onboard stages, confirms on both nodes and activates a credential,
// exactly as the intake does.
func (f *mailFixture) onboard(t *testing.T, login string) PersonalCredential {
	t.Helper()
	staged, err := f.db.StagePersonalCredential("sam-mail", "", login, "sam", []string{"n1", "n2"})
	if err != nil {
		t.Fatalf("StagePersonalCredential: %v", err)
	}
	for _, n := range []string{"n1", "n2"} {
		if err := f.db.MarkCredentialCopy(staged.Seq, n, true, 1); err != nil {
			t.Fatal(err)
		}
	}
	active, _, err := f.db.ActivatePersonalCredential(staged.Seq, "sam")
	if err != nil {
		t.Fatalf("ActivatePersonalCredential: %v", err)
	}
	return active
}

func (f *mailFixture) request(sub int64) BackgroundAuthorizationRequest {
	return BackgroundAuthorizationRequest{
		Connection: "sam-mail", Scope: BackgroundScope{Folders: []string{"INBOX", "School"}},
		ProcessingLocation: ProcessingLocal, Agent: f.agent, TargetCalendar: "sam-family",
		Subscription: sub,
	}
}

// subscribe files and (optionally) Owner-approves a subscription that
// names digest.
func (f *mailFixture) subscribe(t *testing.T, digest string, approve bool) int64 {
	t.Helper()
	match, _ := json.Marshal(BackgroundSubscriptionMatch("sam-mail", digest))
	id, err := f.db.InsertAgentSubscription(f.agent, "schedule", string(match), AudienceOwner,
		[]string{"mail.changes"}, "")
	if err != nil {
		t.Fatal(err)
	}
	if approve {
		if err := f.db.ApproveAgentSubscription(id, "mika"); err != nil {
			t.Fatal(err)
		}
	}
	return id
}

// authorize runs the whole two-sided flow: preview digest, subscription
// naming it, Person authorization, Owner approval.
func (f *mailFixture) authorize(t *testing.T) (BackgroundAuthorization, int64) {
	t.Helper()
	preview, err := f.db.PrepareBackgroundAuthorization("sam", f.request(0))
	if err != nil {
		t.Fatalf("PrepareBackgroundAuthorization: %v", err)
	}
	sub := f.subscribe(t, preview.Digest, false)
	a, err := f.db.AuthorizeBackgroundUseAsPerson("sam", f.request(sub))
	if err != nil {
		t.Fatalf("AuthorizeBackgroundUseAsPerson: %v", err)
	}
	if a.Digest != preview.Digest {
		t.Fatalf("stored digest %s != preview %s", a.Digest, preview.Digest)
	}
	if err := f.db.ApproveAgentSubscription(sub, "mika"); err != nil {
		t.Fatal(err)
	}
	return a, sub
}

func mustDeny(t *testing.T, f *mailFixture, sub int64, why string) {
	t.Helper()
	if _, err := f.db.AuthorizeBackgroundUse(context.Background(), "sam-mail", sub, 1); !errors.Is(err, ErrBackgroundUseDenied) {
		t.Fatalf("%s: background use = %v, want denied", why, err)
	}
}

func TestBackgroundUseNeedsBothThePersonAndTheOwner(t *testing.T) {
	f := newMailFixture(t)
	a, sub := f.authorize(t)
	grant, err := f.db.AuthorizeBackgroundUse(context.Background(), "sam-mail", sub, 1)
	if err != nil {
		t.Fatalf("both halves hold, background use = %v", err)
	}
	if grant.Person != "sam" || grant.CredentialVersion != 1 || grant.TargetCalendar != "sam-family" ||
		grant.AuthorizationDigest != a.Digest || grant.SecretRef != f.credV1.SecretRef ||
		strings.Join(grant.Scope.Folders, ",") != "INBOX,School" || grant.ProcessingLocation != ProcessingLocal {
		t.Fatalf("grant = %+v", grant)
	}
	// Not the approved version the dispatcher asked about.
	if _, err := f.db.AuthorizeBackgroundUse(context.Background(), "sam-mail", sub, 2); !errors.Is(err, ErrBackgroundUseDenied) {
		t.Fatal("a different subscription version was accepted")
	}

	// The Person revoking fences new reads at once.
	epoch := grant.LeaseEpoch
	if _, err := f.db.EnqueueIntegrationWork("sam-mail", IntegrationWork{Kind: WorkQuery, Capability: "mail.changes"}, time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := f.db.RevokeBackgroundAuthorization("sam-mail", "sam", "stop watching"); err != nil {
		t.Fatal(err)
	}
	mustDeny(t, f, sub, "revoked by the Person")
	conn, _ := f.db.GetConnection("sam-mail")
	if conn.LeaseEpoch <= epoch {
		t.Fatal("revocation did not move the lease epoch")
	}
	var queued int
	f.db.sql.QueryRow(`SELECT COUNT(*) FROM integration_work WHERE state = 'queued'`).Scan(&queued)
	if queued != 0 {
		t.Fatal("queued mail work survived the revocation")
	}
}

// The denial matrix for AUTHORIZE: nobody but the owner.
func TestOnlyTheOwningPersonMayAuthorize(t *testing.T) {
	f := newMailFixture(t)
	for _, actor := range []string{"bo", "mika", "hobson", "root-admin", ""} {
		if _, err := f.db.PrepareBackgroundAuthorization(actor, f.request(0)); !errors.Is(err, ErrNotCredentialOwner) {
			t.Errorf("%q previewed an authorization for sam's mailbox: %v", actor, err)
		}
		if _, err := f.db.AuthorizeBackgroundUseAsPerson(actor, f.request(1)); !errors.Is(err, ErrNotCredentialOwner) {
			t.Errorf("%q authorized sam's mailbox: %v", actor, err)
		}
		if _, err := f.db.StagePersonalCredential("sam-mail", "", "x@example.internal", actor, []string{"n1"}); !errors.Is(err, ErrNotCredentialOwner) {
			t.Errorf("%q replaced sam's credential: %v", actor, err)
		}
	}
}

// USE: the Owner approving a subscription alone is not enough, and no
// other artifact substitutes for sam.
func TestOwnerApprovalAloneAndOtherStandInsDoNotSubstitute(t *testing.T) {
	f := newMailFixture(t)
	preview, err := f.db.PrepareBackgroundAuthorization("sam", f.request(0))
	if err != nil {
		t.Fatal(err)
	}
	// Owner-approved, enabled, naming the exact digest — but sam never
	// authorized.
	sub := f.subscribe(t, preview.Digest, true)
	mustDeny(t, f, sub, "Owner approval alone")

	// An Agent grant on a household secret and a shared-Agent
	// relationship are both in place (hobson is granted to sam, bo and
	// mika). Still nothing.
	mustDeny(t, f, sub, "Agent grants and a shared Agent")

	// A row claiming to be bo's authorization of sam's mailbox cannot be
	// written through the API; forged directly, the evaluator refuses it.
	scope, _ := json.Marshal(preview.Scope)
	conn, _ := f.db.GetConnection("sam-mail")
	subRow, _ := f.db.GetAgentSubscription(sub)
	subDigest, _ := SubscriptionContentDigest(subRow)
	if _, err := f.db.ExecForTest(`INSERT INTO personal_background_authorizations (connection, person, credential,
		credential_version, source_scope, processing_location, result_audience, agent, target_calendar, executable,
		subscription, subscription_digest, digest, created_at)
		VALUES (?, 'bo', ?, 1, ?, 'local', 'aud_owner', ?, 'sam-family', ?, ?, ?, ?, ?)`,
		conn.Seq, f.credV1.Seq, string(scope), f.agent, testExecutable, sub, subDigest, preview.Digest,
		time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	mustDeny(t, f, sub, "an authorization given by another Person")
}

func TestMaterialEditsInvalidate(t *testing.T) {
	cases := map[string]func(t *testing.T, f *mailFixture, sub int64){
		"credential replaced": func(t *testing.T, f *mailFixture, _ int64) {
			f.onboard(t, "sam@example.internal")
		},
		"adapter executable changed": func(t *testing.T, f *mailFixture, _ int64) {
			if err := f.db.SetConnectionExecutable("sam-mail", "mail-roundcube@1.2.0/wild-mail-adapter", "root-admin"); err != nil {
				t.Fatal(err)
			}
		},
		"subscription edited in place": func(t *testing.T, f *mailFixture, sub int64) {
			if _, err := f.db.ExecForTest(`UPDATE agent_subscriptions SET capability_set = '["mail.changes","mail.message"]' WHERE id = ?`, sub); err != nil {
				t.Fatal(err)
			}
		},
		"subscription audience widened": func(t *testing.T, f *mailFixture, sub int64) {
			if _, err := f.db.ExecForTest(`UPDATE agent_subscriptions SET audience = 'aud_household' WHERE id = ?`, sub); err != nil {
				t.Fatal(err)
			}
		},
		"Owner disabled the subscription": func(t *testing.T, f *mailFixture, sub int64) {
			if err := f.db.SetAgentSubscriptionEnabled(sub, false, "mika"); err != nil {
				t.Fatal(err)
			}
		},
		"scope re-authorized by the Person": func(t *testing.T, f *mailFixture, sub int64) {
			// A new authorization with a different scope supersedes the
			// old one; the approved subscription names the OLD digest,
			// so the Owner's approval no longer applies either.
			req := f.request(sub)
			req.Scope.Folders = []string{"INBOX"}
			if _, err := f.db.AuthorizeBackgroundUseAsPerson("sam", req); !errors.Is(err, ErrDigestMismatch) {
				t.Fatalf("re-authorizing against the old subscription = %v, want a digest mismatch", err)
			}
			preview, err := f.db.PrepareBackgroundAuthorization("sam", f.request(0))
			if err != nil {
				t.Fatal(err)
			}
			preview.Scope.Folders = []string{"INBOX"}
			other := f.subscribe(t, "not-the-live-digest", true)
			req.Subscription = other
			if _, err := f.db.AuthorizeBackgroundUseAsPerson("sam", req); !errors.Is(err, ErrDigestMismatch) {
				t.Fatalf("authorizing against a subscription naming another digest = %v", err)
			}
			// Now the real re-authorization: its own subscription.
			newPreview, err := f.db.PrepareBackgroundAuthorization("sam", BackgroundAuthorizationRequest{
				Connection: "sam-mail", Scope: BackgroundScope{Folders: []string{"INBOX"}}, Agent: f.agent,
				TargetCalendar: "sam-family"})
			if err != nil {
				t.Fatal(err)
			}
			fresh := f.subscribe(t, newPreview.Digest, false)
			req.Subscription = fresh
			if _, err := f.db.AuthorizeBackgroundUseAsPerson("sam", req); err != nil {
				t.Fatal(err)
			}
		},
		"cloud routing narrowed after a cloud authorization": nil,
		"Person offboarded": func(t *testing.T, f *mailFixture, _ int64) {
			if _, err := f.db.OffboardMember("sam", "root-admin", "mika"); err != nil {
				t.Fatal(err)
			}
		},
		"restore quarantine": func(t *testing.T, f *mailFixture, _ int64) {
			if err := f.db.EnterRestoreQuarantine("restored", time.Now()); err != nil {
				t.Fatal(err)
			}
		},
	}
	for name, edit := range cases {
		if edit == nil {
			continue
		}
		t.Run(name, func(t *testing.T) {
			f := newMailFixture(t)
			_, sub := f.authorize(t)
			if _, err := f.db.AuthorizeBackgroundUse(context.Background(), "sam-mail", sub, 1); err != nil {
				t.Fatalf("before the edit: %v", err)
			}
			edit(t, f, sub)
			mustDeny(t, f, sub, name)
		})
	}

	t.Run("cloud routing narrowed after a cloud authorization", func(t *testing.T) {
		f := newMailFixture(t)
		if err := f.db.WidenConnection("sam-mail", false, false, true); err != nil {
			t.Fatal(err)
		}
		req := f.request(0)
		req.ProcessingLocation = ProcessingCloud
		preview, err := f.db.PrepareBackgroundAuthorization("sam", req)
		if err != nil {
			t.Fatal(err)
		}
		req.Subscription = f.subscribe(t, preview.Digest, false)
		if _, err := f.db.AuthorizeBackgroundUseAsPerson("sam", req); err != nil {
			t.Fatal(err)
		}
		if err := f.db.ApproveAgentSubscription(req.Subscription, "mika"); err != nil {
			t.Fatal(err)
		}
		if _, err := f.db.AuthorizeBackgroundUse(context.Background(), "sam-mail", req.Subscription, 1); err != nil {
			t.Fatal(err)
		}
		if err := f.db.WidenConnection("sam-mail", false, false, false); err != nil {
			t.Fatal(err)
		}
		mustDeny(t, f, req.Subscription, "cloud narrowed")
	})

	t.Run("a stored authorization cannot be edited", func(t *testing.T) {
		f := newMailFixture(t)
		a, _ := f.authorize(t)
		if _, err := f.db.ExecForTest(`UPDATE personal_background_authorizations SET target_calendar = 'other' WHERE seq = ?`, a.Seq); err == nil {
			t.Fatal("a material field of an authorization was edited in place")
		}
		if err := f.db.RevokeBackgroundAuthorization("sam-mail", "sam", "done"); err != nil {
			t.Fatal(err)
		}
		if _, err := f.db.ExecForTest(`UPDATE personal_background_authorizations SET revoked_at = '' WHERE seq = ?`, a.Seq); err == nil {
			t.Fatal("a revoked authorization was revived")
		}
	})
}

// Replacement stages the new version on every node BEFORE fencing the
// old one, then the old copies must be proven erased.
func TestReplacementStagesBeforeFencingThenErases(t *testing.T) {
	f := newMailFixture(t)
	before, _ := f.db.GetConnection("sam-mail")
	staged, err := f.db.StagePersonalCredential("sam-mail", "", "sam@example.internal", "sam", []string{"n1", "n2"})
	if err != nil {
		t.Fatal(err)
	}
	// Not on every node yet: cannot activate, and v1 is untouched.
	if err := f.db.MarkCredentialCopy(staged.Seq, "n1", true, 1); err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.db.ActivatePersonalCredential(staged.Seq, "sam"); err == nil {
		t.Fatal("a credential missing from n2 was activated")
	}
	if cur, _ := f.db.ActivePersonalCredential("sam-mail", ""); cur.Seq != f.credV1.Seq {
		t.Fatal("the working credential moved before its replacement was everywhere")
	}
	if mid, _ := f.db.GetConnection("sam-mail"); mid.LeaseEpoch != before.LeaseEpoch {
		t.Fatal("staging fenced the working credential early")
	}
	if err := f.db.MarkCredentialCopy(staged.Seq, "n2", true, 1); err != nil {
		t.Fatal(err)
	}
	active, superseded, err := f.db.ActivatePersonalCredential(staged.Seq, "sam")
	if err != nil {
		t.Fatal(err)
	}
	if active.Version != 2 || len(superseded) != 1 || superseded[0] != f.credV1.Seq {
		t.Fatalf("activate = %+v %v", active, superseded)
	}
	after, _ := f.db.GetConnection("sam-mail")
	if after.LeaseEpoch <= before.LeaseEpoch {
		t.Fatal("the old credential was not fenced")
	}
	if !strings.HasPrefix(after.DegradedReason, ErasureUnverifiedReason) || after.State != ConnectionDegraded {
		t.Fatalf("connection = %s %q, want degraded erasure_unverified", after.State, after.DegradedReason)
	}
	pending, err := f.db.OutstandingCredentialErasures("sam-mail")
	if err != nil || len(pending) != 2 {
		t.Fatalf("pending = %+v, %v", pending, err)
	}
	for _, p := range pending {
		if p.SecretRef != f.credV1.SecretRef || p.MaxVersion != 1 {
			t.Fatalf("pending erase = %+v", p)
		}
	}
	// One proven, one not: still degraded, naming the node.
	if err := f.db.RecordCredentialErasure(f.credV1.Seq, "n1", true, "nonce:evidence", ""); err != nil {
		t.Fatal(err)
	}
	if err := f.db.RecordCredentialErasure(f.credV1.Seq, "n2", false, "", "node unreachable"); err != nil {
		t.Fatal(err)
	}
	c, _ := f.db.GetConnection("sam-mail")
	if c.State != ConnectionDegraded || !strings.Contains(c.DegradedReason, "n2") || strings.Contains(c.DegradedReason, "n1") {
		t.Fatalf("connection = %s %q", c.State, c.DegradedReason)
	}
	// A proven erasure cannot be reopened.
	if _, err := f.db.ExecForTest(`UPDATE integration_secret_copies SET erase_state = 'pending' WHERE secret = ? AND node = 'n1'`, f.credV1.Seq); err == nil {
		t.Fatal("a proven erasure was reopened")
	}
	if err := f.db.RecordCredentialErasure(f.credV1.Seq, "n2", true, "nonce:evidence", ""); err != nil {
		t.Fatal(err)
	}
	c, _ = f.db.GetConnection("sam-mail")
	if c.State == ConnectionDegraded {
		t.Fatalf("every copy is proven erased, connection still %q", c.DegradedReason)
	}
}

func TestRevocationFencesFirstThenErases(t *testing.T) {
	f := newMailFixture(t)
	_, sub := f.authorize(t)
	if _, err := f.db.EnqueueIntegrationWork("sam-mail", IntegrationWork{Kind: WorkQuery, Capability: "mail.changes"}, time.Minute); err != nil {
		t.Fatal(err)
	}
	// An Owner may withdraw (administration), even though they could
	// never authorize.
	n, err := f.db.RevokePersonalCredentials("sam-mail", "mika", "disabled by an Owner")
	if err != nil || n != 1 {
		t.Fatalf("revoke = %d, %v", n, err)
	}
	mustDeny(t, f, sub, "credential revoked")
	if _, err := f.db.ActivePersonalCredential("sam-mail", ""); !errors.Is(err, ErrNoActiveCredential) {
		t.Fatal("a revoked credential is still active")
	}
	if _, err := f.db.LiveBackgroundAuthorization("sam-mail"); !errors.Is(err, ErrNotFound) {
		t.Fatal("the authorization survived its credential's revocation")
	}
	placements, _ := f.db.ListPlacements("sam-mail")
	for _, p := range placements {
		if p.SecretProvisioned || p.Prepared() {
			t.Fatalf("%s still counts as holding the revoked secret", p.Node)
		}
	}
	c, _ := f.db.GetConnection("sam-mail")
	if c.State != ConnectionDegraded || !strings.HasPrefix(c.DegradedReason, ErasureUnverifiedReason) {
		t.Fatalf("connection = %s %q", c.State, c.DegradedReason)
	}
	for _, node := range []string{"n1", "n2"} {
		if err := f.db.RecordCredentialErasure(f.credV1.Seq, node, true, "nonce:evidence", ""); err != nil {
			t.Fatal(err)
		}
	}
	c, _ = f.db.GetConnection("sam-mail")
	if c.State != ConnectionConfigured {
		t.Fatalf("after proven erasure the connection is %s %q, want configured", c.State, c.DegradedReason)
	}
	var journalled int
	f.db.sql.QueryRow(`SELECT COUNT(*) FROM deletion_journal WHERE subject_kind = 'secret' AND subject_id = ?`,
		f.credV1.SecretRef).Scan(&journalled)
	if journalled == 0 {
		t.Fatal("the revocation was not journalled for restore replay")
	}
}

// A placement removal cannot be proven complete on a caller's word while
// Keystone's own credential copies there lack erase evidence.
func TestPlacementRemovalNeedsCredentialEvidence(t *testing.T) {
	f := newMailFixture(t)
	if err := f.db.BeginPlacementRemoval("sam-mail", "n2"); err != nil {
		t.Fatal(err)
	}
	complete, err := f.db.ProvePlacementRemoval("sam-mail", "n2", true, true, true)
	if err != nil {
		t.Fatal(err)
	}
	if complete {
		t.Fatal("removal proven without the node's erase evidence")
	}
	c, _ := f.db.GetConnection("sam-mail")
	if !strings.Contains(c.DegradedReason, ErasureUnverifiedReason) {
		t.Fatalf("degraded reason = %q", c.DegradedReason)
	}
	if err := f.db.RecordCredentialErasure(f.credV1.Seq, "n2", true, "nonce:evidence", ""); err != nil {
		t.Fatal(err)
	}
	if complete, err = f.db.ProvePlacementRemoval("sam-mail", "n2", true, true, true); err != nil || !complete {
		t.Fatalf("with evidence, removal = %v, %v", complete, err)
	}
}

func TestOffboardingFencesCredentialsAndLeavesErasureVisible(t *testing.T) {
	f := newMailFixture(t)
	_, sub := f.authorize(t)
	result, err := f.db.OffboardMember("sam", "root-admin", "mika")
	if err != nil {
		t.Fatal(err)
	}
	if result.Stores["credentials-revoked"] != 1 || result.Stores["credential-erasures-pending"] != 2 ||
		result.Stores["background-authorizations-revoked"] != 1 {
		t.Fatalf("offboarding stores = %v", result.Stores)
	}
	mustDeny(t, f, sub, "offboarded")
	c, _ := f.db.GetConnection("sam-mail")
	if c.State != ConnectionDegraded || !strings.HasPrefix(c.DegradedReason, ErasureUnverifiedReason) {
		t.Fatalf("connection = %s %q", c.State, c.DegradedReason)
	}
	for _, node := range []string{"n1", "n2"} {
		if err := f.db.RecordCredentialErasure(f.credV1.Seq, node, true, "nonce:evidence", ""); err != nil {
			t.Fatal(err)
		}
	}
	c, _ = f.db.GetConnection("sam-mail")
	if c.State != ConnectionUnavailable {
		t.Fatalf("an offboarded Person's connection after erasure = %s, want unavailable", c.State)
	}
}

// A snapshot taken before a revocation cannot revive the credential or
// the authorization: quarantine holds use, and replay re-fences.
func TestRestoreCannotReviveCredentialUse(t *testing.T) {
	f := newMailFixture(t)
	a, sub := f.authorize(t)
	// Take the snapshot while everything is live.
	snapshot := f.db.path + ".snapshot"
	if _, err := f.db.ExecForTest(`VACUUM INTO ?`, snapshot); err != nil {
		t.Fatal(err)
	}
	// After the snapshot: the Person revokes.
	if _, err := f.db.RevokePersonalCredentials("sam-mail", "sam", "stop"); err != nil {
		t.Fatal(err)
	}
	var journal [][3]string
	rows, err := f.db.sql.Query(`SELECT subject_kind, subject_id, action FROM deletion_journal`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var e [3]string
		rows.Scan(&e[0], &e[1], &e[2])
		journal = append(journal, e)
	}
	rows.Close()

	restored, err := Open(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { restored.Close(); os.Remove(snapshot) })
	r := &mailFixture{db: restored, agent: f.agent, credV1: f.credV1}
	if err := restored.EnterRestoreQuarantine("restored from before the revocation", time.Now().Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	// Quarantined: the restored authorization is live on paper and still
	// cannot be used.
	mustDeny(t, r, sub, "quarantined restore")
	// The journal is brought current from the newer source, then replayed.
	for _, e := range journal {
		if _, err := restored.ExecForTest(`INSERT INTO deletion_journal (subject_kind, subject_id, action, actor, at)
			VALUES (?, ?, ?, 'sam', ?)`, e[0], e[1], e[2], time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
			t.Fatal(err)
		}
	}
	if err := restored.AssertJournalCurrent("mika"); err != nil {
		t.Fatal(err)
	}
	if _, err := restored.ReconcileRestore(); err != nil {
		t.Fatal(err)
	}
	if err := restored.ReleaseRestoreQuarantine("mika"); err != nil {
		t.Fatal(err)
	}
	mustDeny(t, r, sub, "after replay")
	if got, _ := restored.GetBackgroundAuthorization(a.Seq); got.RevokedAt.IsZero() {
		t.Fatal("replay left the authorization live")
	}
	c, _ := restored.GetConnection("sam-mail")
	if c.State != ConnectionDegraded || !strings.HasPrefix(c.DegradedReason, ErasureUnverifiedReason) {
		t.Fatalf("restored connection = %s %q, want erasure_unverified", c.State, c.DegradedReason)
	}
}

// The value has nowhere to go: no column can hold it, and none of the
// store's reads return it.
func TestNoCredentialValueColumn(t *testing.T) {
	db := open(t)
	for _, table := range []string{"integration_secrets", "integration_secret_copies", "personal_background_authorizations"} {
		rows, err := db.sql.Query(`SELECT name FROM pragma_table_info(?)`, table)
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			var col string
			rows.Scan(&col)
			for _, bad := range []string{"value", "password", "plaintext", "ciphertext"} {
				if strings.Contains(col, bad) {
					t.Errorf("%s.%s could hold a credential value", table, col)
				}
			}
		}
		rows.Close()
	}
}
