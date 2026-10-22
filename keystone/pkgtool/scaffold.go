package pkgtool

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/Wildstone-Systems/wildstone-homelab/keystone/pkgspec"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/trait"
)

// Scaffolding a new package (ADR-W025 Part B item 2).
//
// Everything a package needs to be correct here is already checkable —
// lint is the contract test, the trait vocabulary is listable, the
// catalog gate runs over every shipped package — and none of that helps
// with the blank file. An author's first act was to copy an existing
// package and delete the parts they did not recognise, which is how a
// scaffold gets built anyway, badly and once per person: the digest pin
// they forgot is a validation error, but the `auth: none` on a
// family_public Route they inherited from the wrong donor package is a
// policy violation, and the `container/` prefix in env is six shipped
// occurrences of a mistake that could never have started.
//
// So: `wild pkg new` writes a package that LINTS GREEN as it stands, with
// the idioms already in place and TODOs everywhere a decision belongs to
// the author. TestScaffoldLintsGreen is the whole point — a scaffold that
// does not lint is worse than none, because it teaches the reader that
// failing output is normal.

// Shape is one starting point for a new package: which of the three ways
// a service can reach Wildstone this package takes.
//
// The vocabulary is small and closed, and it is NOT (yet) a field in the
// manifest — a shape is a choice about what to write, made once, and the
// file it produces is an ordinary package with nothing declaring which
// scaffold it came from. Declaring the posture in metadata is its own
// decision (ADR-W025 Part B item 1) and is not made here.
type Shape struct {
	// Name is the -shape value.
	Name string
	// Summary is one line, printed by `wild pkg new` with no arguments.
	Summary string
	// Description says which real situation this shape is for, in the
	// terms the person choosing has: what they already run, and what
	// they are willing to change about it.
	Description string
	// PinsImages is true when this shape renders a Container, and so
	// carries a placeholder digest the author must replace.
	//
	// It exists so `wild pkg new` can offer `refresh-digests` to the
	// shapes that need it and stay quiet for the one that does not.
	// Advice that does not apply is how a reader learns to skim the
	// advice that does. TestShapeImageFlagMatchesTheTemplate pins it to
	// what the templates actually render.
	PinsImages bool
}

// Shapes are the starting points, in the order `wild pkg new` lists them:
// most work on the service's side first, least last.
var Shapes = []Shape{
	{
		Name:    "service",
		Summary: "Wildstone runs the app in a container, behind a Route, with single sign-on.",
		Description: "The shape of photos-immich and documents-paperless. You get a Volume, " +
			"a Container, a Route and an OidcClient, already wired to whichever package " +
			"provides identity:oidc. Choose this when Wildstone is what installs and runs " +
			"the service.",
		PinsImages: true,
	},
	{
		Name:    "sidecar",
		Summary: "The app runs elsewhere; a small adapter container watches it and publishes events.",
		Description: "Choose this when the service already runs somewhere Wildstone does not " +
			"manage AND cannot call a webhook itself — so something has to poll or subscribe " +
			"on its behalf. You write the adapter; this scaffolds the package that runs it " +
			"and hands it its intake credential.",
		PinsImages: true,
	},
	{
		Name:    "config",
		Summary: "The app runs elsewhere and can already call a webhook; ship it the configuration.",
		Description: "The shape of home-automation, and the cheapest integration there is: no " +
			"container, no adapter, no code on either side. Wildstone renders one managed " +
			"config file that tells the service where to POST and what token to present. " +
			"Choose this whenever the service can call a URL on its own.",
	},
}

// LookupShape finds a shape by name.
func LookupShape(name string) (Shape, bool) {
	for _, s := range Shapes {
		if s.Name == name {
			return s, true
		}
	}
	return Shape{}, false
}

// ShapeNames are the shape names in listing order, for "must be one of
// ..." messages.
func ShapeNames() []string {
	out := make([]string, 0, len(Shapes))
	for _, s := range Shapes {
		out = append(out, s.Name)
	}
	return out
}

// ScaffoldOptions describes the package to write.
type ScaffoldOptions struct {
	// Name is the package name and its directory name.
	Name string
	// Capability is the user-facing capability this package implements.
	// Empty means Name — right for a first package, and wrong the moment
	// a second profile of the same capability exists, which is why it is
	// a separate field rather than derived silently.
	Capability string
	// Profile names this particular implementation of the capability
	// ("authelia-lldap", "immich"). Empty means Name.
	Profile string
	// Shape is one of ShapeNames().
	Shape string
}

// scaffoldNameRegexp bounds a package name to what every downstream use
// of it accepts: it becomes a directory, resource metadata names, a
// container name, and (with dashes mapped to underscores) an event type
// namespace. Checking it here means the author hears about it now rather
// than as a resource-name validation error from a file they did not
// realise was derived from the name they chose.
var scaffoldNameRegexp = regexp.MustCompile(`^[a-z][a-z0-9-]{0,38}[a-z0-9]$`)

