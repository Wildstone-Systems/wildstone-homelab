package pkgtool

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/chickenrat17/wildstone-homelab/keystone/pkgspec"
	"github.com/chickenrat17/wildstone-homelab/keystone/resource"
)

// The checks in this file all answer one question the rest of lint does
// not ask: does what the package SAYS match what the app actually DOES?
//
// Everything else lint checks is internal consistency — a secret that is
// declared, a mount that names a Volume this package creates, a Route
// whose upstream is a container it runs. Three services shipped with
// every one of those checks passing and none of them working:
//
//   - calibre-web got TZ=CST, a real abbreviation and not a zone. The
//     app died at startup; its s6 init held PID 1, so Docker said Up and
//     the proxy said 502.
//   - immich-server got nine OAUTH_* env vars. Immich has not read OAuth
//     from the environment since it moved those settings into its system
//     config; every one was inert, and the login page simply had no
//     button on it.
//   - open-webui got WEBUI_URL=https://chat.wildstone.internal, a name
//     that resolves nowhere on a tailnet deployment.
//
// None is a malformed package. Each is a package that was never compared
// against the thing it configures.

// paramShapeIssues checks a package's OWN values against the shapes it
// declares — the Default it ships and the Example it documents.
//
// The value an operator supplies is checked in capability.Expand, which
// is the check that matters. This one exists so an author learns at
// `wild pkg lint` that the default they wrote cannot pass, rather than
// from a stranger's failed install.
func paramShapeIssues(pkg *pkgspec.Package) []Issue {
	var issues []Issue
	for _, decl := range pkg.Params {
		if decl.Validate == "" {
			continue
		}
		for label, v := range map[string]string{"default": decl.Default, "example": decl.Example} {
			if v == "" {
				continue
			}
			if err := pkgspec.ValidateParamValue(decl.Validate, v); err != nil {
				issues = append(issues, Issue{Rule: "param-validate", Msg: fmt.Sprintf(
					"param %q %s: %v", decl.Name, label, err)})
			}
		}
	}
	sort.Slice(issues, func(i, j int) bool { return issues[i].Msg < issues[j].Msg })
	return issues
}

// selfURLFromDomain finds an env value that builds the app's own
// browser-visible URL by concatenating the domain params.
//
// The rule packages/README.md already states for exports — "use origin,
// never string concatenation" — is the same rule here, and until
// recently it was unenforceable in a template because `origin` was not
// in scope for one. It is now, so this can be a finding rather than a
// paragraph.
//
// What makes the concatenated form wrong is that a Route is not always
// served at the host it declares (ADR-W019 A10). Under the tailnet
// posture every service shares one certified name and differs by port,
// so `https://chat.` + public_domain names a host nothing answers —
// while every existing check still passes, because none of them compares
// a rendered value against what is actually served.
//
// Scoped to env values on purpose. A Route's `host:` is the declared
// host and SHOULD be built this way — it is the input origin resolves
// FROM. An OidcClient's redirectUris are compensated downstream, where
// the reconciler adds the resolved form alongside the declared one.
func selfURLFromDomain(pkg *pkgspec.Package) []Issue {
	var issues []Issue
	for i, tmpl := range pkg.Templates {
		for _, line := range envLines(tmpl) {
			if !schemeThenTemplate.MatchString(line) {
				continue
			}
			if strings.Contains(line, "origin") {
				continue // already asking where it is served
			}
			if !strings.Contains(line, ".Params.") {
				continue
			}
			key := strings.TrimSpace(strings.SplitN(strings.TrimSpace(line), ":", 2)[0])
			issues = append(issues, Issue{Rule: "self-url", Msg: fmt.Sprintf(
				"templates[%d] env %s builds a browser-visible URL by concatenating params. "+
					"A Route is not always served at the host it declares (ADR-W019 A10): under the tailnet "+
					"posture that host resolves nowhere. Use `{{ origin \"host\" }}`, which answers where the "+
					"host is actually served", i, key)})
		}
	}
	return issues
}

// schemeThenTemplate matches a URL literal whose host comes from a
// template action: https://{{ ... }} and https://sub.{{ ... }} alike.
var schemeThenTemplate = regexp.MustCompile(`https?://[^"'\s]*\{\{`)

// envLines returns the lines of a template that sit under an `env:` key,
// by indentation. Text scanning rather than YAML parsing because the
// template has not been rendered yet and does not parse as YAML until it
// has — which is the whole point of checking here, where the author's
// spelling is still visible.
func envLines(tmpl string) []string {
	var out []string
	envIndent := -1
	for _, line := range strings.Split(tmpl, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		indent := len(line) - len(strings.TrimLeft(line, " "))
		if envIndent >= 0 {
			if indent > envIndent {
				out = append(out, line)
				continue
			}
			envIndent = -1 // dedented out of the env block
		}
		if trimmed == "env:" {
			envIndent = indent
		}
	}
	return out
}

// inertEnv is what a given image IGNORES: env vars an author can set,
// that reconcile cleanly, that the process reads nothing from.
//
// A curated table, and it has to be. Nothing in a manifest can tell you
// which variables a binary consults; the only source is the app's own
// documentation, read by someone integrating it. This is where that
// reading gets written down so the next author — or the next model —
// does not have to repeat it, and cannot skip it.
//
// Keyed by a substring of the image reference so a digest pin still
// matches. Add a row when you discover one; the cost of a wrong row is
// a false positive an author can read and argue with, which is much
// cheaper than the silence it replaces.
var inertEnv = []struct {
	image  string
	prefix []string
	why    string
}{
	{
		image:  "immich-app/immich-server",
		prefix: []string{"OAUTH_", "PUBLIC_LOGIN_PAGE_MESSAGE", "IMMICH_WEB_URL"},
		why: "Immich reads OAuth and most feature settings from its system config — the database, " +
			"or the JSON file named by IMMICH_CONFIG_FILE — not from the environment. Env vars set here " +
			"reconcile cleanly and do nothing: the effective config Immich logs at startup shows " +
			"\"oauth\": false and the login page has no button. Render a ConfigFile and point " +
			"IMMICH_CONFIG_FILE at it",
	},
}

// inertEnvIssues reports env vars a package sets on an image that does
// not read them.
func inertEnvIssues(r resource.Resource) []Issue {
	if r.Kind != resource.KindContainer || r.Container == nil {
		return nil
	}
	var issues []Issue
	for _, rule := range inertEnv {
		if !strings.Contains(r.Container.Image, rule.image) {
			continue
		}
		var hit []string
		for name := range r.Container.Env {
			for _, p := range rule.prefix {
				if strings.HasPrefix(name, p) {
					hit = append(hit, name)
					break
				}
			}
		}
		if len(hit) == 0 {
			continue
		}
		sort.Strings(hit)
		issues = append(issues, Issue{Rule: "inert-env", Msg: fmt.Sprintf(
			"Container/%s sets %s, which this image ignores. %s",
			r.Metadata.Name, strings.Join(hit, ", "), rule.why)})
	}
	return issues
}
