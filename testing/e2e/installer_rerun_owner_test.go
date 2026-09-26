package e2e

import (
	"os/exec"
	"regexp"
	"strings"
	"testing"
)

// shellFunction extracts one top-level function from the installer, from
// "name() {" to the first line that is exactly "}".
func shellFunction(t *testing.T, src, name string) string {
	t.Helper()
	m := regexp.MustCompile(`(?ms)^` + regexp.QuoteMeta(name) + `\(\) \{\n.*?^\}\n`).FindString(src)
	if m == "" {
		t.Fatalf("%s is gone from the installer", name)
	}
	return m
}

// A re-run does not ask for the first Owner again. The name and address
// are in the answers file, and a password is only ever used to create an
// account — which a re-run does not do for one that exists (#362). It is
// run with no terminal at all: any prompt would fail to read /dev/tty.
func TestARerunDoesNotAskForTheOwnerAgain(t *testing.T) {
	src := installerSource(t)
	script := "set -euo pipefail\nlog() { echo \"LOG: $*\"; }\nanswers_save() { echo \"SAVED: $1\"; }\n" +
		shellFunction(t, src, "step_admin") + shellFunction(t, src, "ask_owner_password") +
		"ADMIN_USER=sam ADMIN_EMAIL=sam@example.com ADMIN_PASS=\nstep_admin\necho \"PASS=[$ADMIN_PASS]\"\n"
	cmd := exec.Command("setsid", "bash", "-c", script)
	cmd.Stdin = strings.NewReader("")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("step_admin on a re-run failed: %v\n%s", err, out)
	}
	got := string(out)
	for _, prompt := range []string{"Username:", "Email:", "Password:"} {
		if strings.Contains(got, prompt) {
			t.Fatalf("a re-run asked %q again:\n%s", prompt, got)
		}
	}
	if !strings.Contains(got, "first owner chosen previously: sam (sam@example.com)") || !strings.Contains(got, "PASS=[]") {
		t.Fatalf("output:\n%s", got)
	}
	if strings.Contains(got, "SAVED:") {
		t.Fatalf("a re-run rewrote the saved owner:\n%s", got)
	}

	// And the one case a re-run does need a password: the account was
	// never created. It is asked for there, and only there.
	create := shellFunction(t, src, "create_first_owner")
	notDeclared := create[strings.Index(create, "    *)"):]
	ask := strings.Index(notDeclared, `[ -n "$ADMIN_PASS" ] || ask_owner_password`)
	propose := strings.Index(notDeclared, "api POST /api/v1/household/members")
	if ask < 0 || propose < 0 || ask > propose {
		t.Fatal("create_first_owner does not ask for a missing password before creating the account")
	}
}
