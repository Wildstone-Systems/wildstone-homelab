package pkgtool

import (
	"strings"
	"testing"
)

// imageArgv0 records, per image repository, what a ContainerSpec.Command
// must start with — because Wildstone maps command to Docker's Cmd, and
// Cmd is the WHOLE argv when the image declares no entrypoint. The value
// is a SET of acceptable command[0]s, because one repository can be run
// as more than one Wildstone-supplied binary (distroless below):
//
//	{""}           the image has an ENTRYPOINT that supplies the binary,
//	               so the command carries arguments only
//	{"name", ...}  the image has none, so the command must name one of
//	               these binaries itself
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
var imageArgv0 = map[string][]string{
	"caddy":                  {"caddy"}, // no ENTRYPOINT — command is the whole argv
	"cloudflare/cloudflared": {""},      // ENTRYPOINT ["cloudflared","--no-autoupdate"]
	"binwiederhier/ntfy":     {""},      // ENTRYPOINT ["ntfy"]
	// No ENTRYPOINT: the chat packages, and now agent-hermes's
	// agent-bridge sidecar (ADR-W039 Phase 6/H5, #351), each run their
	// own program the installer puts on the host, mounted at
	// /opt/wildstone/bin under the same distroless base — one image
	// repository, more than one binary depending on which package rendered
	// the container.
	"gcr.io/distroless/static-debian12": {
		"/opt/wildstone/bin/chat-adapter",
		"/opt/wildstone/bin/agent-bridge",
	},

	// nousresearch/hermes-agent (agent-hermes's own "hermes" container,
	// ADR-W039 Phase 6/H5, #351) has a real ENTRYPOINT that supplies the
	// binary, so its command carries the "gateway run" subcommand and
	// flags only — the cloudflared/authelia shape. Evidence: the
	// published Dockerfile at NousResearch/Hermes@main.
	"nousresearch/hermes-agent": {""},

	// The official Synapse image declares ENTRYPOINT ["/start.py"]. Its
	// supported run and migrate_config modes are arguments to that entrypoint.
	// Evidence: element-hq/synapse v1.161.0 docker/Dockerfile and start.py.
	"matrixdotorg/synapse": {""},

	// The official PostgreSQL image declares ENTRYPOINT
	// ["docker-entrypoint.sh"], but that wrapper supplies `postgres` only
	// when its first argument begins with `-`. Wildstone replaces the image
	// Cmd, so the socket-only command must retain `postgres` as argv[0].
	// Evidence: docker-library/postgres's published Dockerfile and
	// docker-entrypoint.sh for PostgreSQL 18/bookworm.
	"postgres": {"postgres"},

	// Caddy with the caddy-dns/cloudflare module compiled in, for the
	// DNS-01 profile (ADR-W019 A9). Same shape as stock caddy, and
	// checked rather than assumed: the image config blob at
	// sha256:62639363…de96 declares Entrypoint null with Cmd
	// ["caddy","run","--config","/etc/caddy/Caddyfile","--adapter","caddyfile"],
	// read from the GHCR registry API on 2026-09-05. Unlike the entries
	// above, this one did not need a node to check — the registry answers
	// the question directly when its network is reachable.
	"ghcr.io/caddybuilds/caddy-cloudflare": {"caddy"},

	// The subtle third case, and the reason this table asks "what must
	// argv[0] be" rather than "does the image have an entrypoint":
	// gotenberg's ENTRYPOINT is ["/usr/bin/tini","--"], a supervisor that
	// execs its arguments. It exists, but supplies no binary, so the
	// command must still name one (its own CMD is ["gotenberg"]).
	"gotenberg/gotenberg": {"gotenberg"},

	// Authelia's ENTRYPOINT is ["/app/entrypoint.sh"], a wrapper that
	// applies UMASK, chowns /config, drops to PUID:PGID and then execs
	// `authelia "$@"` — so the command carries flags only. The wrapper
	// has a branch written for exactly this call shape: it execs its
	// arguments directly UNLESS the first one is --config, in which case
	// it hands them to authelia. Evidence: the image's published
	// Dockerfile and entrypoint.sh at authelia/authelia@master.
	"authelia/authelia": {""},

	// alpine declares no ENTRYPOINT at all, so the command is the whole
	// argv and must name a real binary — the caddy case exactly. It is
	// here as a job runner: the identity capability's group provisioning
	// runs a shell script in it, because the lldap image's own entrypoint
	// unconditionally execs /app/lldap and so cannot run a script under a
	// Cmd-only EnsureContainer. Evidence: alpine's published Dockerfile
	// is FROM scratch with only CMD ["/bin/sh"].
	"alpine": {"/bin/sh"},

	// Prometheus's image declares ENTRYPOINT ["/bin/prometheus"] with
	// CMD ["--config.file=/etc/prometheus/prometheus.yml",
	// "--storage.tsdb.path=/prometheus"], so a command carries flags
	// only — the authelia shape. Evidence: the published Dockerfile at
	// prometheus/prometheus@main, and the image config blob read from
	// Docker Hub on 2026-09-07 while resolving the package's digest.
	"prom/prometheus": {""},

	// Davis's standalone image declares no ENTRYPOINT — its CMD is
	// ["/usr/bin/supervisord", "-c", "/etc/supervisord.conf"], which
	// runs Caddy and php-fpm — so a Cmd-only EnsureContainer that
	// wants the migration console instead must name the binary. It is
	// /bin/sh: the schema migration and the chown that lets php-fpm's
	// non-root workers create the database file are one run, and the
	// image is alpine-based, so sh is there. Evidence: the published
	// docker/Dockerfile-standalone at tchapi/davis@main, whose final
	// stage sets no ENTRYPOINT and no USER.
	"ghcr.io/tchapi/davis-standalone": {"/bin/sh"},

	// Radicale's image declares
	// ENTRYPOINT ["/app/bin/python", "/app/bin/radicale"] with
	// CMD ["--hosts", "0.0.0.0:5232,[::]:5232"], so a command carries
	// flags only — the prometheus shape. Naming the binary again would
	// run `radicale radicale` and exit on an unrecognised argument.
	// Evidence: the published Dockerfile at Kozea/Radicale@master. The
	// image CONFIG blob is not reachable from here — ghcr redirects
	// blobs to pkg-containers.githubusercontent.com, which CI's network
	// does not reach — so the Dockerfile is the check, and a docker
	// inspect on a node is the stronger one when one is available.
	"ghcr.io/kozea/radicale": {""},

	// Z-Wave JS UI declares no ENTRYPOINT (Entrypoint null) with
	// WorkingDir /usr/src/app and CMD ["node", "server/bin/www"], so a
	// command is the whole argv and names node — the caddy shape. The
	// package runs its managed start.mjs, which imports server/bin/www.js
	// itself. Evidence: `docker image inspect` of the pinned digest
	// sha256:717f9d26…2f89 (11.24.2), pulled from Docker Hub on
	// 2026-09-30, and TestZwaveJSUIServesHomeAssistant boots it.
	"zwavejs/zwave-js-ui": {"node"},
	// The OpenThread project's border router image declares
	// ENTRYPOINT ["/app/etc/docker/test/docker_entrypoint.sh"], which
	// parses --radio-url/--backbone-interface/--debug-level itself and
	// drops anything else, so a command carries those flags only.
	// Evidence: `docker image inspect` of the pinned digest on 2026-09-30,
	// and TestOTBRImageContract, which starts it.
	"openthread/otbr": {""},
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
				case hasEntrypoint(want) && got == repoBinary(repo):
					t.Errorf("%s %q: image %q has an ENTRYPOINT, so command[0] must not repeat %q",
						r.kind, r.name, repo, got)
				case !hasEntrypoint(want) && !containsArgv0(want, got):
					t.Errorf("%s %q: image %q has no ENTRYPOINT, so command[0] must be one of %q, got %q "+
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

// hasEntrypoint reports whether an imageArgv0 entry means "this image's
// ENTRYPOINT already supplies the binary", recorded as the single-element
// set {""} rather than a named binary.
func hasEntrypoint(names []string) bool {
	return len(names) == 1 && names[0] == ""
}

// containsArgv0 reports whether got is one of the acceptable command[0]
// values for an image with no ENTRYPOINT.
func containsArgv0(names []string, got string) bool {
	for _, n := range names {
		if n == got {
			return true
		}
	}
	return false
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
