// Package cloudflare implements keystone/provider.Provider for
// resource.KindDnsRecord against the Cloudflare API v4 (ADR-W004 A5/S2c).
//
// Idempotency key: a DNS record's identity is (zone, name, type) — never
// the Cloudflare-assigned record ID, which this package never persists
// anywhere outside a single call. Create is therefore find-by-key-then-
// create-or-update: a retry after a partial failure (POST succeeded, the
// follow-up read-back failed) finds the record that already exists on the
// retry's own find-by-key lookup and updates it (or no-ops if it already
// matches) instead of creating a duplicate.
//
// Rate limits: every request goes through do(), which retries on HTTP 429
// honoring the Retry-After header (seconds), up to MaxAttempts (default 4)
// tries. If still rate-limited after that, do() returns a
// *provider.RateLimited the caller (keystone/reconcile) treats as
// retryable-later, not fatal.
//
// Transient server errors: an HTTP 5xx from Cloudflare is returned as a
// *provider.Transient — the same "retryable-later, not fatal" treatment
// as RateLimited, but for the ordinary "the control plane had a blip"
// case rather than exhausted rate-limit budget.
//
// The API token is never logged: no code path in this package writes it to
// an error string, and errors here only ever include response bodies
// (which never contain the token — CF doesn't echo it back).
package cloudflare

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/chickenrat17/wildstone-homelab/keystone/provider"
	"github.com/chickenrat17/wildstone-homelab/keystone/resource"
)

// DefaultBaseURL is Cloudflare's real API v4 base. Tests override BaseURL
// to point at an httptest fake.
const DefaultBaseURL = "https://api.cloudflare.com/client/v4"

// sleepFn is the backoff seam: overridden by tests to a no-op so the
// rate-limit/backoff suite doesn't actually sleep real seconds.
var sleepFn = time.Sleep

// Provider is a Cloudflare DNS provider for resource.KindDnsRecord.
type Provider struct {
	// APIToken authenticates every request as "Authorization: Bearer
	// <token>". Never logged.
	APIToken string
	// BaseURL overrides DefaultBaseURL; tests point this at an httptest
	// server. Empty uses DefaultBaseURL.
	BaseURL string
	// HTTPClient is the client used for every request. nil uses
	// http.DefaultClient.
	HTTPClient *http.Client
	// MaxAttempts bounds the number of tries for a single logical request
	// before giving up on rate-limiting and returning
	// *provider.RateLimited. <= 0 uses a default of 4.
	MaxAttempts int

	mu    sync.Mutex
	zones map[string]string // zone name -> zone id, cached for process lifetime
}

func (p *Provider) client() *http.Client {
	if p.HTTPClient != nil {
		return p.HTTPClient
	}
	return http.DefaultClient
}

func (p *Provider) baseURL() string {
	if p.BaseURL != "" {
		return p.BaseURL
	}
	return DefaultBaseURL
}

func (p *Provider) maxAttempts() int {
	if p.MaxAttempts > 0 {
		return p.MaxAttempts
	}
	return 4
}

// ---------------------------------------------------------------- Cloudflare API v4 shapes

type cfEnvelope struct {
	Success bool            `json:"success"`
	Errors  []cfAPIError    `json:"errors"`
	Result  json.RawMessage `json:"result"`
}

type cfAPIError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e cfAPIError) String() string { return fmt.Sprintf("%d: %s", e.Code, e.Message) }

type cfZone struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type cfDNSRecord struct {
	ID      string `json:"id,omitempty"`
	Type    string `json:"type"`
	Name    string `json:"name"`
	Content string `json:"content"`
	Proxied bool   `json:"proxied"`
}

// ---------------------------------------------------------------- Provider interface

// Observe returns the live Cloudflare record matching r's identity key
// (zone, name, type), if any.
func (p *Provider) Observe(ctx context.Context, r resource.Resource) (*resource.Resource, bool, error) {
	spec, err := dnsSpec(r)
	if err != nil {
		return nil, false, err
	}
	zoneID, err := p.resolveZone(ctx, spec.Zone)
	if err != nil {
		return nil, false, err
	}
	rec, err := p.findRecord(ctx, zoneID, spec.Name, spec.Type)
	if err != nil {
		return nil, false, err
	}
	if rec == nil {
		return nil, false, nil
	}
	observed := r
	observed.DnsRecord = &resource.DnsRecordSpec{
		Zone:    spec.Zone,
		Name:    rec.Name,
		Type:    rec.Type,
		Content: rec.Content,
		Proxied: rec.Proxied,
	}
	return &observed, true, nil
}

