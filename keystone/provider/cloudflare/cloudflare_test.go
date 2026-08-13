package cloudflare

import (
	"context"
	"testing"
	"time"

	"github.com/chickenrat17/wildstone-homelab/keystone/provider"
	"github.com/chickenrat17/wildstone-homelab/keystone/resource"
)

func dnsResource(zone, name, rtype, content string, proxied bool) resource.Resource {
	return resource.Resource{
		ApiVersion: resource.ApiVersion,
		Kind:       resource.KindDnsRecord,
		Metadata:   resource.Metadata{Name: name, Owner: "system"},
		DnsRecord:  &resource.DnsRecordSpec{Zone: zone, Name: name, Type: rtype, Content: content, Proxied: proxied},
	}
}

func newTestProvider(f *fakeCF) *Provider {
	return &Provider{
		APIToken:    "test-token",
		BaseURL:     f.URL(),
		MaxAttempts: 4,
	}
}

func TestCreateThenObserve(t *testing.T) {
	f := newFakeCF(t)
	f.addZone("example.com")
	p := newTestProvider(f)

	r := dnsResource("example.com", "app.example.com", "A", "1.2.3.4", false)
	if err := p.Create(context.Background(), r); err != nil {
		t.Fatalf("Create: %v", err)
	}

	obs, exists, err := p.Observe(context.Background(), r)
	if err != nil {
		t.Fatalf("Observe: %v", err)
	}
	if !exists {
		t.Fatal("expected record to exist after Create")
	}
	if obs.DnsRecord.Content != "1.2.3.4" {
		t.Errorf("Content = %q, want 1.2.3.4", obs.DnsRecord.Content)
	}
	if f.recordCount("zone-1", "A", "app.example.com") != 1 {
		t.Fatalf("expected exactly one record, got %d", f.recordCount("zone-1", "A", "app.example.com"))
	}
}

func TestObserveDrift(t *testing.T) {
	f := newFakeCF(t)
	zoneID := f.addZone("example.com")
	p := newTestProvider(f)

	r := dnsResource("example.com", "app.example.com", "A", "1.2.3.4", false)
	if err := p.Create(context.Background(), r); err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Out-of-band drift: someone changes the record directly at Cloudflare.
	f.setRecord(zoneID, "A", "app.example.com", "9.9.9.9", true)

	obs, exists, err := p.Observe(context.Background(), r)
	if err != nil {
		t.Fatalf("Observe: %v", err)
	}
	if !exists {
		t.Fatal("expected record to exist")
	}
	if obs.DnsRecord.Content != "9.9.9.9" || !obs.DnsRecord.Proxied {
		t.Errorf("Observe did not reflect drift: %+v", obs.DnsRecord)
	}

	// Reconverge: Update should bring it back to the desired spec.
	if err := p.Update(context.Background(), r); err != nil {
		t.Fatalf("Update: %v", err)
	}
	rec, ok := f.getRecord(zoneID, "A", "app.example.com")
	if !ok {
		t.Fatal("record vanished after Update")
	}
	if rec.content != "1.2.3.4" || rec.proxied != false {
		t.Errorf("record not reconverged: content=%q proxied=%v", rec.content, rec.proxied)
	}
}

func TestPartialFailureCreateRetryIsIdempotent(t *testing.T) {
	f := newFakeCF(t)
	zoneID := f.addZone("example.com")
	p := newTestProvider(f)

	r := dnsResource("example.com", "app.example.com", "A", "1.2.3.4", false)

	// The read-back GET immediately after the create POST fails once.
	f.failNextReadBack()

	err := p.Create(context.Background(), r)
	if err == nil {
		t.Fatal("expected Create to report the read-back failure")
	}

	// The POST itself DID succeed (only the read-back was injected to
	// fail), so exactly one record should already exist.
	if got := f.recordCount(zoneID, "A", "app.example.com"); got != 1 {
		t.Fatalf("expected exactly 1 record after the failed read-back, got %d", got)
	}

	// Retry: must be idempotent (find-by-key -> already matches -> no-op),
	// never a duplicate.
	if err := p.Create(context.Background(), r); err != nil {
		t.Fatalf("retried Create: %v", err)
	}
	if got := f.recordCount(zoneID, "A", "app.example.com"); got != 1 {
		t.Fatalf("expected exactly 1 record after retry, got %d (duplicate!)", got)
	}
	if got := f.requestCount("POST", "/zones/{id}/dns_records"); got != 1 {
		t.Errorf("expected exactly 1 POST (the retry should have updated-or-noop'd, not re-created), got %d", got)
	}
}

