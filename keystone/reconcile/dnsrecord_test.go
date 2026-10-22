package reconcile

// End-to-end DnsRecord coverage: the Reconciler driven purely through the
// Provider path (ADR-W004 A5, S2c) against the Cloudflare fake — no
// gateway/agent involved at all, since DnsRecord never touches Applier.

import (
	"context"
	"testing"
	"time"

	"github.com/Wildstone-Systems/wildstone-homelab/keystone/plan"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/provider"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/provider/cloudflare"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/resource"
)

func dnsRecordResource(name, zone, rtype, content string, proxied bool) resource.Resource {
	return resource.Resource{
		ApiVersion: resource.ApiVersion,
		Kind:       resource.KindDnsRecord,
		Metadata:   resource.Metadata{Name: name, Owner: "system"},
		DnsRecord:  &resource.DnsRecordSpec{Zone: zone, Name: name, Type: rtype, Content: content, Proxied: proxied},
	}
}

func newDnsReconciler(p provider.Provider) *Reconciler {
	return &Reconciler{
		Providers:          map[resource.Kind]provider.Provider{resource.KindDnsRecord: p},
		MaxApplyRetries:    3,
		RetryBackoff:       5 * time.Millisecond,
		HealthGateAttempts: 5,
		HealthGateBackoff:  5 * time.Millisecond,
	}
}

func TestReconcileCreatesDnsRecordEndToEnd(t *testing.T) {
	f := newFakeCFForReconcileTest(t)
	f.addZone("example.com")
	p := &cloudflare.Provider{APIToken: "tok", BaseURL: f.URL(), MaxAttempts: 4}
	r := newDnsReconciler(p)

	desired := []resource.Resource{dnsRecordResource("app.example.com", "example.com", "A", "1.2.3.4", false)}
	outcome, err := r.Reconcile(context.Background(), "", desired)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if !outcome.Applied || outcome.PendingApproval || outcome.Rolledback || outcome.Incident != "" {
		t.Fatalf("unexpected outcome: %+v", outcome)
	}
	if outcome.Plan.OverallRisk != plan.RiskLow {
		t.Fatalf("expected Low risk for a new DnsRecord, got %v", outcome.Plan.OverallRisk)
	}
	if len(outcome.Plan.Steps) != 1 || outcome.Plan.Steps[0].Action != plan.ActionCreate {
		t.Fatalf("expected a single create step, got: %+v", outcome.Plan.Steps)
	}

	obs, exists, err := p.Observe(context.Background(), desired[0])
	if err != nil || !exists {
		t.Fatalf("record not created: exists=%v err=%v", exists, err)
	}
	if obs.DnsRecord.Content != "1.2.3.4" {
		t.Fatalf("Content = %q, want 1.2.3.4", obs.DnsRecord.Content)
	}
}

func TestReconcileDnsRecordNoOpOnReconverge(t *testing.T) {
	f := newFakeCFForReconcileTest(t)
	f.addZone("example.com")
	p := &cloudflare.Provider{APIToken: "tok", BaseURL: f.URL(), MaxAttempts: 4}
	r := newDnsReconciler(p)

	desired := []resource.Resource{dnsRecordResource("app.example.com", "example.com", "A", "1.2.3.4", false)}
	if outcome, err := r.Reconcile(context.Background(), "", desired); err != nil || !outcome.Applied {
		t.Fatalf("initial reconcile: outcome=%+v err=%v", outcome, err)
	}

	outcome, err := r.Reconcile(context.Background(), "", desired)
	if err != nil {
		t.Fatalf("second reconcile: %v", err)
	}
	if !outcome.Applied || len(outcome.Plan.Steps) != 0 {
		t.Fatalf("expected a no-op reconcile, got: %+v", outcome)
	}
}

func TestReconcileDnsRecordDriftRepairs(t *testing.T) {
	f := newFakeCFForReconcileTest(t)
	zoneID := f.addZone("example.com")
	p := &cloudflare.Provider{APIToken: "tok", BaseURL: f.URL(), MaxAttempts: 4}
	r := newDnsReconciler(p)

	desired := []resource.Resource{dnsRecordResource("app.example.com", "example.com", "A", "1.2.3.4", false)}
	if outcome, err := r.Reconcile(context.Background(), "", desired); err != nil || !outcome.Applied {
		t.Fatalf("initial reconcile: outcome=%+v err=%v", outcome, err)
	}

	// Out-of-band drift: the record changes directly at Cloudflare.
	f.setRecord(zoneID, "A", "app.example.com", "9.9.9.9", true)

	// A DnsRecord Update is Medium risk (ADR-W004 A3: "may affect
	// resolution for its name") and so requires approval, unlike the
	// additive Low-risk Create test above.
	r.Approve = func(*plan.Plan) bool { return true }

	outcome, err := r.Reconcile(context.Background(), "", desired)
	if err != nil {
		t.Fatalf("repair reconcile: %v", err)
	}
	if len(outcome.Plan.Steps) != 1 || outcome.Plan.Steps[0].Action != plan.ActionUpdate {
		t.Fatalf("expected a single Update repair step, got: %+v", outcome.Plan.Steps)
	}
	if !outcome.Applied {
		t.Fatalf("drift repair was not applied: %+v", outcome)
	}

	rec, ok := f.getRecord(zoneID, "A", "app.example.com")
	if !ok || rec.content != "1.2.3.4" || rec.proxied {
		t.Fatalf("record was not reconverged: %+v ok=%v", rec, ok)
	}
}

