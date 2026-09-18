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
	// The check has to end in `fail`. Asserted on the verification block
	// rather than anywhere in the function, because a `fail` elsewhere
	// would satisfy a looser test while the owner check warned and
	// continued.
	verify := body[strings.LastIndex(body, "api GET /api/v1/butler/members"):]
	if !strings.Contains(verify, "|| fail ") {
		t.Errorf("the owner check does not abort the install:\n%s", verify)
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
	if strings.Contains(body, "ADMIN_PASS\"") && !strings.Contains(body, "LLDAP_ADMIN_PASS") {
		t.Error("the directory's service password is the operator's again")
	}
}