func TestPartialFailureCreateRetryWithDivergedContentUpdates(t *testing.T) {
	// A variant where between the failed read-back and the retry the
	// desired content changed — the retry's find-by-key must Update, not
	// no-op, and still never duplicate.
	f := newFakeCF(t)
	zoneID := f.addZone("example.com")
	p := newTestProvider(f)

	r1 := dnsResource("example.com", "app.example.com", "A", "1.2.3.4", false)
	f.failNextReadBack()
	if err := p.Create(context.Background(), r1); err == nil {
		t.Fatal("expected the injected read-back failure to surface")
	}

	r2 := dnsResource("example.com", "app.example.com", "A", "5.6.7.8", false)
	if err := p.Create(context.Background(), r2); err != nil {
		t.Fatalf("retried Create with diverged content: %v", err)
	}
	if got := f.recordCount(zoneID, "A", "app.example.com"); got != 1 {
		t.Fatalf("expected exactly 1 record, got %d", got)
	}
	rec, _ := f.getRecord(zoneID, "A", "app.example.com")
	if rec.content != "5.6.7.8" {
		t.Errorf("record content = %q, want 5.6.7.8", rec.content)
	}
}

func TestRateLimitBacksOffThenSucceeds(t *testing.T) {
	f := newFakeCF(t)
	f.addZone("example.com")
	p := newTestProvider(f)

	orig := sleepFn
	var slept []time.Duration
	sleepFn = func(d time.Duration) { slept = append(slept, d) }
	t.Cleanup(func() { sleepFn = orig })

	f.setRateLimit(2, 1) // first 2 requests across all endpoints 429

	r := dnsResource("example.com", "app.example.com", "A", "1.2.3.4", false)
	if err := p.Create(context.Background(), r); err != nil {
		t.Fatalf("Create should have backed off and succeeded, got: %v", err)
	}
	if len(slept) == 0 {
		t.Error("expected the provider to back off at least once")
	}
	for _, d := range slept {
		if d != time.Second {
			t.Errorf("expected a 1s backoff (from Retry-After: 1), got %s", d)
		}
	}
}

func TestRateLimitExhaustsToTypedError(t *testing.T) {
	f := newFakeCF(t)
	f.addZone("example.com")
	p := newTestProvider(f)
	p.MaxAttempts = 3

	orig := sleepFn
	sleepFn = func(time.Duration) {}
	t.Cleanup(func() { sleepFn = orig })

	f.setRateLimit(100, 1) // never lets up within the bounded attempts

	r := dnsResource("example.com", "app.example.com", "A", "1.2.3.4", false)
	err := p.Create(context.Background(), r)
	if err == nil {
		t.Fatal("expected an error")
	}
	if !provider.IsRateLimited(err) {
		t.Fatalf("expected a *provider.RateLimited error, got: %v (%T)", err, err)
	}
}

func TestServerErrorSurfacesAsTransient(t *testing.T) {
	f := newFakeCF(t)
	f.addZone("example.com")
	p := newTestProvider(f)

	f.setServerError(1) // one 503, not repeated — do() does not retry 5xx itself

	r := dnsResource("example.com", "app.example.com", "A", "1.2.3.4", false)
	err := p.Create(context.Background(), r)
	if err == nil {
		t.Fatal("expected an error")
	}
	if !provider.IsTransient(err) {
		t.Fatalf("expected a *provider.Transient error, got: %v (%T)", err, err)
	}
	if provider.IsRateLimited(err) {
		t.Fatalf("a 5xx must not be classified as RateLimited: %v", err)
	}
}

func TestIdempotencyKeyCreateTwiceIsOneRecord(t *testing.T) {
	f := newFakeCF(t)
	zoneID := f.addZone("example.com")
	p := newTestProvider(f)

	r := dnsResource("example.com", "app.example.com", "A", "1.2.3.4", false)
	if err := p.Create(context.Background(), r); err != nil {
		t.Fatalf("first Create: %v", err)
	}
	if err := p.Create(context.Background(), r); err != nil {
		t.Fatalf("second Create: %v", err)
	}
	if got := f.recordCount(zoneID, "A", "app.example.com"); got != 1 {
		t.Fatalf("expected exactly 1 record after 2 Creates with the same identity key, got %d", got)
	}
}

func TestDeleteIsIdempotent(t *testing.T) {
	f := newFakeCF(t)
	zoneID := f.addZone("example.com")
	p := newTestProvider(f)

	r := dnsResource("example.com", "app.example.com", "A", "1.2.3.4", false)
	if err := p.Create(context.Background(), r); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := p.Delete(context.Background(), r); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if got := f.recordCount(zoneID, "A", "app.example.com"); got != 0 {
		t.Fatalf("expected 0 records after Delete, got %d", got)
	}
	// Deleting again (already gone) must not error.
	if err := p.Delete(context.Background(), r); err != nil {
		t.Fatalf("second Delete (already absent): %v", err)
	}
}

func TestTokenNeverAppearsInErrors(t *testing.T) {
	f := newFakeCF(t)
	// No zone registered: resolveZone will fail.
	p := &Provider{APIToken: "super-secret-token", BaseURL: f.URL(), MaxAttempts: 2}

	r := dnsResource("nonexistent.example.com", "app.example.com", "A", "1.2.3.4", false)
	err := p.Create(context.Background(), r)
	if err == nil {
		t.Fatal("expected an error resolving an unregistered zone")
	}
	if containsToken(err.Error(), "super-secret-token") {
		t.Fatalf("API token leaked into error text: %v", err)
	}
}

func containsToken(s, tok string) bool {
	for i := 0; i+len(tok) <= len(s); i++ {
		if s[i:i+len(tok)] == tok {
			return true
		}
	}
	return false
}
