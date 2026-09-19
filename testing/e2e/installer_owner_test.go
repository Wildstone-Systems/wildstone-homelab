package e2e

// The installer's first-Owner contract (design §2.4, #206).
//
// scripts/install.sh cannot be unit-tested: it installs Docker, pulls
// release assets and talks to a running control plane. CI checks it with
// `bash -n` and shellcheck, which prove it parses and has no obvious
// shell defects — neither of which would notice the property that
// matters here.
//
// That property is a REQUIREMENT, not an intention: installation must
// not complete without an active Owner. The regression it guards against
// is not a typo, it is somebody deleting a step that looks redundant —
// and the state it would leave behind is the one slice 3 exists to end,
// where the Console admits whoever the directory says because Wildstone
// has never been told who administers the house.
//
// So this reads the script and asserts the shape. A structural test is
// weaker than running the thing, and it is what the repository already
// does where two files have to agree (see the "release and installer
// agree on the console asset" step in ci.yml). It is the strongest check
// available without a VM.

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

func installerSource(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile("../../scripts/install.sh")
	if err != nil {
		t.Fatalf("reading the installer: %v", err)
	}
	return string(raw)
}

// The central one: setup fails if no Owner exists at the end.
//
// #206 words it as "Installation FAILS if no active Owner exists at the
// end. Not a warning." An installer that logged and carried on would
// report success on exactly the households that are broken.
func TestInstallWithoutAnOwnerFails(t *testing.T) {
	src := installerSource(t)
	if !strings.Contains(src, "create_first_owner") {
		t.Fatal("the installer no longer creates a first owner (design §2.4)")
	}

	// The verification, and that it is fatal rather than advisory. `fail`
	// is the installer's own abort; `log` would be the warning §2.4 says
	// this must not be.
	idx := strings.Index(src, "create_first_owner() {")
	if idx < 0 {
		t.Fatal("create_first_owner is called but not defined")
	}
	body := src[idx:]
	if end := strings.Index(body, "\n}\n"); end > 0 {
		body = body[:end]
	}
	if !strings.Contains(body, `"role":"owner"`) {
		t.Error("create_first_owner does not check the recorded role is owner")
	}
	if !strings.Contains(body, "refusing to report success") {
		t.Error("create_first_owner does not refuse to report success without an owner")
	}
	// The check has to abort. Asserted on the verification block rather
	// than anywhere in the function, because a `fail` elsewhere would
	// satisfy a looser test while the owner check warned and continued.
	//
	// The index is checked before it is used. The first version sliced
	// on LastIndex without testing for -1, so removing the verification
	// block — the exact regression this guards — panicked instead of
	// failing, and a panic in a table of checks is a worse signal than
	// the failure it replaced.
	at := strings.LastIndex(body, "api GET /api/v1/butler/members")
	if at < 0 {
		t.Fatal("create_first_owner no longer reads the member list back to verify")
	}
	verify := body[at:]
	if !strings.Contains(verify, "fail ") {
		t.Errorf("the owner check does not abort the install:\n%s", verify)
	}
	// Admitted as well as owner: a recorded Owner who was never let in
	// is not an active Owner, and a check that read only the role would
	// call that household done.
	if !strings.Contains(verify, `"admitted":true`) {
		t.Error("the check does not require the owner to be admitted")
	}
}

// Both halves exist and are linked: a directory identity to sign in
// with, and a Person the household is authoritative for (decision 1).
//
// One without the other is a specific, familiar failure. A directory
// account with no Person is what every install produced before this
// slice, and it is why a fresh deployment had an empty butler_members.
// A Person with no directory account is somebody Wildstone will admit
// and who has nothing to log in with.
func TestInstallCreatesFirstOwnerAndDirectoryIdentity(t *testing.T) {
	src := installerSource(t)
	for _, want := range []struct{ call, why string }{
		{"/api/v1/household/members", "the directory account"},
		{"/api/v1/butler/members", "the Person record"},
		{"/admit", "admission — a recorded Person who was never admitted is not an active Owner"},
		{`console_users`, "the group that offers the Console"},
	} {
		if !strings.Contains(src, want.call) {
			t.Errorf("the installer no longer creates %s (%s)", want.why, want.call)
		}
	}
}

