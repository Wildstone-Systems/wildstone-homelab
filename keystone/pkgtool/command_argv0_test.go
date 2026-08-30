package pkgtool

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/chickenrat17/wildstone-homelab/keystone/capability"
	"github.com/chickenrat17/wildstone-homelab/keystone/pkgspec"
	"github.com/chickenrat17/wildstone-homelab/keystone/resource"
)

// imageArgv0 records, per image repository, what a ContainerSpec.Command
// must start with — because Wildstone maps command to Docker's Cmd, and
// Cmd is the WHOLE argv when the image declares no entrypoint.
//
//	""      the image has an ENTRYPOINT that supplies the binary, so the
//	        command carries arguments only
//	"name"  the image has none, so the command must name the binary itself
//
// This mattered: both ingress profiles shipped ["run", "--config", ...]
// for caddy, an image with no entrypoint, and every Caddy container died
// with `exec: "run": executable file not found in $PATH` (F23) — the one
// blocker in the v0.1.1-alpha install validation. cloudflared, in the same
// file, DOES have an entrypoint, so ["tunnel","run"] is correct there;
// that neighbouring correct line is what made the wrong one look right.
//
// An image missing from this table fails the test rather than passing
// silently, so adding a command to a new image forces the author to check
// its entrypoint instead of copying whichever neighbour is nearest.
// Evidence for each entry is recorded so a future author can re-check it
// rather than trust the table: caddy's absent entrypoint was proven by the
// runtime failure in the v0.1.1-alpha validation (`exec: "run": ...`) and
// by the fixed spec then starting and serving; cloudflared's and ntfy's
// entrypoints come from their published Dockerfiles. Image config blobs
// are not reachable from CI's network, so a docker inspect on a node is
// the stronger check when one is available.
var imageArgv0 = map[string]string{
	"caddy":                  "caddy", // no ENTRYPOINT — command is the whole argv
	"cloudflare/cloudflared": "",      // ENTRYPOINT ["cloudflared","--no-autoupdate"]
	"binwiederhier/ntfy":     "",      // ENTRYPOINT ["ntfy"]

	// The subtle third case, and the reason this table asks "what must
	// argv[0] be" rather than "does the image have an entrypoint":
	// gotenberg's ENTRYPOINT is ["/usr/bin/tini","--"], a supervisor that
	// execs its arguments. It exists, but supplies no binary, so the
	// command must still name one (its own CMD is ["gotenberg"]).
	"gotenberg/gotenberg": "gotenberg",
}

// TestPackageCommandsNameTheirBinary walks every shipped package's
// rendered containers. It cannot catch a wrong flag, but it catches the
// whole class where argv[0] is a subcommand of a binary nothing will run.
//
// It is NOT a substitute for starting the container: no test in this repo
// does that, which is why F23 shipped past a green CI and was found only
// by a human deploying the spec on a VM.
func TestPackageCommandsNameTheirBinary(t *testing.T) {
	root, err := filepath.Abs("../../packages")
	if err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}

	fillers := map[string]string{
		"public_domain":     "example.internal",
		"admin_domain":      "admin.example.internal",
		"node":              "n1",
		"admin_listen_addr": "100.64.0.1",
		"listen_addr":       "100.64.0.1",
	}

	var allPkgs []*pkgspec.Package
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		p, err := pkgspec.Load(filepath.Join(root, e.Name(), "package.yaml"))
		if err != nil {
			t.Fatalf("load %s: %v", e.Name(), err)
		}
		allPkgs = append(allPkgs, p)
	}

	checked := 0
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		dir := filepath.Join(root, e.Name())
		t.Run(e.Name(), func(t *testing.T) {
			pkg, err := pkgspec.Load(filepath.Join(dir, "package.yaml"))
			if err != nil {
				t.Fatalf("load: %v", err)
			}
			params := map[string]string{}
			for _, d := range pkg.Params {
				if v, ok := fillers[d.Name]; ok {
					params[d.Name] = v
				} else if d.Default != "" {
					params[d.Name] = d.Default
				} else {
					params[d.Name] = "lint-" + sanitizeParam(d.Name)
				}
			}
			// Every shipped package is on the catalog, so a package with
			// requires expands instead of skipping. A skip here would be
			// the same hole as a guard that silently stops running: green,
			// and checking nothing.
			catalog := capability.NewCatalog(allPkgs...)
			rendered, err := capability.NewEngine().Expand(pkg, params, catalog)
			if err != nil {
				t.Fatalf("expand: %v", err)
			}
			for _, r := range rendered {
				if r.Kind != resource.KindContainer || r.Container == nil || len(r.Container.Command) == 0 {
					continue
				}
				checked++
				repo := imageRepo(r.Container.Image)
				want, known := imageArgv0[repo]
				if !known {
					t.Errorf("container %q runs image %q with a command, but %q is not in imageArgv0: "+
						"check whether the image declares an ENTRYPOINT and add it, so command[0] is a decision "+
						"rather than a copy of the nearest neighbour", r.Metadata.Name, repo, repo)
					continue
				}
				got := r.Container.Command[0]
				switch {
				case want == "" && got == repoBinary(repo):
					t.Errorf("container %q: image %q has an ENTRYPOINT, so command[0] must not repeat %q",
						r.Metadata.Name, repo, got)
				case want != "" && got != want:
					t.Errorf("container %q: image %q has no ENTRYPOINT, so command[0] must be %q, got %q "+
						"— the container will die with `exec: %q: executable file not found in $PATH`",
						r.Metadata.Name, repo, want, got, got)
				}
			}
		})
	}
	if checked == 0 {
		t.Fatal("no shipped container carried a command — the walk is broken, not the packages")
	}
}

// imageRepo strips the digest pin: "caddy@sha256:..." -> "caddy".
func imageRepo(image string) string {
	repo, _, _ := strings.Cut(image, "@")
	return repo
}

// repoBinary is the binary an image's repository name implies, used only
// to catch a command that repeats a name the entrypoint already supplies.
func repoBinary(repo string) string {
	if i := strings.LastIndex(repo, "/"); i >= 0 {
		return repo[i+1:]
	}
	return repo
}
