package pkgtool

import (
	"strings"
	"testing"
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

	// Caddy with the caddy-dns/cloudflare module compiled in, for the
	// DNS-01 profile (ADR-W019 A9). Same shape as stock caddy, and
	// checked rather than assumed: the image config blob at
	// sha256:62639363…de96 declares Entrypoint null with Cmd
	// ["caddy","run","--config","/etc/caddy/Caddyfile","--adapter","caddyfile"],
	// read from the GHCR registry API on 2026-09-05. Unlike the entries
	// above, this one did not need a node to check — the registry answers
	// the question directly when its network is reachable.
	"ghcr.io/caddybuilds/caddy-cloudflare": "caddy",

	// The subtle third case, and the reason this table asks "what must
	// argv[0] be" rather than "does the image have an entrypoint":
	// gotenberg's ENTRYPOINT is ["/usr/bin/tini","--"], a supervisor that
	// execs its arguments. It exists, but supplies no binary, so the
	// command must still name one (its own CMD is ["gotenberg"]).
	"gotenberg/gotenberg": "gotenberg",

	// Authelia's ENTRYPOINT is ["/app/entrypoint.sh"], a wrapper that
	// applies UMASK, chowns /config, drops to PUID:PGID and then execs
	// `authelia "$@"` — so the command carries flags only. The wrapper
	// has a branch written for exactly this call shape: it execs its
	// arguments directly UNLESS the first one is --config, in which case
	// it hands them to authelia. Evidence: the image's published
	// Dockerfile and entrypoint.sh at authelia/authelia@master.
	"authelia/authelia": "",

	// alpine declares no ENTRYPOINT at all, so the command is the whole
	// argv and must name a real binary — the caddy case exactly. It is
	// here as a job runner: the identity capability's group provisioning
	// runs a shell script in it, because the lldap image's own entrypoint
	// unconditionally execs /app/lldap and so cannot run a script under a
	// Cmd-only EnsureContainer. Evidence: alpine's published Dockerfile
	// is FROM scratch with only CMD ["/bin/sh"].
	"alpine": "/bin/sh",
}

// TestPackageCommandsNameTheirBinary walks every shipped package's
// rendered containers. It cannot catch a wrong flag, but it catches the
// whole class where argv[0] is a subcommand of a binary nothing will run.
//
// It is NOT a substitute for starting the container: no test in this repo
// does that, which is why F23 shipped past a green CI and was found only
// by a human deploying the spec on a VM.
func TestPackageCommandsNameTheirBinary(t *testing.T) {
	checked := 0
	for name, rendered := range renderShippedPackages(t) {
		t.Run(name, func(t *testing.T) {
			for _, r := range runnables(rendered) {
				if len(r.spec.Command) == 0 {
					continue
				}
				checked++
				repo := imageRepo(r.spec.Image)
				want, known := imageArgv0[repo]
				if !known {
					t.Errorf("%s %q runs image %q with a command, but %q is not in imageArgv0: "+
						"check whether the image declares an ENTRYPOINT and add it, so command[0] is a decision "+
						"rather than a copy of the nearest neighbour", r.kind, r.name, repo, repo)
					continue
				}
				got := r.spec.Command[0]
				switch {
				case want == "" && got == repoBinary(repo):
					t.Errorf("%s %q: image %q has an ENTRYPOINT, so command[0] must not repeat %q",
						r.kind, r.name, repo, got)
				case want != "" && got != want:
					t.Errorf("%s %q: image %q has no ENTRYPOINT, so command[0] must be %q, got %q "+
						"— the container will die with `exec: %q: executable file not found in $PATH`",
						r.kind, r.name, repo, want, got, got)
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
