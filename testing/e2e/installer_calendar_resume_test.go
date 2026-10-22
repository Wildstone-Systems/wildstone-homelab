package e2e

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Execute the real resume functions against a disconnected node and existing
// accounts. A password setter or an early plan submission aborts the test.
func TestInstallerCalendarResumePreservesCredentials(t *testing.T) {
	src := installerSource(t)
	var functions strings.Builder
	for _, name := range []string{"wait_agent", "seed_calendar_account", "grant_owner_groups", "create_first_owner"} {
		start := strings.Index(src, name+"() {")
		if start < 0 {
			t.Fatal(name)
		}
		end := strings.Index(src[start:], "\n}\n")
		if end < 0 {
			t.Fatal(name)
		}
		functions.WriteString(src[start:start+end+3] + "\n")
	}
	dir := t.TempDir()
	script := `set -euo pipefail
NODE_ID=test
ADMIN_USER=owner
ADMIN_EMAIL=owner@example.test
ADMIN_PASS=stale-installer-answer
CALENDAR_USER=radicale
CALENDAR_PASS=retained-service-password
CALENDAR_PROVISIONED=1
CONSOLE_BEHIND_PROXY=1
OWNER_GROUPS='"calendar_users"'
log() { :; }
fail() { echo "$*" >&2; exit 1; }
sleep() { :; }
lldap_login() { fail 'resume attempted directory provisioning'; }
set_owner_password() { fail 'resume reset Owner password'; }
docker() { fail 'resume changed Calendar password'; }
api() {
 case "$1 $2" in
 'GET /api/v1/nodes')
   n=$(cat "$TEST_DIR/polls"); n=$((n+1)); echo "$n" > "$TEST_DIR/polls"
   if [ "$n" -lt 4 ]; then printf '{"id":"test","connected":false}'; else printf '{"id":"test","connected":true}'; fi ;;
 'GET /api/v1/household/members') printf '[{"username":"radicale","groups":["calendar_users"]},{"username":"owner","groups":["console_users"]}]' ;;
 'PUT /api/v1/household/members/owner')
   [ "$(cat "$TEST_DIR/polls")" -ge 4 ] || fail 'plan submitted before reconnect'
   printf '{"id":"groups"}' ;;
 'POST /api/v1/plans/groups/approve') echo approved > "$TEST_DIR/approved"; printf '{}' ;;
 'GET /api/v1/plans/groups') printf '{"state":"applied"}' ;;
 'POST /api/v1/butler/members'|'POST /api/v1/butler/members/owner/admit') printf '{}' ;;
 'GET /api/v1/butler/members') printf '[{"member":"owner","role":"owner","admitted":true}]' ;;
 *) fail "unexpected API: $*" ;;
 esac
}
`
	script += functions.String() + "\nseed_calendar_account\ncreate_first_owner\n"
	path := filepath.Join(dir, "resume.sh")
	if err := os.WriteFile(filepath.Join(dir, "polls"), []byte("0"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(script), 0600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("bash", path)
	cmd.Env = append(os.Environ(), "TEST_DIR="+dir)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("resume: %v\n%s", err, out)
	}
	if _, err := os.Stat(filepath.Join(dir, "approved")); err != nil {
		t.Fatal("Owner group plan was not approved", err)
	}
}