// The first Owner is an ordinary Owner (§2.4): no permanent flag, no
// superuser, nothing the household cannot later undo.
//
// Asserted as an absence, which is the only way to assert it: the
// protection they get is the Last Owner invariant, and that is a
// property of the schema rather than anything the installer writes. A
// flag here would be the thing §2.4 says not to build, so the test is
// that no such flag is introduced.
//
// Comments are stripped first. The first version of this test read the
// whole file and failed on the word "permanent" in the very comment
// explaining that there is no permanent privilege — an absence test that
// matches prose is one that fires on the documentation rather than the
// code.
func TestInstallerGivesTheFirstOwnerNoPermanentPrivilege(t *testing.T) {
	var code strings.Builder
	for _, line := range strings.Split(installerSource(t), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		code.WriteString(strings.ToLower(line))
		code.WriteString("\n")
	}
	for _, forbidden := range []string{"superuser", "is_first_owner", "permanent_owner", "undeletable"} {
		if strings.Contains(code.String(), forbidden) {
			t.Errorf("the installer sets %q on the first owner; §2.4 says they are an ordinary owner", forbidden)
		}
	}
}

// The password the operator types is set on the account they named.
//
// This is the defect the slice found, stated as a test. The installer
// used to prompt for a password and set it on LLDAP's built-in `admin`,
// so an operator who typed "mika" was told they had an account called
// mika, had none, and signed in as admin. The two facts that make that
// impossible now are asserted together, because either alone is the bug:
// a password is collected, AND it is set on $ADMIN_USER.
//
// ADR-W033 supersedes ADR-W020 A4 for the first Owner; see the note on
// A4. Everyone after them still gets a claim code.
func TestInstallerSetsTheOperatorsPasswordOnTheirOwnAccount(t *testing.T) {
	src := installerSource(t)
	if !strings.Contains(src, "ADMIN_PASS") {
		t.Fatal("the installer no longer collects a password (design §2.4)")
	}
	if !strings.Contains(src, "set_owner_password") {
		t.Fatal("the installer collects a password and never sets it on an account")
	}
	idx := strings.Index(src, "set_owner_password() {")
	if idx < 0 {
		t.Fatal("set_owner_password is called but not defined")
	}
	body := src[idx:]
	if end := strings.Index(body, "\n}\n"); end > 0 {
		body = body[:end]
	}
	// On the operator's OWN account, not the directory's service one.
	// The whole defect was that these were the same call.
	if !strings.Contains(body, `--username "$ADMIN_USER"`) {
		t.Error("the password is not set on the account the operator named")
	}
	if !strings.Contains(body, `LLDAP_USER_PASSWORD="$ADMIN_PASS"`) {
		t.Error("the password set is not the one the operator typed")
	}
	// And the result is proved rather than assumed, because the failure
	// this step exists to stop is silent.
	if !strings.Contains(body, "cannot sign in with the password just set") {
		t.Error("set_owner_password does not verify the password took")
	}
}

