// Package kubeapi is the small, typed Kubernetes API client that Keystone
// and Wardstone use for leader coordination (W050 slice 3b, safety
// contract #180 §2). It knows four things: Leases, ConfigMaps, a dry-run
// create and the API's error shape. It is deliberately not a general
// client: there is no way to send an arbitrary object, verb or path, so it
// can't become a manifest-shaped side door (ADR-W003). App workloads go
// through the typed leader operations instead (leader ops §1).
package kubeapi

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// Client talks to one API server as one identity.
type Client struct {
	base  string
	token func() (string, error)
	http  *http.Client
}

// New returns a client for the API server at base (https://host:port),
// trusting only the CA in caFile and authenticating with the bearer token
// read from tokenFile on every request, so a rotated token is picked up.
func New(base, caFile, tokenFile string) (*Client, error) {
	pem, err := os.ReadFile(caFile)
	if err != nil {
		return nil, fmt.Errorf("kubeapi: reading CA: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		return nil, fmt.Errorf("kubeapi: %s holds no PEM certificate", caFile)
	}
	hc := &http.Client{
		Timeout: 10 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12},
			Proxy:           nil, // never route cluster traffic through a proxy
		},
	}
	token := func() (string, error) {
		b, err := os.ReadFile(tokenFile)
		if err != nil {
			return "", fmt.Errorf("kubeapi: reading token: %w", err)
		}
		return strings.TrimSpace(string(b)), nil
	}
	return NewWithHTTP(base, token, hc)
}

// NewWithHTTP returns a client using hc and the given token source. Tests
// and callers with their own transport use it.
func NewWithHTTP(base string, token func() (string, error), hc *http.Client) (*Client, error) {
	u, err := url.Parse(base)
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") || u.Path != "" && u.Path != "/" {
		return nil, fmt.Errorf("kubeapi: %q is not an API server URL", base)
	}
	return &Client{base: strings.TrimSuffix(base, "/"), token: token, http: hc}, nil
}

// Server is the API server this client talks to.
func (c *Client) Server() string { return c.base }

// ObjectMeta is the part of metadata this package reads and writes.
type ObjectMeta struct {
	Name            string            `json:"name,omitempty"`
	GenerateName    string            `json:"generateName,omitempty"`
	Namespace       string            `json:"namespace,omitempty"`
	UID             string            `json:"uid,omitempty"`
	ResourceVersion string            `json:"resourceVersion,omitempty"`
	Labels          map[string]string `json:"labels,omitempty"`
	Annotations     map[string]string `json:"annotations,omitempty"`
}

// MicroTime is a Lease timestamp, which the API carries with microseconds.
type MicroTime struct{ time.Time }

const microTimeLayout = "2006-01-02T15:04:05.000000Z07:00"

// MarshalJSON writes the API's MicroTime form, or null for a zero time.
func (t MicroTime) MarshalJSON() ([]byte, error) {
	if t.IsZero() {
		return []byte("null"), nil
	}
	return json.Marshal(t.UTC().Format(microTimeLayout))
}

// UnmarshalJSON reads MicroTime, accepting plain RFC 3339 too.
func (t *MicroTime) UnmarshalJSON(b []byte) error {
	if string(b) == "null" {
		t.Time = time.Time{}
		return nil
	}
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}
	v, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return err
	}
	t.Time = v
	return nil
}

// LeaseSpec is coordination.k8s.io/v1 LeaseSpec.
type LeaseSpec struct {
	HolderIdentity       string     `json:"holderIdentity,omitempty"`
	LeaseDurationSeconds int32      `json:"leaseDurationSeconds,omitempty"`
	AcquireTime          *MicroTime `json:"acquireTime,omitempty"`
	RenewTime            *MicroTime `json:"renewTime,omitempty"`
	LeaseTransitions     int32      `json:"leaseTransitions,omitempty"`
}

// Lease is coordination.k8s.io/v1 Lease.
type Lease struct {
	APIVersion string     `json:"apiVersion"`
	Kind       string     `json:"kind"`
	Metadata   ObjectMeta `json:"metadata"`
	Spec       LeaseSpec  `json:"spec"`
}

// ConfigMap is v1 ConfigMap.
type ConfigMap struct {
	APIVersion string            `json:"apiVersion"`
	Kind       string            `json:"kind"`
	Metadata   ObjectMeta        `json:"metadata"`
	Data       map[string]string `json:"data,omitempty"`
}

