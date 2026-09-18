package pkgtool

// The calendar capability's own conformance checks.
//
// Everything here is a claim the catalog-wide tests cannot make. They
// check a package against ITSELF — secrets referenced are declared,
// mounts name declared volumes, a Route's upstream is a container the
// package runs — and calendar-radicale passes all of them while being
// completely unable to start, in four different ways. Each is a
// relationship between two declarations that only this package knows
// about, and each one fails as a 502 behind a converged install rather
// than as anything a reconcile reports.

import (
	"strings"
	"testing"

	"github.com/chickenrat17/wildstone-homelab/keystone/resource"
)

// The paths the package's declarations have to agree about. Spelled out
// here rather than derived, so that a change to either side is a
// deliberate edit to this file.
const (
	// calendarConfigPath is where the reconciler puts a ConfigFile owned
	// by capability/calendar: <managedPrefix>/<capability>/<filename>
	// (keystone/reconcile.configFilePath). The container's --config flag
	// has to name exactly this, and nothing else checks that it does.
	calendarConfigPath = "/etc/wildstone/calendar/config"
	// calendarCredentialDir is the volume the credential Job writes into
	// and Radicale reads out of.
	calendarCredentialDir = "/run/calendar"
	// calendarCredentialVolume holds the directory reader's password.
	calendarCredentialVolume = "radicale-credentials"
	// calendarCollectionsVolume holds the household's calendars.
	calendarCollectionsVolume = "radicale-collections"
)

func calendarPackage(t *testing.T) []resource.Resource {
	t.Helper()
	rendered, ok := renderShippedPackages(t)["calendar-radicale"]
	if !ok {
		t.Fatal("calendar-radicale is not in the catalog")
	}
	return rendered
}

// TestCalendarConfigCarriesNoSecretReference is the sharpest of the four
// and the least obvious.
//
// A managed file whose content carries a secret reference is narrowed to
// 0600 root-owned when the node materializes it (wardstone ops.go), and
// the Radicale image runs as uid 1000 — so putting the directory reader's
// password in this ConfigFile would produce a config file the process
// cannot open. Authelia is the only package that gets away with a
// secret-bearing config, and only because it pins PUID=0; this image has
// no such knob.
//
// The failure is silent in every surface that would normally report one:
// the file is written, the plan converges, the capability reads as
// installed, and Radicale exits on a permission error behind a 502.
func TestCalendarConfigCarriesNoSecretReference(t *testing.T) {
	config := calendarConfigFile(t)
	if strings.Contains(config, "secret/") {
		t.Error("the Radicale config carries a secret reference. The node narrows such a file to 0600 root-owned " +
			"and Radicale runs as uid 1000, so it could not read its own configuration: put the credential in the " +
			"Job's environment and let it write the file instead")
	}
	// The other half of the same claim: the config must point at the
	// file the Job writes, or Radicale refuses to start at all —
	// radicale/auth/ldap.py raises on a reader DN with no secret.
	if !strings.Contains(config, "ldap_secret_file = "+calendarCredentialDir+"/ldap-reader") {
		t.Errorf("the Radicale config does not read its reader password from %s/ldap-reader, which is the only "+
			"place anything writes one; with a reader DN and no secret Radicale raises at startup",
			calendarCredentialDir)
	}
}

// TestCalendarContainerReadsTheConfigWildstoneWrites pins the flag
// against the path the reconciler derives.
//
// Radicale reads no environment variables at all, so this one flag is
// the whole of its configuration. Named wrongly, it starts on its
// built-in defaults instead — auth type denyall, storage somewhere else
// — which is a running container answering 401 to the household with
// nothing anywhere to say the config was never read.
func TestCalendarContainerReadsTheConfigWildstoneWrites(t *testing.T) {
	spec := calendarContainer(t, "radicale")

	var named bool
	for i, arg := range spec.Command {
		if arg == "--config" && i+1 < len(spec.Command) && spec.Command[i+1] == calendarConfigPath {
			named = true
		}
	}
	if !named {
		t.Errorf("the radicale container's command does not pass --config %s (got %v): Radicale reads no "+
			"environment variables, so it would start on its own defaults and deny every login",
			calendarConfigPath, spec.Command)
	}

	// Naming it is not enough: the directory has to be mounted, and a
	// package's mount list is the only thing that puts it there.
	if !mountsPath(spec, "/etc/wildstone/calendar") {
		t.Error("the radicale container does not mount /etc/wildstone/calendar, so the config it is pointed at is not there")
	}
	if !mountsVolume(spec, calendarCredentialVolume, calendarCredentialDir) {
		t.Errorf("the radicale container does not mount %s at %s, so the reader password the Job writes is not visible to it",
			calendarCredentialVolume, calendarCredentialDir)
	}
	if !mountsVolume(spec, calendarCollectionsVolume, "/var/lib/radicale") {
		t.Errorf("the radicale container does not mount %s at /var/lib/radicale, so the household's calendars "+
			"would live in the container's writable layer and vanish with it", calendarCollectionsVolume)
	}
}

