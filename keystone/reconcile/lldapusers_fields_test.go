package reconcile

import (
	"os/exec"
	"strings"
	"testing"

	"github.com/chickenrat17/wildstone-homelab/keystone/resource"
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
	}}
	cmd := exec.Command(sh, "-c", `while IFS="$(printf '\t')" read -r u d e g; do
  case "$u" in ''|'#'*) continue ;; esac
  printf '%s|%s|%s|%s\n' "$u" "$d" "$e" "$g"
done`)
	cmd.Stdin = strings.NewReader(string(renderLdapUsers(desired)))
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("running the job's read: %v", err)
	}
	got := strings.TrimSpace(string(out))
	want := "sandy|sandy|sandy@example.com|console_users,homepage_users"
	if got != want {
		t.Errorf("the job reads the line as %q, want %q", got, want)
	}
}