// The refusal of `admin` covers every spelling of it, not a list of
// three.
//
// The first version matched `admin|Admin|ADMIN`, which is 3 of the 32
// ways to type the name, so `ADmin` walked through a guard whose entire
// purpose is that this name does not get through — and the structural
// tests above said nothing, because they asked whether a refusal
// existed and not whether it was complete.
//
// Asserted as "folds case before matching" rather than by listing
// spellings, because a test that enumerates permutations is the same
// mistake as the code it is checking: both are complete only until
// somebody thinks of a case neither listed.
func TestInstallerRefusesEverySpellingOfAdmin(t *testing.T) {
	src := installerSource(t)
	idx := strings.Index(src, "step_admin() {")
	if idx < 0 {
		t.Fatal("step_admin is gone")
	}
	body := src[idx:]
	if end := strings.Index(body, "\n}\n"); end > 0 {
		body = body[:end]
	}
	// Comments stripped, for the reason the sibling test below records:
	// the first run of THIS test failed on the comment explaining why
	// the spelling list was wrong, which names the list. A test that
	// reads prose reports on the documentation, not the code — and it
	// caught me one function after I had written that warning down.
	var code strings.Builder
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		code.WriteString(line)
		code.WriteString("\n")
	}
	body = code.String()
	if !strings.Contains(body, "admin") {
		t.Fatal("step_admin no longer refuses the directory's service account")
	}
	// Either fold the subject before matching, or turn matching
	// case-insensitive. Both are correct; listing spellings is not.
	folds := strings.Contains(body, "tr '[:upper:]' '[:lower:]'") ||
		strings.Contains(body, "tr 'A-Z' 'a-z'") ||
		strings.Contains(body, "nocasematch") ||
		strings.Contains(body, "${u,,}")
	if !folds {
		t.Error("the `admin` refusal matches spellings rather than folding case: " +
			"`ADmin` and 28 others get through")
	}
	// And the enumeration does not come back alongside the fold.
	if strings.Contains(body, "admin|Admin|ADMIN") {
		t.Error("the spelling list is back; it was never complete")
	}
}

// The directory's own admin stays a service account: generated, never
// typed, never the thing a person signs in as.
//
// Separate from the test above because they are different claims, and
// collapsing them is what produced the original defect — one password
// doing two jobs.
func TestTheDirectoryServiceAccountPasswordIsGenerated(t *testing.T) {
	src := installerSource(t)
	if !strings.Contains(src, "/dev/urandom") {
		t.Error("the directory's service password is no longer generated")
	}
	idx := strings.Index(src, "set_admin_password() {")
	if idx < 0 {
		t.Fatal("set_admin_password is gone")
	}
	body := src[idx:]
	if end := strings.Index(body, "\n}\n"); end > 0 {
		body = body[:end]
	}
	// The operator's password must not appear in this function at all.
	// Written as a scan for $ADMIN_PASS that is not $LLDAP_ADMIN_PASS,
	// because the two differ by a prefix and a naive Contains matches
	// both — the first version of this test asked for
	// `ADMIN_PASS"` AND NOT `LLDAP_ADMIN_PASS` anywhere in the same
	// body, which no text can satisfy, so it could never fail.
	for _, line := range strings.Split(body, "\n") {
		stripped := strings.ReplaceAll(line, "LLDAP_ADMIN_PASS", "")
		if strings.Contains(stripped, "ADMIN_PASS") {
			t.Errorf("the directory's service password is the operator's again: %s", strings.TrimSpace(line))
		}
	}
}