// TestCalendarCredentialJobWritesWhereRadicaleReads checks the other end
// of the same handshake, including the ownership.
//
// Writing the file is not the job; writing it somewhere a uid-1000
// process can open it is. A Job runs as root, so the default is a file
// Radicale cannot read — which is the exact failure this whole
// arrangement exists to avoid.
func TestCalendarCredentialJobWritesWhereRadicaleReads(t *testing.T) {
	spec := calendarContainer(t, "calendar-ldap-credential")

	script := strings.Join(spec.Command, "\n")
	if !strings.Contains(script, calendarCredentialDir+"/ldap-reader") {
		t.Errorf("the credential job does not write %s/ldap-reader, which is the file the config names", calendarCredentialDir)
	}
	if !strings.Contains(script, "chown 1000:1000") {
		t.Error("the credential job does not chown the file it writes: a Job runs as root and Radicale runs as " +
			"uid 1000, so the file lands unreadable by the only process that needs it")
	}
	if !mountsVolume(spec, calendarCredentialVolume, calendarCredentialDir) {
		t.Errorf("the credential job does not mount %s at %s, so it writes into its own container and exits 0",
			calendarCredentialVolume, calendarCredentialDir)
	}
	// The generation param is in the environment for one reason: a Job
	// re-runs on a spec-hash change, and the env carries the secret
	// REFERENCE rather than its value — so without a knob that moves the
	// hash, rotating the directory password would leave Radicale reading
	// a file written from the old one, forever.
	if _, ok := spec.Env["LDAP_READER_GENERATION"]; !ok {
		t.Error("the credential job carries no LDAP_READER_GENERATION: nothing would re-run it after a rotation, " +
			"because a rotated secret does not change the job's spec hash")
	}
}

// TestCalendarBackupTakesTheCalendarsAndNotTheCredential.
//
// The collections volume is the only copy of the household's calendars,
// so it has to be in the plan. The credential volume holds the password
// that reads the directory's people, so it must not be: a restic
// snapshot of the family's appointments should not also be a way in to
// the directory.
func TestCalendarBackupTakesTheCalendarsAndNotTheCredential(t *testing.T) {
	var plan *resource.BackupPlanSpec
	for _, r := range calendarPackage(t) {
		if r.Kind == resource.KindBackupPlan && r.BackupPlan != nil {
			plan = r.BackupPlan
		}
	}
	if plan == nil {
		t.Fatal("calendar-radicale declares no BackupPlan: these are the only copies of the household's calendars")
	}

	var tookCalendars bool
	for _, v := range plan.Volumes {
		switch v {
		case calendarCollectionsVolume:
			tookCalendars = true
		case calendarCredentialVolume:
			t.Errorf("the backup plan includes %s, which holds the directory reader's password: a snapshot of the "+
				"calendars would carry a live credential for the household's directory", calendarCredentialVolume)
		}
	}
	if !tookCalendars {
		t.Errorf("the backup plan does not include %s, which is the only copy of the household's calendars",
			calendarCollectionsVolume)
	}
	if plan.Schedule != "daily" {
		t.Errorf("the calendar backup runs %s; a phone that resyncs after a loss gets whatever this volume says, "+
			"so the cadence is daily like mail-roundcube's, not the weekly a replaceable library gets", plan.Schedule)
	}
}

// --- small readers, so each test above reads as its claim ---

func calendarConfigFile(t *testing.T) string {
	t.Helper()
	for _, r := range calendarPackage(t) {
		if r.Kind == resource.KindConfigFile && r.ConfigFile != nil && r.ConfigFile.Filename == "config" {
			return r.ConfigFile.Content
		}
	}
	t.Fatal("calendar-radicale renders no ConfigFile named `config`, which is the whole of Radicale's configuration")
	return ""
}

func calendarContainer(t *testing.T, name string) *resource.ContainerSpec {
	t.Helper()
	for _, r := range runnables(calendarPackage(t)) {
		if r.name == name {
			return r.spec
		}
	}
	t.Fatalf("calendar-radicale renders no container or job named %q", name)
	return nil
}

func mountsVolume(spec *resource.ContainerSpec, source, target string) bool {
	for _, m := range spec.Mounts {
		if m.Source == source && m.Target == target {
			return true
		}
	}
	return false
}

func mountsPath(spec *resource.ContainerSpec, target string) bool {
	for _, m := range spec.Mounts {
		if m.Target == target {
			return true
		}
	}
	return false
}
