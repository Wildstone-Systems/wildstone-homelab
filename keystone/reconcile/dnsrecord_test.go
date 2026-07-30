package reconcile

// End-to-end DnsRecord coverage: the Reconciler driven purely through the
// Provider path (ADR-W004 A5, S2c) against the Cloudflare fake — no
// gateway/agent involved at all, since DnsRecord never touches Applier.

import (
	"context"
	"testing"
	"time"

	"github.com/chickenrat17/wildstone-homelab/keystone/plan"
	"github.com/chickenrat17/wildstone-homelab/keystone/provider"
	"github.com/chickenrat17/wildstone-homelab/keystone/provider/cloudflare"
	"github.com/chickenrat17/wildstone-homelab/keystone/resource"
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

	// The read-back GET right after Create's POST fails once — Create()
	// itself surfaces this as an error (not rate-limited), so retryProvider
	// won't retry it (that's only for RateLimited); the Reconciler's own
	// step therefore fails on the first attempt inside applyDnsRecordStep,
	// which does not itself loop Create calls beyond retryProvider's
	// rate-limit-only retry. Model the retry the way the reconcile loop
	// actually gets one: re-invoke Reconcile (a fresh pass), simulating a
	// caller (Keystone's scheduler) retrying on the next tick after a
	// transient failure — the important assertion is that this produces
	// exactly one record, never a duplicate.
	f.armFailGetCall(3) // see armFailGetCall doc: call 3 is the post-POST read-back through the full Reconcile path

	desired := []resource.Resource{dnsRecordResource("app.example.com", "example.com", "A", "1.2.3.4", false)}
	outcome, err := r.Reconcile(context.Background(), "", desired)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if outcome.Incident == "" {
		t.Fatalf("expected the injected read-back failure to surface as an Incident: %+v", outcome)
	}
	if got := f.recordCount(zoneID, "A", "app.example.com"); got != 1 {
		t.Fatalf("expected exactly 1 record after the failed read-back, got %d", got)
	}

	// Retry via a fresh Reconcile call: must be idempotent.
	outcome, err = r.Reconcile(context.Background(), "", desired)
	if err != nil {
		t.Fatalf("retried reconcile: %v", err)
	}
	if !outcome.Applied {
		t.Fatalf("retried reconcile should have converged: %+v", outcome)
	}
	if got := f.recordCount(zoneID, "A", "app.example.com"); got != 1 {
		t.Fatalf("expected exactly 1 record after retry, got %d (duplicate!)", got)
	}
	if got := f.requestCount("POST", "/zones/{id}/dns_records"); got != 1 {
		t.Fatalf("expected exactly 1 POST across both reconcile attempts, got %d", got)
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
