package pkgspec

import "fmt"

// Deployment capability fields (ADR-W005 A5; the package deployment
// capabilities contract, docs/design/package-deployment-capabilities-
// contract.md). They say what a package's workload needs from a machine
// and what it can safely do across machines. Nothing reads them at run
// time yet: the Docker backend runs every package on its one named node.
//
// As with AccessGroups, the vocabulary below is checked by lint, not
// here, so a manifest written against a newer list still loads. The
// fields themselves are new to the format, and the parser rejects
// unknown fields, so catalog packages adopt them only a release after
// this parser ships (contract §7).

// Device kinds: what one mapped device is.
const (
	// DeviceIdentity is this exact physical device (a Zigbee stick): the
	// component cannot move while it is mapped.
	DeviceIdentity = "identity"
	// DeviceClass is any device of a type, named by Class (a GPU): the
	// component may move to a machine labelled with that class.
	DeviceClass = "class"
	// DeviceAnyNode is a kernel-provided node every Linux machine has
	// (/dev/net/tun): it does not tie the component to a machine.
	DeviceAnyNode = "any-node"
)

// DeviceKinds is the closed set of device kinds.
var DeviceKinds = map[string]bool{DeviceIdentity: true, DeviceClass: true, DeviceAnyNode: true}

// Component copies and roles.
const (
	CopiesOne  = "one"
	CopiesMany = "many"
	RoleMain   = "main"
	RoleWorker = "worker"
)

// ComponentCopies and ComponentRoles are the closed sets; empty means the
// conservative default (one copy, main).
var (
	ComponentCopies = map[string]bool{CopiesOne: true, CopiesMany: true}
	ComponentRoles  = map[string]bool{RoleMain: true, RoleWorker: true}
)

// DeviceDecl labels one device a template can map: the value of a param
// (Param) or a path written in the template (Path), exactly one of them.
type DeviceDecl struct {
	Param string `yaml:"param,omitempty" json:"param,omitempty"`
	Path  string `yaml:"path,omitempty" json:"path,omitempty"`
	Kind  string `yaml:"kind" json:"kind"`
	// Class is the node label a DeviceClass device needs, e.g. "gpu".
	Class string `yaml:"class,omitempty" json:"class,omitempty"`
}

// DeploymentSpec declares what each component can safely do. A rendered
// Container it does not list gets the defaults: one copy, does not move,
// main.
type DeploymentSpec struct {
	Components []ComponentDecl `yaml:"components,omitempty" json:"components,omitempty"`
}

// ComponentDecl is one Container's declared capabilities.
type ComponentDecl struct {
	// Name is the Container's metadata.name after rendering; it may use
	// template expressions over .Params, rendered with the same values.
	Name string `yaml:"name" json:"name"`
	// Copies is "many" when several copies may run at once.
	Copies string `yaml:"copies,omitempty" json:"copies,omitempty"`
	// Relocate is true when the component may stop on one machine and
	// start on another eligible one.
	Relocate bool `yaml:"relocate,omitempty" json:"relocate,omitempty"`
	// Role is "worker" for a component that does extra work for a main
	// component of the same package.
	Role string `yaml:"role,omitempty" json:"role,omitempty"`
}

// EnumValues returns the closed choices of an `enum:` param, or false.
func (d ParamDecl) EnumValues() ([]string, bool) {
	return enumShapeValues(d.Validate)
}

// validateDeployment checks the fields' structure. Values are lint's.
func validateDeployment(p *Package) error {
	params := map[string]bool{}
	for _, d := range p.Params {
		params[d.Name] = true
	}
	for i, d := range p.Devices {
		field := fmt.Sprintf("devices[%d]", i)
		if (d.Param == "") == (d.Path == "") {
			return &ValidationError{Field: field, Msg: "name exactly one of param or path"}
		}
		if d.Param != "" && !params[d.Param] {
			return &ValidationError{Field: field + ".param", Msg: fmt.Sprintf("%q is not a declared param", d.Param)}
		}
		if d.Kind == "" {
			return &ValidationError{Field: field + ".kind", Msg: "must not be empty"}
		}
	}
	if p.Deployment != nil {
		for i, c := range p.Deployment.Components {
			if c.Name == "" {
				return &ValidationError{Field: fmt.Sprintf("deployment.components[%d].name", i), Msg: "must not be empty"}
			}
		}
	}
	return nil
}
