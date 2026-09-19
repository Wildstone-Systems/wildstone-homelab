package reconcile

import (
	"strings"
	"testing"

	"github.com/chickenrat17/wildstone-homelab/keystone/resource"
)

// No line in the user list ever carries an empty field.
//
// This is the whole reason `-` exists, and the failure it prevents is
// severe rather than cosmetic: tab is IFS whitespace, so two tabs in a
// row are ONE separator to the job's `IFS=<tab> read`. An empty groups
// column shifts the revocations into it, and the provisioner then ADDS
// every membership it was told to take away — the exact inversion of
// the intent, from one absent character.
//
// The same collapse has already cost this file once, when an empty
// display name shifted an email into it and the first Owner was created
// with their group list as their address. So the assertion is over
// EVERY field rather than the two this slice added: a field that
// becomes optional later must not reintroduce it.
func TestNoUserLineHasAnEmptyField(t *testing.T) {
	out := renderLdapUsers([]resource.Resource{
		// Every combination of the two optional lists, plus the
		// display name that caused this the first time.
		user("a", "", "a@example.com", nil, nil),
		user("b", "B", "b@example.com", []string{"photos_users"}, nil),
		user("c", "C", "c@example.com", nil, []string{"media_users"}),
		user("d", "D", "d@example.com", []string{"photos_users"}, []string{"media_users"}),
	})
	for _, line := range strings.Split(string(out), "\n") {
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Split(line, "\t")
		if len(fields) != 5 {
			t.Errorf("line %q has %d fields, want 5", line, len(fields))
			continue
		}
		for i, f := range fields {
			if f == "" {
				t.Errorf("line %q has an empty field %d; `IFS=<tab> read` collapses "+
					"two tabs into one separator, so an empty field shifts every "+
					"later one left", line, i+1)
			}
		}
	}
}

// And the dash is what the empty lists render as, so the job can tell
// "none" from a name.
func TestAnEmptyListRendersAsADash(t *testing.T) {
	out := string(renderLdapUsers([]resource.Resource{
		user("a", "A", "a@example.com", nil, []string{"media_users"}),
	}))
	if !strings.Contains(out, "a\tA\ta@example.com\t-\tmedia_users") {
		t.Errorf("want the empty groups column written as a dash, got:\n%s", out)
	}
}

// Revocations are sorted, like groups, so the file — and the job's spec
// hash — is stable across ticks. An unstable hash re-runs the job on
// every reconcile forever.
func TestRevocationsAreSorted(t *testing.T) {
	out := string(renderLdapUsers([]resource.Resource{
		user("a", "A", "a@example.com", nil, []string{"photos_users", "docs_users", "media_users"}),
	}))
	if !strings.Contains(out, "docs_users,media_users,photos_users") {
		t.Errorf("revocations are not sorted:\n%s", out)
	}
}

func user(name, display, email string, groups, revoke []string) resource.Resource {
	return resource.Resource{
		Kind:     resource.KindLdapUser,
		Metadata: resource.Metadata{Name: name, Owner: "system", Node: "n1"},
		LdapUser: &resource.LdapUserSpec{
			Username: name, DisplayName: display, Email: email,
			Groups: groups, Revoke: revoke,
		},
	}
}