// Create creates r's DNS record. It is create-or-update by (zone, name,
// type): if a record already matches the identity key (e.g. this is a
// retry after a partial failure), it is updated to match r's spec (or left
// alone if it already matches) instead of a duplicate being created.
func (p *Provider) Create(ctx context.Context, r resource.Resource) error {
	spec, err := dnsSpec(r)
	if err != nil {
		return err
	}
	zoneID, err := p.resolveZone(ctx, spec.Zone)
	if err != nil {
		return err
	}

	existing, err := p.findRecord(ctx, zoneID, spec.Name, spec.Type)
	if err != nil {
		return fmt.Errorf("cloudflare: create %s/%s %s: pre-create lookup: %w", spec.Name, spec.Type, spec.Zone, err)
	}
	if existing != nil {
		if existing.Content == spec.Content && existing.Proxied == spec.Proxied {
			return nil // already converged (e.g. a retried Create after the original actually succeeded)
		}
		return p.updateByID(ctx, zoneID, existing.ID, spec)
	}

	body := cfDNSRecord{Type: spec.Type, Name: spec.Name, Content: spec.Content, Proxied: spec.Proxied}
	url := fmt.Sprintf("%s/zones/%s/dns_records", p.baseURL(), zoneID)
	env, err := p.do(ctx, http.MethodPost, url, body, "create dns_record")
	if err != nil {
		return err
	}
	if !env.Success {
		return fmt.Errorf("cloudflare: create %s/%s %s: %v", spec.Name, spec.Type, spec.Zone, env.Errors)
	}

	// Read-back: confirm the record is actually there. If this fails
	// (5xx/transport), the POST may still have succeeded — do NOT treat
	// that as "no record was created". Return the error; a retried Create
	// finds the record via the pre-create lookup above and converges
	// without duplicating it.
	if _, err := p.findRecord(ctx, zoneID, spec.Name, spec.Type); err != nil {
		return fmt.Errorf("cloudflare: create %s/%s %s: read-back after create failed (safe to retry): %w", spec.Name, spec.Type, spec.Zone, err)
	}
	return nil
}

// Update updates the existing record matching r's identity key. If none
// exists (e.g. it was deleted out-of-band since Observe), it falls back to
// Create so Update stays idempotent too.
func (p *Provider) Update(ctx context.Context, r resource.Resource) error {
	spec, err := dnsSpec(r)
	if err != nil {
		return err
	}
	zoneID, err := p.resolveZone(ctx, spec.Zone)
	if err != nil {
		return err
	}
	existing, err := p.findRecord(ctx, zoneID, spec.Name, spec.Type)
	if err != nil {
		return fmt.Errorf("cloudflare: update %s/%s %s: lookup: %w", spec.Name, spec.Type, spec.Zone, err)
	}
	if existing == nil {
		return p.Create(ctx, r)
	}
	return p.updateByID(ctx, zoneID, existing.ID, spec)
}

// Delete removes the record matching r's identity key. Absent is treated
// as success (idempotent).
func (p *Provider) Delete(ctx context.Context, r resource.Resource) error {
	spec, err := dnsSpec(r)
	if err != nil {
		return err
	}
	zoneID, err := p.resolveZone(ctx, spec.Zone)
	if err != nil {
		return err
	}
	existing, err := p.findRecord(ctx, zoneID, spec.Name, spec.Type)
	if err != nil {
		return fmt.Errorf("cloudflare: delete %s/%s %s: lookup: %w", spec.Name, spec.Type, spec.Zone, err)
	}
	if existing == nil {
		return nil
	}
	url := fmt.Sprintf("%s/zones/%s/dns_records/%s", p.baseURL(), zoneID, existing.ID)
	env, err := p.do(ctx, http.MethodDelete, url, nil, "delete dns_record")
	if err != nil {
		return err
	}
	if !env.Success {
		return fmt.Errorf("cloudflare: delete %s/%s %s: %v", spec.Name, spec.Type, spec.Zone, env.Errors)
	}
	return nil
}

func (p *Provider) updateByID(ctx context.Context, zoneID, id string, spec *resource.DnsRecordSpec) error {
	body := cfDNSRecord{Type: spec.Type, Name: spec.Name, Content: spec.Content, Proxied: spec.Proxied}
	url := fmt.Sprintf("%s/zones/%s/dns_records/%s", p.baseURL(), zoneID, id)
	env, err := p.do(ctx, http.MethodPut, url, body, "update dns_record")
	if err != nil {
		return err
	}
	if !env.Success {
		return fmt.Errorf("cloudflare: update %s/%s %s: %v", spec.Name, spec.Type, spec.Zone, env.Errors)
	}
	return nil
}

// ---------------------------------------------------------------- zone resolution

func (p *Provider) resolveZone(ctx context.Context, zoneName string) (string, error) {
	p.mu.Lock()
	if id, ok := p.zones[zoneName]; ok {
		p.mu.Unlock()
		return id, nil
	}
	p.mu.Unlock()

	url := fmt.Sprintf("%s/zones?name=%s", p.baseURL(), zoneName)
	env, err := p.do(ctx, http.MethodGet, url, nil, "resolve zone")
	if err != nil {
		return "", err
	}
	if !env.Success {
		return "", fmt.Errorf("cloudflare: resolve zone %s: %v", zoneName, env.Errors)
	}
	var zones []cfZone
	if err := json.Unmarshal(env.Result, &zones); err != nil {
		return "", fmt.Errorf("cloudflare: resolve zone %s: decoding response: %w", zoneName, err)
	}
	if len(zones) == 0 {
		return "", fmt.Errorf("cloudflare: zone %q not found", zoneName)
	}

	p.mu.Lock()
	if p.zones == nil {
		p.zones = map[string]string{}
	}
	p.zones[zoneName] = zones[0].ID
	p.mu.Unlock()
	return zones[0].ID, nil
}