// The Console is put behind the identity proxy, and the four decisions
// that make that true agree with each other.
//
// Each one alone is inert, and that is why they are asserted together.
// The route needs a listener the proxy can dial; the group needs the
// package that declares it; the first Owner needs the group; and every
// one of them needs an ingress to exist at all. A change that keeps
// three and drops the fourth produces an install that completes and a
// Console that cannot be reached, with nothing on the way saying so.
//
// This is a structural test over a script that cannot be run here, so
// it proves the pieces are present and consistent — not that they work.
// The PR says the same in plainer words. #256 is the precedent and also
// the warning: its shell changes drew ten review findings, three of
// which would have broken every install, and none were reachable
// without a host.
func TestInstallPutsTheConsoleBehindTheProxy(t *testing.T) {
	src := installerSource(t)

	// 1. The listener moves where a container can dial it. Without this
	// the rendered route points at the proxy's own loopback.
	if !strings.Contains(src, "-api-listen 0.0.0.0:9441") {
		t.Error("the API still listens only on loopback, so the proxy cannot reach it " +
			"and the Console route can never connect")
	}
	// 2. The package that declares BOTH the group and the route.
	if !strings.Contains(src, "enable_capability wildstone-console") {
		t.Error("wildstone-console is not installed, so console_users and the route " +
			"that puts Authelia in front of the Console do not exist")
	}
	// 3. The first Owner is put in the group, or they sign in through
	// the proxy and Keystone refuses them.
	if !strings.Contains(src, `OWNER_GROUPS='"console_users",`) ||
		!strings.Contains(src, `"groups\":[$OWNER_GROUPS]`) {
		t.Error("the first Owner is not put in console_users, so the proxy signs them " +
			"in and the Console refuses them")
	}

	// 4. All of it is conditional on there being an ingress, and on the
	// SAME condition. A host with no tailnet has no proxy to sit behind,
	// and asking for an undeclared group there aborts the install — the
	// defect #256 found and fixed by removing the request outright.
	if strings.Count(src, "CONSOLE_BEHIND_PROXY") < 4 {
		t.Error("the console-behind-proxy decision is not consistently applied; " +
			"the route, the group and the summary must agree or the install " +
			"completes with a Console nobody can open")
	}
	idx := strings.Index(src, "create_first_owner() {")
	if idx < 0 {
		t.Fatal("create_first_owner is gone")
	}
	body := src[idx:]
	if end := strings.Index(body, "\n}\n"); end > 0 {
		body = body[:end]
	}
	if !strings.Contains(body, "CONSOLE_BEHIND_PROXY") {
		t.Error("the first Owner's group request is unconditional; on a host with no " +
			"ingress the group is undeclared and the proposal is refused, which " +
			"aborts the whole install")
	}
}

// An operator who named -api-listen keeps it.
//
// Binding 0.0.0.0 is something the installer does FOR a household that
// has an ingress, never something it does TO one that made its own
// choice. docs/audit/runtime-access.md documents a second Keystone on
// `-api-listen 127.0.0.1:19441`, and Go's flag package takes the last
// occurrence and reports nothing — so an unguarded append would move
// that instance to a different interface without a word anywhere.
//
// The installer already treats --console-dir this way and says so at
// the call site; this is the same rule, and the test exists because the
// guard is one line and reads like a nicety rather than the thing that
// keeps an explicit choice explicit.
func TestInstallDoesNotOverrideAnOperatorsApiListen(t *testing.T) {
	src := installerSource(t)
	idx := strings.Index(src, "-api-listen 0.0.0.0:9441")
	if idx < 0 {
		t.Fatal("the installer no longer binds the API where the proxy can reach it")
	}
	// The guard sits on the branch that sets it, not merely somewhere in
	// the file: a check elsewhere would satisfy a looser test while the
	// append still ran unconditionally.
	start := strings.LastIndex(src[:idx], "\n  if ")
	if start < 0 {
		t.Fatal("could not find the condition guarding the api-listen append")
	}
	guard := src[start:idx]
	if !strings.Contains(guard, "keystone_arg_named -api-listen") {
		t.Errorf("the api-listen append is not guarded by an operator's own choice:\n%s", guard)
	}
}

// The package is installed before the Owner who is put in its group.
//
// Ordering, asserted as ordering rather than presence, because both
// steps exist either way and the wrong sequence fails at a distance:
// addHouseholdMember refuses a group nothing has declared yet, so the
// install dies on the Owner step with an error naming a group, several
// hundred lines from the install step that was supposed to create it.
func TestTheConsolePackageIsInstalledBeforeTheFirstOwner(t *testing.T) {
	src := installerSource(t)
	pkg := strings.Index(src, "enable_capability wildstone-console")
	owner := strings.LastIndex(src, "create_first_owner\n")
	if pkg < 0 || owner < 0 {
		t.Fatal("the console package install or the first-owner call is gone")
	}
	if pkg > owner {
		t.Error("wildstone-console is installed AFTER create_first_owner; the group " +
			"it declares does not exist yet, so the Owner's proposal is refused")
	}
}

