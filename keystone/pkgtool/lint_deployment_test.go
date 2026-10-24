package pkgtool

import (
	"strings"
	"testing"

	"github.com/Wildstone-Systems/wildstone-homelab/keystone/pkgspec"
)

// deployPackage builds a one- or two-Container package for the
// deployment-capability lint (contract §4). head goes between params and
// templates (devices:, deployment:); app and side are each Container's
// extra spec lines, already indented; params are extra param lines.
func deployPackage(params, head, app, side string) string {
	pkg := `apiVersion: wildstone.dev/v1
kind: Package
metadata: { name: deploypkg, version: 1.0.0, capability: deploypkg, profile: default }
params:
  - { name: node, required: true }
` + params + head + `templates:
  - |
    apiVersion: wildstone.dev/v1
    kind: Container
    metadata: { name: deploy-app, node: "{{ .Params.node }}" }
    spec:
      image: "docker.io/library/nginx@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
` + app
	if side != "" {
		pkg += `  - |
    apiVersion: wildstone.dev/v1
    kind: Container
    metadata: { name: deploy-side, node: "{{ .Params.node }}" }
    spec:
      image: "docker.io/library/nginx@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
` + side
	}
	return pkg
}

func deployLint(t *testing.T, yaml string) []Issue {
	t.Helper()
	issues, err := Lint(writePackage(t, yaml), LintOptions{})
	if err != nil {
		t.Fatalf("Lint: %v", err)
	}
	var out []Issue
	for _, i := range issues {
		if i.Rule == "deployment" || i.Rule == "spec" || i.Rule == "expand" {
			out = append(out, i)
		}
	}
	return out
}

const relocateApp = "deployment:\n  components:\n    - { name: deploy-app, relocate: true }\n"

func TestDeploymentLintAllows(t *testing.T) {
	cases := []struct{ name, params, head, app, side string }{
		{"no fields at all, even with an unlabelled device (opt-in until catalog adoption)", "", "",
			"      devices: [{ source: /dev/ttyUSB0 }]\n", ""},
		{"a stateless component that may move and run as copies", "",
			"deployment:\n  components:\n    - { name: deploy-app, copies: many, relocate: true }\n", "", ""},
		{"a class device on a relocating component", "  - { name: gpu, default: /dev/dri }\n",
			"devices:\n  - { param: gpu, kind: class, class: gpu }\n" + relocateApp,
			"      devices: [{ source: \"{{ .Params.gpu }}\" }]\n", ""},
		{"/dev/net/tun labelled any-node", "",
			"devices:\n  - { path: /dev/net/tun, kind: any-node }\n" + relocateApp,
			"      devices: [{ source: /dev/net/tun }]\n", ""},
		{"a folder from a source: directory param", "  - { name: media, default: /srv/wildstone/media, source: directory }\n",
			relocateApp, "      mounts: [{ source: \"{{ .Params.media }}\", target: /media, readOnly: true }]\n", ""},
		{"Keystone-managed config under /etc/wildstone", "", relocateApp,
			"      mounts: [{ source: /etc/wildstone/app, target: /config, readOnly: true }]\n", ""},
		{"copies over read-only mounts", "",
			"deployment:\n  components:\n    - { name: deploy-app, copies: many }\n",
			"      mounts: [{ source: /etc/wildstone/app, target: /config, readOnly: true }]\n", ""},
		{"a worker beside an unlisted main", "",
			"deployment:\n  components:\n    - { name: deploy-side, role: worker, copies: many, relocate: true }\n", "", "      env: { ROLE: worker }\n"},
		{"a component name with a template expression", "",
			"deployment:\n  components:\n    - { name: \"deploy-{{ if .Params.node }}app{{ end }}\", relocate: true }\n", "", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if issues := deployLint(t, deployPackage(c.params, c.head, c.app, c.side)); len(issues) != 0 {
				t.Fatalf("issues = %v, want none", issues)
			}
		})
	}
}