// findRecord looks up the (at most one, by construction — Create/Update
// never allow more than one record per (zone,name,type)) record matching
// name+type, or returns nil if none exists.
func (p *Provider) findRecord(ctx context.Context, zoneID, name, rtype string) (*cfDNSRecord, error) {
	url := fmt.Sprintf("%s/zones/%s/dns_records?type=%s&name=%s", p.baseURL(), zoneID, rtype, name)
	env, err := p.do(ctx, http.MethodGet, url, nil, "list dns_records")
	if err != nil {
		return nil, err
	}
	if !env.Success {
		return nil, fmt.Errorf("cloudflare: list dns_records %s %s: %v", rtype, name, env.Errors)
	}
	var records []cfDNSRecord
	if err := json.Unmarshal(env.Result, &records); err != nil {
		return nil, fmt.Errorf("cloudflare: list dns_records %s %s: decoding response: %w", rtype, name, err)
	}
	if len(records) == 0 {
		return nil, nil
	}
	return &records[0], nil
}

// ---------------------------------------------------------------- transport + rate limiting

// do sends one logical request, retrying on HTTP 429 (honoring
// Retry-After, in seconds) up to maxAttempts() tries. If still
// rate-limited after that, it returns *provider.RateLimited. A 5xx status
// is returned as a *provider.Transient error — not retried here (that
// bounded retry lives in keystone/reconcile's retryProvider, same as it
// does for RateLimited) but distinguished from a genuine 4xx rejection so
// the caller knows it's safe to retry. Any other non-2xx (4xx other than
// 429) status is returned as a plain error (not retried — those are not
// transient, e.g. a genuinely invalid request).
func (p *Provider) do(ctx context.Context, method, url string, body any, op string) (*cfEnvelope, error) {
	var reqBody []byte
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("cloudflare: %s: encoding request: %w", op, err)
		}
		reqBody = b
	}

	maxAttempts := p.maxAttempts()
	var lastRetryAfter time.Duration
	var lastErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		req, err := http.NewRequestWithContext(ctx, method, url, bytes.NewReader(reqBody))
		if err != nil {
			return nil, fmt.Errorf("cloudflare: %s: building request: %w", op, err)
		}
		req.Header.Set("Authorization", "Bearer "+p.APIToken)
		req.Header.Set("Content-Type", "application/json")

		resp, err := p.client().Do(req)
		if err != nil {
			return nil, fmt.Errorf("cloudflare: %s: %w", op, err)
		}
		data, readErr := io.ReadAll(resp.Body)
		resp.Body.Close()
		if readErr != nil {
			return nil, fmt.Errorf("cloudflare: %s: reading response: %w", op, readErr)
		}

		if resp.StatusCode == http.StatusTooManyRequests {
			lastRetryAfter = parseRetryAfter(resp.Header.Get("Retry-After"))
			lastErr = fmt.Errorf("HTTP 429: %s", string(data))
			if attempt < maxAttempts {
				sleepFn(lastRetryAfter)
				continue
			}
			return nil, &provider.RateLimited{RetryAfter: lastRetryAfter, Op: op, Err: lastErr}
		}

		if resp.StatusCode >= 500 {
			return nil, &provider.Transient{Op: op, Err: fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(data))}
		}

		if resp.StatusCode >= 300 {
			return nil, fmt.Errorf("cloudflare: %s: HTTP %d: %s", op, resp.StatusCode, string(data))
		}

		var env cfEnvelope
		if err := json.Unmarshal(data, &env); err != nil {
			return nil, fmt.Errorf("cloudflare: %s: decoding response: %w", op, err)
		}
		return &env, nil
	}
	// Unreachable in practice (the 429 branch above always returns by the
	// final attempt), kept for completeness.
	return nil, &provider.RateLimited{RetryAfter: lastRetryAfter, Op: op, Err: lastErr}
}

// parseRetryAfter parses Cloudflare's Retry-After header (seconds, per RFC
// 7231 delta-seconds — CF does not send HTTP-date for this header). An
// unparseable or missing value defaults to 1 second so callers always back
// off at least a little.
func parseRetryAfter(v string) time.Duration {
	if v == "" {
		return time.Second
	}
	secs, err := strconv.Atoi(v)
	if err != nil || secs < 0 {
		return time.Second
	}
	return time.Duration(secs) * time.Second
}

func dnsSpec(r resource.Resource) (*resource.DnsRecordSpec, error) {
	if r.Kind != resource.KindDnsRecord || r.DnsRecord == nil {
		return nil, fmt.Errorf("cloudflare: not a DnsRecord resource: %s/%s", r.Kind, r.Metadata.Name)
	}
	return r.DnsRecord, nil
}