// The install ends on the household's own homepage, not on gethomepage.
//
// Both are profiles of the one exclusive `dashboard` capability, so the
// installer picks exactly one. It picks homepage-wildstone: the page
// the family actually uses — what needs a decision, what the house did,
// their files, their assistant — is served by Keystone, and gethomepage
// only presents. The address it prints is that page, under /home/,
// because Keystone serves the Console at /.
func TestTheInstallLandsOnTheHouseholdHomepage(t *testing.T) {
	src := installerSource(t)
	if !strings.Contains(src, "enable_capability homepage-wildstone") {
		t.Fatal("the household homepage is not installed")
	}
	if strings.Contains(src, "enable_capability dashboard ") {
		t.Error("gethomepage is still installed by default; the two profiles are " +
			"exclusive, so the household homepage would be refused")
	}
	if !strings.Contains(src, `DASHBOARD_URL="https://$tsname/home/"`) {
		t.Error("the install does not send the operator to /home/; the bare host " +
			"is the Console, not the family's page")
	}
	// Its group is requested for the first Owner, or the page the install
	// just printed refuses the person who installed it.
	if !regexp.MustCompile(`OWNER_GROUPS='[^']*"homepage_users"`).MatchString(src) {
		t.Error("the first Owner is not put in homepage_users")
	}
	// Installed before the Owner who is put in its group, for the reason
	// TestTheConsolePackageIsInstalledBeforeTheFirstOwner gives.
	pkg := strings.Index(src, "enable_capability homepage-wildstone")
	owner := strings.LastIndex(src, "create_first_owner\n")
	if owner < 0 || pkg > owner {
		t.Error("homepage-wildstone is installed after create_first_owner, so " +
			"homepage_users does not exist when the Owner asks for it")
	}
	// And only behind the proxy: it requires ingress, and its group must
	// not be requested where it was never declared.
	start := strings.LastIndex(src[:pkg], `if [ "$CONSOLE_BEHIND_PROXY" = 1 ]; then`)
	if start < 0 || strings.Contains(src[start:pkg], "\n  fi\n") {
		t.Error("homepage-wildstone is installed outside the CONSOLE_BEHIND_PROXY branch")
	}
}

// LLDAP will not create an account without an email, and Keystone
// refuses a member declared without one. An installer that never asked
// stopped at the step that makes somebody an Owner, on every install.
func TestTheInstallerAsksForTheOwnersEmail(t *testing.T) {
	src := installerSource(t)
	if !strings.Contains(src, `printf 'Email: '`) {
		t.Error("the installer does not ask for the first Owner's email")
	}
	if !strings.Contains(src, `\"email\":\"$ADMIN_EMAIL\"`) {
		t.Error("the first Owner's account is proposed without their email, " +
			"which Keystone refuses")
	}
}

// Resuming is a documented path (the answers file), and a resumed
// install proposes what an earlier run already installed. Keystone
// answers that with a no-op refusal, and treating it as a failure meant
// no install could resume past its first finished step.
func TestAResumedInstallSkipsWhatIsAlreadyInstalled(t *testing.T) {
	src := installerSource(t)
	idx := strings.Index(src, "enable_capability() {")
	if idx < 0 {
		t.Fatal("enable_capability is gone")
	}
	body := src[idx:]
	if end := strings.Index(body, "\n}\n"); end > 0 {
		body = body[:end]
	}
	if !strings.Contains(body, "no-op against current desired state") {
		t.Error("enable_capability fails on a capability an earlier run installed")
	}
}

// The directory is reached on 127.0.0.1, never "localhost". The helper
// runs a busybox wget in LLDAP's network namespace, where localhost
// resolves to ::1 first — and LLDAP listens on IPv4 only, so every
// attempt was refused before a password was ever checked.
func TestTheDirectoryIsReachedOverIPv4Loopback(t *testing.T) {
	if strings.Contains(installerSource(t), "localhost:17170") {
		t.Error("the installer reaches LLDAP as localhost, which resolves to ::1 " +
			"where LLDAP is not listening")
	}
}