func TestReconcileDnsRecordPartialFailureRetryIsIdempotent(t *testing.T) {
	f := newFakeCFForReconcileTest(t)
	zoneID := f.addZone("example.com")
	p := &cloudflare.Provider{APIToken: "tok", BaseURL: f.URL(), MaxAttempts: 4}
	r := newDnsReconciler(p)

	// The read-back GET right after Create's POST fails once with a 5xx —
	// cloudflare.go surfaces that as a *provider.Transient (ADR-W004 A5
	// B8), which retryProvider now retries in-place (same bounded budget
	// as RateLimited) instead of failing the step outright. The retried
	// Create() call's own pre-create lookup then finds the record the
	// first POST already made (matching spec) and converges without a
	// second POST — so this now resolves within a single Reconcile call,
	// where it used to require the caller to re-invoke Reconcile on a
	// later tick. The important assertion is unchanged: exactly one
	// record, never a duplicate.
	f.armFailGetCall(3) // see armFailGetCall doc: call 3 is the post-POST read-back through the full Reconcile path

	desired := []resource.Resource{dnsRecordResource("app.example.com", "example.com", "A", "1.2.3.4", false)}
	outcome, err := r.Reconcile(context.Background(), "", desired)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if !outcome.Applied || outcome.Incident != "" {
		t.Fatalf("expected the injected transient read-back failure to be absorbed by retryProvider, got: %+v", outcome)
	}
	if got := f.recordCount(zoneID, "A", "app.example.com"); got != 1 {
		t.Fatalf("expected exactly 1 record, got %d (duplicate!)", got)
	}
	if got := f.requestCount("POST", "/zones/{id}/dns_records"); got != 1 {
		t.Fatalf("expected exactly 1 POST despite the retry, got %d", got)
	}
}

func TestReconcileDnsRecordRateLimitBacksOffThenSucceeds(t *testing.T) {
	f := newFakeCFForReconcileTest(t)
	f.addZone("example.com")
	p := &cloudflare.Provider{APIToken: "tok", BaseURL: f.URL(), MaxAttempts: 4}
	r := newDnsReconciler(p)

	f.setRateLimit(2, 0) // Retry-After: 0 keeps this test's own sleeps trivial

	desired := []resource.Resource{dnsRecordResource("app.example.com", "example.com", "A", "1.2.3.4", false)}
	outcome, err := r.Reconcile(context.Background(), "", desired)
	if err != nil {
		t.Fatalf("reconcile should not fail fatally on a bounded rate-limit: %v", err)
	}
	if !outcome.Applied || outcome.Incident != "" {
		t.Fatalf("expected the rate limit to be absorbed by backoff+retry, got: %+v", outcome)
	}
}

// TestReconcileDnsRecordTransientBacksOffThenSucceeds mirrors the
// RateLimited case above for a transient 5xx (ADR-W004 A5 B8):
// retryProvider's bounded retry (MaxApplyRetries=3 here, via
// newDnsReconciler) must absorb a couple of 503s and still converge.
func TestReconcileDnsRecordTransientBacksOffThenSucceeds(t *testing.T) {
	f := newFakeCFForReconcileTest(t)
	f.addZone("example.com")
	p := &cloudflare.Provider{APIToken: "tok", BaseURL: f.URL(), MaxAttempts: 4}
	r := newDnsReconciler(p)

	f.setServerError(2) // fewer 503s than MaxApplyRetries=3 -> absorbed

	desired := []resource.Resource{dnsRecordResource("app.example.com", "example.com", "A", "1.2.3.4", false)}
	outcome, err := r.Reconcile(context.Background(), "", desired)
	if err != nil {
		t.Fatalf("reconcile should not fail fatally on a bounded transient failure: %v", err)
	}
	if !outcome.Applied || outcome.Incident != "" {
		t.Fatalf("expected the transient failure to be absorbed by backoff+retry, got: %+v", outcome)
	}
}

// TestReconcileDnsRecordTransientExhaustsToIncident confirms the other
// half of the contract: once transient 503s outlast retryProvider's
// bounded budget (MaxApplyRetries=3), the failure is no longer silently
// absorbed — it surfaces as an Outcome.Incident, same as a permanent
// provider failure would.
//
// The outage is armed to start only after the Reconciler's observe
// pre-pass (2 requests: resolve zone + find-by-key) has already
// succeeded, so the persistent failure hits during apply — where a
// provider error is reported through sr.Error/Outcome.Incident. (An
// observe-phase exhaustion instead makes Reconcile return the error
// directly, per observeProviders' doc comment — a different, and already
// covered, error path.)
func TestReconcileDnsRecordTransientExhaustsToIncident(t *testing.T) {
	f := newFakeCFForReconcileTest(t)
	f.addZone("example.com")
	p := &cloudflare.Provider{APIToken: "tok", BaseURL: f.URL(), MaxAttempts: 4}
	r := newDnsReconciler(p)

	f.setServerErrorAfter(2, 1000) // let observe succeed, then outage never lets up

	desired := []resource.Resource{dnsRecordResource("app.example.com", "example.com", "A", "1.2.3.4", false)}
	outcome, err := r.Reconcile(context.Background(), "", desired)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if outcome.Incident == "" {
		t.Fatalf("expected exhausted transient retries to surface as an Incident: %+v", outcome)
	}
	if outcome.Applied {
		t.Fatalf("must not report Applied when apply never succeeded: %+v", outcome)
	}
}
