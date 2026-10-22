package reconcile

import (
	"os/exec"
	"strings"
	"testing"

	"github.com/Wildstone-Systems/wildstone-homelab/keystone/resource"
)

// The users file survives the shell that reads it.
//
// The provisioning job parses each line with `IFS=<tab> read`, where tab
// is IFS whitespace and consecutive tabs collapse into one. A user with
// no display name therefore lost a field: the groups landed in the email
// and the user was put in no group. This runs the job's own read over
// the rendered file, because a test of the Go string alone passes on
// exactly the output that broke.
func TestTheUsersFileSurvivesTheJobsRead(t *testing.T) {
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh on this machine")
	}
	desired := []resource.Resource{{
		Kind: resource.KindLdapUser,
		LdapUser: &resource.LdapUserSpec{
			Username: "sandy", Email: "sandy@example.com",
			Groups: []string{"homepage_users", "console_users"},
		},
	}, {
		// No display name AND no groups — both optional fields empty at
		// once, which is the line the collapse would mangle worst. Slice
		// 8 added a fifth field, so an empty fourth would now shift the
		// REVOCATIONS into the groups column and the job would add every
		// membership it was told to remove.
		Kind: resource.KindLdapUser,
		LdapUser: &resource.LdapUserSpec{
			Username: "robin", Email: "robin@example.com",
			Revoke: []string{"media_users"},
		},
	}}
	cmd := exec.Command(sh, "-c", `while IFS="$(printf '\t')" read -r u d e g r; do
  case "$u" in ''|'#'*) continue ;; esac
  printf '%s|%s|%s|%s|%s\n' "$u" "$d" "$e" "$g" "$r"
done`)
	cmd.Stdin = strings.NewReader(string(renderLdapUsers(desired)))
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("running the job's read: %v", err)
	}
	got := strings.TrimSpace(string(out))
	want := strings.Join([]string{
		"robin|robin|robin@example.com|-|media_users",
		"sandy|sandy|sandy@example.com|console_users,homepage_users|-",
	}, "\n")
	if got != want {
		t.Errorf("the job reads the lines as\n%q\nwant\n%q", got, want)
	}
}
