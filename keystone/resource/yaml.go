package resource

import (
	"fmt"

	"gopkg.in/yaml.v3"
)

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
	if err := value.Decode(&doc); err != nil {
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
		if err := doc.Spec.Decode(spec); err != nil {
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
	if err := yaml.Unmarshal(data, &r); err != nil {
		return Resource{}, err
	}
	return r, nil
}