// Render produces the package.yaml for opts. It does not touch the disk,
// so a caller can lint the result before writing it — which is what the
// scaffold's own test does.
func Render(opts ScaffoldOptions) ([]byte, error) {
	if !scaffoldNameRegexp.MatchString(opts.Name) {
		return nil, fmt.Errorf("pkgtool: package name %q must be lowercase letters, digits and dashes, "+
			"starting with a letter and ending with a letter or digit (it becomes a directory, "+
			"resource names, and a container name)", opts.Name)
	}
	shape, ok := LookupShape(opts.Shape)
	if !ok {
		return nil, fmt.Errorf("pkgtool: unknown shape %q: must be one of %s",
			opts.Shape, strings.Join(ShapeNames(), ", "))
	}

	capability := opts.Capability
	if capability == "" {
		capability = opts.Name
	}
	if !scaffoldNameRegexp.MatchString(capability) {
		return nil, fmt.Errorf("pkgtool: capability %q must have the same shape as a package name", capability)
	}
	profile := opts.Profile
	if profile == "" {
		profile = opts.Name
	}

	body := map[string]string{
		"service": serviceTemplate,
		"sidecar": sidecarTemplate,
		"config":  configTemplate,
	}[shape.Name]

	// Plain textual substitution rather than text/template, because the
	// thing being written is ITSELF full of {{ ... }} actions meant for
	// the Capability Engine. Rendering a template that emits templates
	// means escaping every one of them, and an escaping mistake here
	// produces a scaffold whose examples are subtly wrong — which is the
	// one failure mode a scaffold must not have.
	return []byte(strings.NewReplacer(
		"__NAME__", opts.Name,
		"__CAPABILITY__", capability,
		"__PROFILE__", profile,
		"__TITLE__", title(opts.Name),
		"__DIGEST__", placeholderDigest,
		// An event type's namespace is [a-z0-9_] and a capability name
		// may carry dashes (ADR-W024 A2), so the two cannot be the same
		// string. Mapping rather than refusing: "home-automation" is a
		// good capability name and "home_automation" is a good namespace.
		"__EVENTNS__", strings.ReplaceAll(capability, "-", "_"),
	).Replace(body)), nil
}

// Scaffold writes Render's output to root/<name>/package.yaml and returns
// the path. It refuses to overwrite: a scaffold is for a package that
// does not exist yet, and the alternative is a command that can silently
// discard an afternoon's work.
func Scaffold(root string, opts ScaffoldOptions) (string, error) {
	content, err := Render(opts)
	if err != nil {
		return "", err
	}
	dir := filepath.Join(root, opts.Name)
	path := filepath.Join(dir, "package.yaml")
	if _, err := os.Stat(path); err == nil {
		return "", fmt.Errorf("pkgtool: %s already exists; scaffolding would overwrite it", path)
	} else if !os.IsNotExist(err) {
		return "", fmt.Errorf("pkgtool: checking %s: %w", path, err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("pkgtool: creating %s: %w", dir, err)
	}
	if err := os.WriteFile(path, content, 0o644); err != nil {
		return "", fmt.Errorf("pkgtool: writing %s: %w", path, err)
	}
	return path, nil
}

// CatalogSiblings are the package directories alongside dir — the other
// packages in the same catalog — sorted.
//
// It exists so `wild pkg new` can print a lint command that actually
// works. A scaffolded service requires identity:oidc, and lint resolves
// requires against the catalog it is GIVEN; told nothing, it reports
// "unsatisfied requirement identity:oidc" for a package that is correct,
// against a deployment that has an identity provider three directories
// away. Telling the author to run `-catalog ...` is the fix, and telling
// them the exact command is the difference between a fix and a footnote.
func CatalogSiblings(dir string) []string {
	parent := filepath.Dir(dir)
	entries, err := os.ReadDir(parent)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		sib := filepath.Join(parent, e.Name())
		if sib == dir {
			continue
		}
		if _, err := os.Stat(filepath.Join(sib, "package.yaml")); err != nil {
			continue
		}
		out = append(out, sib)
	}
	sort.Strings(out)
	return out
}

// CatalogDirsFor narrows siblings to the ones that actually provide a
// trait pkg requires — the shortest `-catalog` list that makes lint
// resolve.
//
// Narrowed rather than "pass them all" because the command is printed for
// a person to run, and a twelve-flag line is one nobody reads before
// pasting. It is also the honest answer to "what does this package
// depend on": exactly these, and if the list comes back empty for a
// package that requires something, the catalog has no provider and that
// is the finding.
func CatalogDirsFor(pkg *pkgspec.Package, siblings []string) []string {
	wanted := map[string]bool{}
	for _, req := range pkg.Requires {
		if trait.IsStructural(req.Trait) {
			continue // the system satisfies it; no package need be named
		}
		wanted[req.Trait+":"+req.Mode] = true
	}
	if len(wanted) == 0 {
		return nil
	}
	var out []string
	for _, dir := range siblings {
		sib, err := pkgspec.Load(filepath.Join(dir, "package.yaml"))
		if err != nil {
			continue // a package that will not load cannot satisfy anything
		}
		for _, prov := range sib.Provides {
			if wanted[prov.Trait+":"+prov.Mode] {
				out = append(out, dir)
				break
			}
		}
	}
	return out
}

// title turns a package name into something a person would write on a
// dashboard: "photos-immich" to "Photos Immich". Rough on purpose — it
// seeds a TODO the author rewrites, and a cleverer version would only be
// wrong in more interesting ways.
func title(name string) string {
	words := strings.Split(name, "-")
	for i, w := range words {
		if w == "" {
			continue
		}
		words[i] = strings.ToUpper(w[:1]) + w[1:]
	}
	return strings.Join(words, " ")
}

// placeholderDigest is a syntactically valid, obviously fake image
// digest.
//
// A digest pin is mandatory (ADR-W003 A4.2) and there is no real digest
// to know at scaffold time, so the choice is between a scaffold that
// lints and a scaffold with an honest blank. All-zeroes is both: it
// satisfies the regexp, so the author's first lint is about their
// decisions rather than about the placeholder, and no registry will ever
// serve it, so it cannot survive to a deployment pretending to be
// pinned — `wild pkg refresh-digests` or a manual pull replaces it, and
// until then a pull fails loudly.
const placeholderDigest = "sha256:0000000000000000000000000000000000000000000000000000000000000000"
