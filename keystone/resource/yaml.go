package resource

import (
	"bytes"
	"fmt"

	"gopkg.in/yaml.v3"
)

// decodeNodeStrict decodes node into out using strict (KnownFields)
// decoding. yaml.v3's Node.Decode does not accept decoder options and
// always decodes non-strictly, even when the outer Decoder that produced
// the node had KnownFields(true) set — so a node-scoped decode has to
// re-serialize the node and run it back through a fresh strict Decoder to
// get unknown-key rejection.
func decodeNodeStrict(node *yaml.Node, out any) error {
	b, err := yaml.Marshal(node)
	if err != nil {
		return err
	}
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	return dec.Decode(out)
}

// resourceDoc is the on-disk shape: apiVersion/kind/metadata plus a single
// generic "spec" node that gets decoded into the kind-specific spec type.
// status is never part of this shape.
type resourceDoc struct {
	ApiVersion string    `yaml:"apiVersion"`
	Kind       Kind      `yaml:"kind"`
	Metadata   Metadata  `yaml:"metadata"`
	Spec       yaml.Node `yaml:"spec"`
}

// specOf returns the resource's populated spec value (kind-specific), or
// nil if none is set.
func (r *Resource) specOf() any {
	switch r.Kind {
	case KindContainer:
		return r.Container
	case KindVolume:
		return r.Volume
	case KindRoute:
		return r.Route
	case KindDnsRecord:
		return r.DnsRecord
	case KindOidcClient:
		return r.OidcClient
	case KindMatrixAppService:
		return r.MatrixAppService
	case KindLdapGroup:
		return r.LdapGroup
	case KindLdapUser:
		return r.LdapUser
	case KindIngress:
		return r.Ingress
	case KindBackupPlan:
		return r.BackupPlan
	case KindConfigFile:
		return r.ConfigFile
	case KindSeedFile:
		return r.SeedFile
	case KindJob:
		return r.Job
	case KindSecret:
		return r.Secret
	case KindAutomation:
		return r.Automation
	case KindMeter:
		return r.Meter
	case KindCommitment:
		return r.Commitment
	default:
		return nil
	}
}

// MarshalYAML implements yaml.Marshaler, serializing the resource envelope
// plus its kind-specific spec into a single "spec" node. status is not part
// of Resource and therefore never appears in the output.
func (r Resource) MarshalYAML() (any, error) {
	doc := struct {
		ApiVersion string   `yaml:"apiVersion"`
		Kind       Kind     `yaml:"kind"`
		Metadata   Metadata `yaml:"metadata"`
		Spec       any      `yaml:"spec"`
	}{
		ApiVersion: r.ApiVersion,
		Kind:       r.Kind,
		Metadata:   r.Metadata,
		Spec:       r.specOf(),
	}
	return doc, nil
}

// UnmarshalYAML implements yaml.Unmarshaler, decoding the envelope and then
// the "spec" node into the concrete kind-specific spec type looked up from
// the closed registry.
func (r *Resource) UnmarshalYAML(value *yaml.Node) error {
	var doc resourceDoc
	if err := decodeNodeStrict(value, &doc); err != nil {
		return err
	}
	r.ApiVersion = doc.ApiVersion
	r.Kind = doc.Kind
	r.Metadata = doc.Metadata

	entry, ok := registry[doc.Kind]
	if !ok {
		return fmt.Errorf("resource: unknown kind %q", doc.Kind)
	}
	spec := entry.newSpec()
	if doc.Spec.Kind != 0 {
		if err := decodeNodeStrict(&doc.Spec, spec); err != nil {
			return fmt.Errorf("resource: decoding spec for kind %q: %w", doc.Kind, err)
		}
	}
	switch doc.Kind {
	case KindContainer:
		r.Container = spec.(*ContainerSpec)
	case KindVolume:
		r.Volume = spec.(*VolumeSpec)
	case KindRoute:
		r.Route = spec.(*RouteSpec)
	case KindDnsRecord:
		r.DnsRecord = spec.(*DnsRecordSpec)
	case KindOidcClient:
		r.OidcClient = spec.(*OidcClientSpec)
	case KindMatrixAppService:
		r.MatrixAppService = spec.(*MatrixAppServiceSpec)
	case KindLdapGroup:
		r.LdapGroup = spec.(*LdapGroupSpec)
	case KindLdapUser:
		r.LdapUser = spec.(*LdapUserSpec)
	case KindIngress:
		r.Ingress = spec.(*IngressSpec)
	case KindBackupPlan:
		r.BackupPlan = spec.(*BackupPlanSpec)
	case KindConfigFile:
		r.ConfigFile = spec.(*ConfigFileSpec)
	case KindSeedFile:
		r.SeedFile = spec.(*SeedFileSpec)
	case KindJob:
		r.Job = spec.(*JobSpec)
	case KindSecret:
		r.Secret = spec.(*SecretSpec)
	case KindAutomation:
		r.Automation = spec.(*AutomationSpec)
	case KindMeter:
		r.Meter = spec.(*MeterSpec)
	case KindCommitment:
		r.Commitment = spec.(*CommitmentSpec)
	}
	return nil
}

// Marshal serializes a single Resource to YAML bytes (one document).
func Marshal(r Resource) ([]byte, error) {
	return yaml.Marshal(r)
}

// Unmarshal decodes a single YAML document into a Resource.
func Unmarshal(data []byte) (Resource, error) {
	var r Resource
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&r); err != nil {
		return Resource{}, err
	}
	return r, nil
}