// StatusError is the API server's refusal.
type StatusError struct {
	Code    int    `json:"code"`
	Reason  string `json:"reason"`
	Message string `json:"message"`
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("kubeapi: %d %s: %s", e.Code, e.Reason, e.Message)
}

// IsNotFound reports a 404.
func IsNotFound(err error) bool { return code(err) == http.StatusNotFound }

// IsConflict reports a 409: a resourceVersion that moved, or a create of
// an object that already exists.
func IsConflict(err error) bool { return code(err) == http.StatusConflict }

// IsForbidden reports a 403: RBAC or an admission policy refused it.
func IsForbidden(err error) bool { return code(err) == http.StatusForbidden }

func code(err error) int {
	var se *StatusError
	if errors.As(err, &se) {
		return se.Code
	}
	return 0
}

func leasePath(ns, name string) string {
	p := "/apis/coordination.k8s.io/v1/namespaces/" + url.PathEscape(ns) + "/leases"
	if name != "" {
		p += "/" + url.PathEscape(name)
	}
	return p
}

func configMapPath(ns, name string) string {
	p := "/api/v1/namespaces/" + url.PathEscape(ns) + "/configmaps"
	if name != "" {
		p += "/" + url.PathEscape(name)
	}
	return p
}

// GetLease reads one Lease.
func (c *Client) GetLease(ctx context.Context, ns, name string) (*Lease, error) {
	var l Lease
	return &l, c.do(ctx, http.MethodGet, leasePath(ns, name), nil, &l)
}

// CreateLease creates a Lease. IsConflict means it already exists.
func (c *Client) CreateLease(ctx context.Context, l *Lease) (*Lease, error) {
	l.APIVersion, l.Kind = "coordination.k8s.io/v1", "Lease"
	var out Lease
	return &out, c.do(ctx, http.MethodPost, leasePath(l.Metadata.Namespace, ""), l, &out)
}

// UpdateLease replaces a Lease. It needs the resourceVersion it read;
// IsConflict means someone else wrote it since.
func (c *Client) UpdateLease(ctx context.Context, l *Lease) (*Lease, error) {
	if l.Metadata.ResourceVersion == "" {
		return nil, errors.New("kubeapi: a Lease update needs the resourceVersion it read")
	}
	l.APIVersion, l.Kind = "coordination.k8s.io/v1", "Lease"
	var out Lease
	return &out, c.do(ctx, http.MethodPut, leasePath(l.Metadata.Namespace, l.Metadata.Name), l, &out)
}

// GetConfigMap reads one ConfigMap.
func (c *Client) GetConfigMap(ctx context.Context, ns, name string) (*ConfigMap, error) {
	var cm ConfigMap
	return &cm, c.do(ctx, http.MethodGet, configMapPath(ns, name), nil, &cm)
}

// UpdateConfigMap replaces a ConfigMap, compare-and-swap on the
// resourceVersion it read.
func (c *Client) UpdateConfigMap(ctx context.Context, cm *ConfigMap) (*ConfigMap, error) {
	if cm.Metadata.ResourceVersion == "" {
		return nil, errors.New("kubeapi: a ConfigMap update needs the resourceVersion it read")
	}
	cm.APIVersion, cm.Kind = "v1", "ConfigMap"
	var out ConfigMap
	return &out, c.do(ctx, http.MethodPut, configMapPath(cm.Metadata.Namespace, cm.Metadata.Name), cm, &out)
}

// DryRunCreateConfigMap asks the API server to admit a ConfigMap create
// without storing it (dryRun=All). Admission policies run as for a real
// write, so a nil error means this server would accept it now.
func (c *Client) DryRunCreateConfigMap(ctx context.Context, cm *ConfigMap) error {
	cm.APIVersion, cm.Kind = "v1", "ConfigMap"
	return c.do(ctx, http.MethodPost, configMapPath(cm.Metadata.Namespace, "")+"?dryRun=All", cm, nil)
}

// maxBody bounds what is read from the API server.
const maxBody = 1 << 20

func (c *Client) do(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, body)
	if err != nil {
		return err
	}
	tok, err := c.token()
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Accept", "application/json")
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("kubeapi: %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return fmt.Errorf("kubeapi: reading %s %s: %w", method, path, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		se := &StatusError{Code: resp.StatusCode}
		if json.Unmarshal(b, se) != nil || se.Code == 0 {
			se.Code = resp.StatusCode
		}
		if se.Message == "" {
			se.Message = http.StatusText(resp.StatusCode)
		}
		return se
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(b, out); err != nil {
		return fmt.Errorf("kubeapi: decoding %s %s: %w", method, path, err)
	}
	return nil
}
