// Package resource defines Keystone's typed machine-layer records (ADR-W004
// A1): the Resource envelope, the four S2 kind-specific specs (Container,
// Volume, Route, DnsRecord), a closed kind registry, YAML (de)serialization,
// and validation.
//
// status (observedGeneration, conditions) is a runtime-DB concept only and
// is never part of Resource or its serialized form — every Statefile commit
// is an intentional spec change (ADR-W004 A1/A2).
package resource

import (
	"fmt"
	"regexp"
)

// Kind is the closed set of resource kinds Keystone understands. Adding a
// kind is a code change (ADR-W004 A1), mirroring the closed-op-surface
// philosophy of ADR-W003.
type Kind string

const (
	KindContainer Kind = "Container"
	KindVolume    Kind = "Volume"
	KindRoute     Kind = "Route"
	KindDnsRecord Kind = "DnsRecord"
)

// ApiVersion is the only supported apiVersion for S2 resources.
const ApiVersion = "wildstone.dev/v1"

// digestPinRegexp matches a fully digest-pinned image reference. Reused
// verbatim from ADR-W003 A4.2 / the Wardstone agent's invariant enforcement
// so Keystone rejects tag-pinned images before they ever reach the wire.
var digestPinRegexp = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9./_:-]*@sha256:[a-f0-9]{64}$`)

// Metadata is the ownership/placement envelope shared by every resource.
type Metadata struct {
	// Name is unique per kind.
	Name string `yaml:"name"`
	// Owner is the ownership chain: "capability/<name>" or "system".
	// Nothing is orphaned (ADR-W004 A1).
	Owner string `yaml:"owner"`
	// Node is the Wardstone node id this resource is applied to. Empty for
	// kinds that are not node-scoped (e.g. DnsRecord, applied via an
	// external-resource provider rather than Wardstone).
	Node string `yaml:"node,omitempty"`
}

// Mount is a single Container bind mount.
type Mount struct {
	Source   string `yaml:"source"`
	Target   string `yaml:"target"`
	ReadOnly bool   `yaml:"readOnly,omitempty"`
}

// ContainerSpec is the spec for kind Container, applied via Wardstone
// EnsureContainer/StopContainer/RemoveContainer.
type ContainerSpec struct {
	// Image must be digest-pinned (ADR-W003 A4.2 regex).
	Image string            `yaml:"image"`
	Env   map[string]string `yaml:"env,omitempty"`
	// Ports are "host:container" pairs.
	Ports   []string `yaml:"ports,omitempty"`
	Mounts  []Mount  `yaml:"mounts,omitempty"`
	Restart string   `yaml:"restart,omitempty"`
}

// VolumeSpec is the spec for kind Volume, applied via Wardstone EnsureVolume.
type VolumeSpec struct {
	Name string `yaml:"name"`
}

// RouteSpec is the spec for kind Route, applied by rendering a managed Caddy
// config fragment via Wardstone WriteManagedFile.
type RouteSpec struct {
	Host string `yaml:"host"`
	// Upstream is "container/<name>:<port>" or "ip:port".
	Upstream string `yaml:"upstream"`
	// Auth is "none" or "authelia_oidc".
	Auth string `yaml:"auth"`
	// Classification is a mandatory policy input, e.g. "family_public".
	Classification string `yaml:"classification"`
}

// DnsRecordSpec is the spec for kind DnsRecord, applied via the external-
// resource provider (Cloudflare) rather than Wardstone (ADR-W004 A5).
type DnsRecordSpec struct {
	Zone string `yaml:"zone"`
	Name string `yaml:"name"`
	// Type is one of "A", "CNAME", "TXT".
	Type    string `yaml:"type"`
	Content string `yaml:"content"`
	Proxied bool   `yaml:"proxied,omitempty"`
}

// dnsRecordTypes is the closed set of supported DnsRecord types.
var dnsRecordTypes = map[string]bool{
	"A":     true,
	"CNAME": true,
	"TXT":   true,
}

// Resource is the typed machine-layer record the Reconciler drives
// (ADR-W004 A1). Exactly one of the Spec fields is populated, matching
// Metadata's Kind (enforced by Validate and by the (de)serialization
// helpers in yaml.go).
//
// status is deliberately absent: it lives only in the runtime DB and must
// never be written to the Statefile.
type Resource struct {
	ApiVersion string   `yaml:"apiVersion"`
	Kind       Kind     `yaml:"kind"`
	Metadata   Metadata `yaml:"metadata"`

	Container *ContainerSpec `yaml:"-"`
	Volume    *VolumeSpec    `yaml:"-"`
	Route     *RouteSpec     `yaml:"-"`
	DnsRecord *DnsRecordSpec `yaml:"-"`
}

// ValidationError is a typed validation failure, identifying the resource
// and the field that failed.
type ValidationError struct {
	Kind  Kind
	Name  string
	Field string
	Msg   string
}

func (e *ValidationError) Error() string {
	who := string(e.Kind)
	if e.Name != "" {
		who = fmt.Sprintf("%s/%s", e.Kind, e.Name)
	}
	return fmt.Sprintf("resource %s: field %s: %s", who, e.Field, e.Msg)
}

// kindEntry is the closed registry's per-kind descriptor: a factory that
// produces an empty spec pointer suitable for decoding into, and a
// validator that checks the fully-populated Resource.
type kindEntry struct {
	newSpec  func() any
	validate func(r *Resource) error
}

// registry is the closed Go map from Kind to its factory/validator.
// Adding a kind is a code change (ADR-W004 A1).
var registry = map[Kind]kindEntry{
	KindContainer: {
		newSpec:  func() any { return &ContainerSpec{} },
		validate: validateContainer,
	},
	KindVolume: {
		newSpec:  func() any { return &VolumeSpec{} },
		validate: validateVolume,
	},
	KindRoute: {
		newSpec:  func() any { return &RouteSpec{} },
		validate: validateRoute,
	},
	KindDnsRecord: {
		newSpec:  func() any { return &DnsRecordSpec{} },
		validate: validateDnsRecord,
	},
}

// KnownKind reports whether k is in the closed registry.
func KnownKind(k Kind) bool {
	_, ok := registry[k]
	return ok
}

// Validate checks common fields (kind known, name, owner) plus the
// kind-specific rules from ADR-W004 A1, returning a *ValidationError (or a
// wrapped list) on failure.
func (r *Resource) Validate() error {
	entry, ok := registry[r.Kind]
	if !ok {
		return &ValidationError{Kind: r.Kind, Name: r.Metadata.Name, Field: "kind", Msg: "unknown kind"}
	}
	if r.Metadata.Name == "" {
		return &ValidationError{Kind: r.Kind, Field: "metadata.name", Msg: "must not be empty"}
	}
	if r.Metadata.Owner == "" {
		return &ValidationError{Kind: r.Kind, Name: r.Metadata.Name, Field: "metadata.owner", Msg: "must not be empty"}
	}
	if err := entry.validate(r); err != nil {
		return err
	}
	return nil
}

func validateContainer(r *Resource) error {
	if r.Container == nil {
		return &ValidationError{Kind: r.Kind, Name: r.Metadata.Name, Field: "spec", Msg: "container spec missing"}
	}
	if !digestPinRegexp.MatchString(r.Container.Image) {
		return &ValidationError{Kind: r.Kind, Name: r.Metadata.Name, Field: "spec.image", Msg: "must be digest-pinned (repo@sha256:<64 hex>), tags are refused"}
	}
	return nil
}

func validateVolume(r *Resource) error {
	if r.Volume == nil {
		return &ValidationError{Kind: r.Kind, Name: r.Metadata.Name, Field: "spec", Msg: "volume spec missing"}
	}
	if r.Volume.Name == "" {
		return &ValidationError{Kind: r.Kind, Name: r.Metadata.Name, Field: "spec.name", Msg: "must not be empty"}
	}
	return nil
}

func validateRoute(r *Resource) error {
	if r.Route == nil {
		return &ValidationError{Kind: r.Kind, Name: r.Metadata.Name, Field: "spec", Msg: "route spec missing"}
	}
	if r.Route.Classification == "" {
		return &ValidationError{Kind: r.Kind, Name: r.Metadata.Name, Field: "spec.classification", Msg: "must not be empty (mandatory policy input)"}
	}
	if r.Route.Host == "" {
		return &ValidationError{Kind: r.Kind, Name: r.Metadata.Name, Field: "spec.host", Msg: "must not be empty"}
	}
	return nil
}

func validateDnsRecord(r *Resource) error {
	if r.DnsRecord == nil {
		return &ValidationError{Kind: r.Kind, Name: r.Metadata.Name, Field: "spec", Msg: "dns record spec missing"}
	}
	if !dnsRecordTypes[r.DnsRecord.Type] {
		return &ValidationError{Kind: r.Kind, Name: r.Metadata.Name, Field: "spec.type", Msg: "must be one of A, CNAME, TXT"}
	}
	return nil
}