// The installer rotates the directory's service password after LLDAP
// has created `admin`, and LLDAP otherwise reads that password only at
// creation. Without the forced reset the rotation never reaches the
// directory, and Authelia and the provisioning job are locked out.
func TestTheDirectoryAdoptsARotatedServicePassword(t *testing.T) {
	raw, err := os.ReadFile("../../packages/identity/package.yaml")
	if err != nil {
		t.Fatalf("reading the identity package: %v", err)
	}
	if !strings.Contains(string(raw), `LLDAP_FORCE_LDAP_USER_PASS_RESET: "always"`) {
		t.Error("LLDAP does not re-read its admin password at start, so the " +
			"installer's rotation of secret/lldap-admin-password never takes")
	}
}

// A variable a flag sets is never reset after the flags are parsed.
//
// The script is one flat file, so a top-level `NAME=""` placed below the
// parsing loop silently discards whatever the operator passed. That is
// how the Console's upstream address, declared as KEYSTONE_ADDR, emptied
// --keystone for every `--role node` run: the installer paired its own
// host with `-keystone ` and no value, and wardstoned refused to start.
//
// Only a bare re-assignment at column 0 counts. `NAME=$NAME` lines are
// the env file the node role writes, which carries the flag through.
func TestNoFlagIsResetAfterParsing(t *testing.T) {
	src := installerSource(t)
	loop := strings.Index(src, "while [ $# -gt 0 ]; do")
	done := strings.Index(src[loop:], "\ndone\n")
	if loop < 0 || done < 0 {
		t.Fatal("could not find the flag-parsing loop")
	}
	parse, after := src[loop:loop+done], src[loop+done:]
	for _, line := range strings.Split(parse, "\n") {
		i := strings.Index(line, `="$2"`)
		if i < 0 {
			continue
		}
		name := strings.TrimSpace(line[strings.LastIndex(line[:i], " ")+1 : i])
		for _, l := range strings.Split(after, "\n") {
			if strings.HasPrefix(l, name+"=") && l != name+"=$"+name {
				t.Errorf("%s is set by a flag and then reset after parsing: %q", name, l)
			}
		}
	}
}

// The homepage arrives with its two widgets, and the first Owner can see
// both. Neither needs configuring to be installed: files creates its
// folder on first use, and the assistant's card says how to give it a
// model. A dashboard that never offered them left the household nothing
// to find.
func TestTheHomepageArrivesWithItsWidgets(t *testing.T) {
	src := installerSource(t)
	owner := strings.LastIndex(src, "create_first_owner\n")
	for _, w := range []struct{ pkg, group string }{
		{"filedrop-wildstone", "filedrop_users"},
		{"assistant-wildstone", "assistant_users"},
	} {
		at := strings.Index(src, "enable_capability "+w.pkg)
		if at < 0 {
			t.Errorf("%s is not installed", w.pkg)
			continue
		}
		// Before the Owner who is put in its group, and behind the proxy
		// that serves it.
		if owner < 0 || at > owner {
			t.Errorf("%s is installed after create_first_owner, so %s does not exist yet", w.pkg, w.group)
		}
		start := strings.LastIndex(src[:at], `if [ "$CONSOLE_BEHIND_PROXY" = 1 ]; then`)
		if start < 0 || strings.Contains(src[start:at], "\n  fi\n") {
			t.Errorf("%s is installed outside the CONSOLE_BEHIND_PROXY branch", w.pkg)
		}
		if !regexp.MustCompile(`OWNER_GROUPS='[^']*"` + w.group + `"`).MatchString(src) {
			t.Errorf("the first Owner is not put in %s, so the widget is installed and hidden from them", w.group)
		}
	}
}
