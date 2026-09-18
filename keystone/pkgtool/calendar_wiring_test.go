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
	"encoding/base64"
	"strings"
	"testing"

	"github.com/chickenrat17/wildstone-homelab/keystone/domestic"
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
	// calendarCredentialVolume holds the service account's password.
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
// the Radicale image runs as uid 1000 — so putting the service account's
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
	if !strings.Contains(config, "ldap_secret_file = "+calendarCredentialDir+"/service-password") {
		t.Errorf("the Radicale config does not read its bind password from %s/service-password, which is the only "+
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
		t.Errorf("the radicale container does not mount %s at %s, so the password the Job writes is not visible to it",
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
	spec := calendarContainer(t, "calendar-service-file")

	script := strings.Join(spec.Command, "\n")
	if !strings.Contains(script, calendarCredentialDir+"/service-password") {
		t.Errorf("the credential job does not write %s/service-password, which is the file the config names", calendarCredentialDir)
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
	if _, ok := spec.Env["SERVICE_ACCOUNT_GENERATION"]; !ok {
		t.Error("the credential job carries no SERVICE_ACCOUNT_GENERATION: nothing would re-run it after a rotation, " +
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
			t.Errorf("the backup plan includes %s, which holds the service account's password: a snapshot of the "+
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

// TestCalendarToolsAreDeclaredByThePackage is the check that makes
// "the tools are on" true rather than asserted.
//
// Two halves have to agree and neither can see the other: the package's
// `agentTools` block, which is what the domestic gateway will ALLOW, and
// keystone/domestic.CalendarTools, which is what it will actually
// DISPATCH. A tool in one and not the other fails in a way nothing
// reports at install time:
//
//   - declared but not registered → a Butler lists it to the household,
//     the household asks for it, and the gateway answers "no executor is
//     registered for this tool on this control plane";
//   - registered but not declared → the executor is wired and every call
//     is refused, because a package that does not declare a tool has not
//     offered it;
//   - declared at a different TIER → the gateway refuses on the
//     mismatch, which reads to a household like the calendar being
//     broken.
//
// The tiers are checked too, not only the names, because the tier is
// what decides whether a household surface may carry the tool at all.
func TestCalendarToolsAreDeclaredByThePackage(t *testing.T) {
	sp, ok := walkShipped(t)["calendar-radicale"]
	if !ok {
		t.Fatal("calendar-radicale is not in the catalog")
	}
	if sp.pkg.Metadata.Capability != domestic.CalendarCapability {
		t.Fatalf("the package's capability is %q but the executors register under %q: every call would be "+
			"refused as a tool the capability does not declare",
			sp.pkg.Metadata.Capability, domestic.CalendarCapability)
	}

	declared := map[string]string{}
	for _, tool := range sp.pkg.AgentTools {
		declared[tool.Name] = tool.Risk
	}
	for name, tier := range domestic.CalendarTools {
		got, present := declared[name]
		if !present {
			t.Errorf("keystone registers an executor for calendar.%s, which the package does not declare: "+
				"the gateway refuses a tool its capability has not offered, so the executor is wired and dead", name)
			continue
		}
		if got != tier {
			t.Errorf("calendar.%s is declared at tier %q and registered at %q: the gateway refuses the mismatch, "+
				"which reads to a household as the calendar being broken", name, got, tier)
		}
	}
	for name := range declared {
		if _, present := domestic.CalendarTools[name]; !present {
			t.Errorf("the package declares calendar.%s, which nothing registers: a Butler lists it to the "+
				"household and the gateway answers \"no executor is registered for this tool\"", name)
		}
	}

	// Every tool has to be one a family session can actually carry
	// (ADR-W024 A5/A5.1) — asked of the one definition rather than a
	// copy of the set. A tool declared above that is not a smaller
	// capability, it is a dead entry in the menu.
	for name, tier := range declared {
		if !domestic.TierAllowed(tier, true) {
			t.Errorf("calendar.%s is declared at tier %q, which a family session cannot carry, "+
				"so the tool would never be callable", name, tier)
		}
	}

	// And the split that tier exists to make: a confirm-tier tool must
	// be refused to an unattended rule. Declaring one and having it run
	// at 3am anyway is the whole failure A5.1 was added to prevent.
	for name, tier := range declared {
		if tier == domestic.TierConfirm && domestic.TierAllowed(tier, false) {
			t.Errorf("calendar.%s is at the confirm tier but an unattended rule may carry it", name)
		}
	}
}

// TestTheHouseholdCalendarIsTheGroupsOwnCollection pins the one value in
// this package that is written twice and cannot be computed once.
//
// Radicale surfaces a group's shared collection by looking for
// <group_collections_folder>/<base64 of the group name> and yielding it
// inside the principal of everybody in that group. So the package has to
// spell that base64 — in the rights file that grants access to it, and
// in the job that creates it — and the package template language has no
// base64 to derive it with.
//
// Hardcoded and pinned, therefore, rather than hardcoded and hoped for.
// Rename the LdapGroup without changing the encoding and nothing fails:
// the calendar is created somewhere no member's discovery looks, the
// rights rule guards a path nothing uses, and the household's shared
// calendar simply never appears on anybody's phone.
func TestTheHouseholdCalendarIsTheGroupsOwnCollection(t *testing.T) {
	rendered := calendarPackage(t)

	var group string
	for _, r := range rendered {
		if r.Kind == resource.KindLdapGroup && r.LdapGroup != nil {
			group = r.LdapGroup.Name
		}
	}
	if group == "" {
		t.Fatal("calendar-radicale declares no LdapGroup, so nothing grants the shared calendar")
	}
	encoded := base64.StdEncoding.EncodeToString([]byte(group))

	// The rights file must grant THIS group THIS path.
	var rights string
	for _, r := range rendered {
		if r.Kind == resource.KindConfigFile && r.ConfigFile != nil && r.ConfigFile.Filename == "rights" {
			rights = r.ConfigFile.Content
		}
	}
	if rights == "" {
		t.Fatal("calendar-radicale renders no rights file")
	}
	if !strings.Contains(rights, "groups: "+group) {
		t.Errorf("the rights file does not grant the %q group anything; membership is the whole grant", group)
	}
	if !strings.Contains(rights, "collection: GROUPS/"+encoded) {
		t.Errorf("the rights file does not name GROUPS/%s, which is where Radicale looks for %q's shared "+
			"collection — so the grant guards a path nothing uses", encoded, group)
	}

	// And the job must create it at exactly that path.
	job := calendarContainer(t, "calendar-service-file")
	if got := job.Env["HOUSEHOLD_GROUP_B64"]; got != encoded {
		t.Errorf("the job creates the shared calendar under GROUPS/%s, but %q encodes to %s — "+
			"the calendar would be created where no member's discovery looks", got, group, encoded)
	}
	if !strings.Contains(strings.Join(job.Command, "\n"), "GROUPS/$HOUSEHOLD_GROUP_B64") {
		t.Error("the job does not create the shared calendar under the group folder")
	}

	// The config has to be asking for group memberships at all, or
	// _user_groups is empty and none of the above is ever consulted.
	config := calendarConfigFile(t)
	for _, want := range []string{"type = from_auth", "ldap_groups_attribute"} {
		if !strings.Contains(config, want) {
			t.Errorf("the config does not carry %q, so Radicale never learns anybody's groups and the "+
				"shared calendar is invisible to everyone", want)
		}
	}
}

// A member who signs in for the first time should find a calendar, not
// an empty account. Radicale creates the principal on its own; these are
// what go in it.
func TestAMembersFirstCalendarIsMadeForThem(t *testing.T) {
	config := calendarConfigFile(t)
	if !strings.Contains(config, "predefined_collections") {
		t.Fatal("no predefined_collections: a member installs the profile, signs in, and meets an account " +
			"with no calendars in it")
	}
	if !strings.Contains(config, `"tag": "VCALENDAR"`) {
		t.Error("the predefined collection is not tagged VCALENDAR, so it is not a calendar")
	}
}

// The image must be a RELEASE. This repository publishes `latest` from
// its nightly schedule build, so a package that pinned `latest` pinned
// an unreleased master build of the thing holding the household's
// calendar — which is what this package did until the digest was moved
// to `stable`.
func TestTheCalendarImageIsTheReleaseNotTheNightly(t *testing.T) {
	spec := calendarContainer(t, "radicale")
	const releaseDigest = "sha256:0d60d544710bda0fa83bfdea685db731b6cbf74e12d7f22b4997e9cff3a50468"
	if !strings.HasSuffix(spec.Image, releaseDigest) {
		t.Errorf("the radicale image is %q; the pin should be the `stable` tag's digest (%s, = 3.8.0). "+
			"`latest` on this repository is the nightly build.", spec.Image, releaseDigest)
	}
}