func TestDeploymentLintRefuses(t *testing.T) {
	cases := []struct{ name, params, head, app, side, want string }{
		{"an unlabelled device once the package opts in", "", relocateApp,
			"      devices: [{ source: /dev/ttyUSB0 }]\n", "", "which no devices entry labels"},
		{"relocate with a specific device", "",
			"devices:\n  - { path: /dev/ttyUSB0, kind: identity }\n" + relocateApp,
			"      devices: [{ source: /dev/ttyUSB0 }]\n", "", "a specific device"},
		// Frigate's case: the Coral is empty by default, so a defaults-only
		// check sees no device at all.
		{"relocate with a specific device only a param switches on",
			"  - { name: coral, default: \"\" }\n",
			"devices:\n  - { param: coral, kind: identity }\n" + relocateApp,
			"      devices:\n        {{- with .Params.coral }}\n        - { source: \"{{ . }}\" }\n        {{- end }}\n        {{- if not .Params.coral }} []{{ end }}\n", "",
			"the device from param coral, a specific device"},
		{"relocate with a fixed host folder, even read-only", "", relocateApp,
			"      mounts: [{ source: /srv/wildstone/media, target: /media, readOnly: true }]\n", "", "fixed host folder"},
		{"copies over a read-write volume", "",
			"deployment:\n  components:\n    - { name: deploy-app, copies: many }\n",
			"      mounts: [{ source: app-data, target: /data }]\n", "", "copies cannot share that state"},
		{"copies with host networking", "",
			"deployment:\n  components:\n    - { name: deploy-app, copies: many }\n",
			"      networkMode: host\n", "", "same ports"},
		{"a component that renders nowhere", "",
			"deployment:\n  components:\n    - { name: deploy-ap, relocate: true }\n", "", "", "matches no rendered Container"},
		{"only workers", "",
			"deployment:\n  components:\n    - { name: deploy-app, role: worker }\n", "", "", "needs a main component"},
		{"an unknown device kind", "",
			"devices:\n  - { path: /dev/ttyUSB0, kind: pinned }\n", "      devices: [{ source: /dev/ttyUSB0 }]\n", "", "has kind \"pinned\""},
		{"a class device without its class", "  - { name: gpu, default: /dev/dri }\n",
			"devices:\n  - { param: gpu, kind: class }\n", "      devices: [{ source: \"{{ .Params.gpu }}\" }]\n", "", "class names the node label"},
		{"an unknown copies value", "",
			"deployment:\n  components:\n    - { name: deploy-app, copies: replicated }\n", "", "", "copies \"replicated\""},
		{"a free-text param that switches on host networking",
			"  - { name: mode, default: \"\", example: host }\n", relocateApp,
			"      {{- if eq .Params.mode \"host\" }}\n      networkMode: host\n      {{- end }}\n", "", "param \"mode\" is free text"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			issues := Failures(deployLint(t, deployPackage(c.params, c.head, c.app, c.side)))
			for _, i := range issues {
				if strings.Contains(i.Msg, c.want) {
					return
				}
			}
			t.Fatalf("issues = %v, want one containing %q", issues, c.want)
		})
	}
}

// Host networking with relocate is the owner's call to make, not lint's
// (contract §9 decision 3): a warning that does not fail lint. An enum
// that switches it on is found, because every enum value is rendered.
func TestDeploymentLintWarnsOnHostNetworkRelocation(t *testing.T) {
	yaml := deployPackage("  - { name: bluetooth, default: off, validate: \"enum:off|host\" }\n", relocateApp,
		"      {{- if eq .Params.bluetooth \"host\" }}\n      networkMode: host\n      {{- end }}\n", "")
	issues := deployLint(t, yaml)
	if len(Failures(issues)) != 0 {
		t.Fatalf("failures = %v, want none", Failures(issues))
	}
	if len(issues) != 1 || !issues[0].Warning || !strings.Contains(issues[0].Msg, "host's network") {
		t.Fatalf("issues = %v, want one host-network warning", issues)
	}
	if !strings.HasPrefix(issues[0].String(), "warning: ") {
		t.Fatalf("a warning prints as %q", issues[0].String())
	}
}

func TestDeploymentFieldStructure(t *testing.T) {
	for _, c := range []struct{ name, head, want string }{
		{"both param and path", "devices:\n  - { param: node, path: /dev/ttyUSB0, kind: identity }\n", "exactly one of param or path"},
		{"neither", "devices:\n  - { kind: identity }\n", "exactly one of param or path"},
		{"an undeclared param", "devices:\n  - { param: radio, kind: identity }\n", "not a declared param"},
		{"a nameless component", "deployment:\n  components:\n    - { relocate: true }\n", "must not be empty"},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, err := pkgspec.Parse([]byte(deployPackage("", c.head, "", "")))
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			issues := deployLint(t, deployPackage("", c.head, "", ""))
			if len(issues) != 1 || issues[0].Rule != "spec" || !strings.Contains(issues[0].Msg, c.want) {
				t.Fatalf("issues = %v, want one spec issue containing %q", issues, c.want)
			}
		})
	}
}
