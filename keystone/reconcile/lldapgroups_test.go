package reconcile

import (
	"strings"
	"testing"

	"github.com/Wildstone-Systems/wildstone-homelab/keystone/resource"
)

func groupRes(name string) resource.Resource {
	return resource.Resource{
		ApiVersion: resource.ApiVersion,
		Kind:       resource.KindLdapGroup,
		Metadata:   resource.Metadata{Name: name, Owner: "capability/photos", Node: "node-a"},
		LdapGroup:  &resource.LdapGroupSpec{Name: name},
	}
}

// groupLines returns the rendered file's group names, without the
// comment header — which is what the provisioning script skips too.
func groupLines(b []byte) []string {
	var out []string
	for _, line := range strings.Split(string(b), "\n") {
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		out = append(out, line)
	}
	return out
}

func TestRenderLdapGroupsIsSortedAndDeduped(t *testing.T) {
	// Two packages declaring the same group is legitimate — a shared
	// family_users. The directory has one group either way.
	got := groupLines(renderLdapGroups([]resource.Resource{
		groupRes("photos_users"),
		groupRes("media_users"),
		groupRes("photos_users"),
		containerResource("noise", testDigestImage),
	}))
	want := []string{"media_users", "photos_users"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("rendered %v, want %v", got, want)
	}
}

// TestEmptyGroupListStillRenders: a deployment with no LdapGroup must
// still produce a readable file. The script reads it with a `while read`
// loop, and a missing file is a non-zero exit on every tick forever.
func TestEmptyGroupListStillRenders(t *testing.T) {
	b := renderLdapGroups(nil)
	if len(b) == 0 || !strings.HasPrefix(string(b), "#") {
		t.Errorf("empty deployment rendered %q, want a comment header and nothing else", b)
	}
	if got := groupLines(b); len(got) != 0 {
		t.Errorf("empty deployment rendered groups %v", got)
	}
}

// TestNewGroupRerunsTheProvisioningJob is the mechanism the whole
// feature rests on. Declaring a group has to reach the directory, and
// the only thing that carries it there is the job re-running — which
// happens because the group list is in the job's config digest.
func TestNewGroupRerunsTheProvisioningJob(t *testing.T) {
	provisioner := &resource.ContainerSpec{
		Image: testDigestImage,
		Mounts: []resource.Mount{
			{Source: testManagedPrefix + "/identity", Target: testManagedPrefix + "/identity", ReadOnly: true},
			{Source: testManagedPrefix + "/lldap", Target: testManagedPrefix + "/lldap", ReadOnly: true},
		},
	}

	before := managedConfigContents(testManagedPrefix, byKey(groupRes("media_users")))
	after := managedConfigContents(testManagedPrefix, byKey(groupRes("media_users"), groupRes("photos_users")))

	h1 := specHash(provisioner, ContainerNetwork, configDigest(cfgPrefix, provisioner, before), nil)
	h2 := specHash(provisioner, ContainerNetwork, configDigest(cfgPrefix, provisioner, after), nil)
	if h1 == h2 {
		t.Fatal("declaring a new LdapGroup left the provisioning job's spec hash unchanged: the group " +
			"is written to the list and the job never runs again, so the group is never created and " +
			"every login that requires it fails")
	}
}

// TestGroupListIsAPrereq: it must be on disk before the job that reads
// it is planned, which is what being a prereq means.
func TestGroupListIsAPrereq(t *testing.T) {
	files := nodePrereqFiles(testManagedPrefix, []resource.Resource{groupRes("media_users")})
	for _, f := range files {
		if f.path != lldapGroupsPath(testManagedPrefix) {
			continue
		}
		if f.materialize {
			t.Error("the group list is marked for secret materialization; it carries no secret references")
		}
		if !strings.Contains(string(f.want), "media_users") {
			t.Errorf("the prereq's content does not carry the declared group: %q", f.want)
		}
		return
	}
	t.Errorf("the group list is not among the node prereqs: %v", files)
}
