package pkgtool

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/chickenrat17/wildstone-homelab/keystone/resource"
)

// Shell shipped inside a package was linted by nothing. CI's shellcheck
// job walks scripts/*.sh, and a ConfigFile's content is not a file in
// this repo — so the identity capability's group provisioner, the one
// piece of shell that runs against a live directory holding its admin
// password, had less checking than any script in scripts/.
//
// These two tests close that. The first lints every shipped .sh
// ConfigFile. The second RUNS the provisioner against a fake LLDAP,
// because linting would not have caught what actually was wrong with it.

// shippedShellScripts returns every .sh ConfigFile any package renders.
func shippedShellScripts(t *testing.T) map[string]string {
	t.Helper()
	out := map[string]string{}
	for pkg, rendered := range renderShippedPackages(t) {
		for _, r := range rendered {
			if r.Kind != resource.KindConfigFile || r.ConfigFile == nil {
				continue
			}
			if strings.HasSuffix(r.ConfigFile.Filename, ".sh") {
				out[pkg+"/"+r.ConfigFile.Filename] = r.ConfigFile.Content
			}
		}
	}
	return out
}

func TestShippedShellScriptsPassShellcheck(t *testing.T) {
	if _, err := exec.LookPath("shellcheck"); err != nil {
		t.Skip("shellcheck not on PATH; CI's ubuntu runner has it")
	}
	scripts := shippedShellScripts(t)
	if len(scripts) == 0 {
		t.Fatal("no shipped .sh ConfigFile found — the walk is broken, not the packages")
	}
	for name, content := range scripts {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), filepath.Base(name))
			if err := os.WriteFile(path, []byte(content), 0o700); err != nil {
				t.Fatal(err)
			}
			// -s sh: these run in images with no bash. A bashism that
			// works on a developer's laptop is a job that fails on the
			// node and nowhere else.
			cmd := exec.Command("shellcheck", "-s", "sh", path)
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Errorf("shellcheck failed:\n%s", out)
			}
		})
	}
}

// fakeToken stands in for the session token LLDAP hands back.
//
// Its SHAPE is what matters — a dot, a dash and an underscore, because
// the sed that extracts it has to survive all three. Its CONTENT is
// deliberately repetitive: the secret scanner flags any high-entropy
// string in a "token": position, and a realistic-looking literal here
// would be a third entry in .gitleaksignore. That file already says
// what to do at the third: stop writing key-shaped literals in tests.
// Low entropy costs this test nothing, because nothing here depends on
// the token being unguessable.
const fakeToken = "aaaa.bbbb-cccc_dddd"

// fakeLldap answers the two endpoints the provisioner calls, and records
// what it was asked to create.
type fakeLldap struct {
	mu           sync.Mutex
	created      []string
	createdUsers []string
	// memberships records "user->group" additions, so a test can assert
	// both that they happened and that they happened only once.
	memberships []string
	// existingUsers is the directory's user list before the run.
	existingUsers []string
	// existingMembers maps a username to the groups it is already in,
	// which is what makes the idempotency assertion meaningful.
	existingMembers map[string][]string
	// spaced makes the fake emit JSON with whitespace around separators.
	// LLDAP is compact today, but {"token": "x"} is equally valid JSON,
	// and the first version of this script silently found no token in
	// it. That bug is the reason this knob exists.
	spaced bool
}

func (f *fakeLldap) write(w http.ResponseWriter, v any) {
	b, _ := json.Marshal(v)
	if f.spaced {
		b, _ = json.MarshalIndent(v, "", "  ")
	}
	w.Header().Set("Content-Type", "application/json")
	w.Write(b) //nolint:errcheck
}

func (f *fakeLldap) handler(password string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body) //nolint:errcheck
		switch r.URL.Path {
		case "/auth/simple/login":
			if body["password"] != password {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			f.write(w, map[string]string{"token": fakeToken, "refreshToken": "r"})
		case "/api/graphql":
			if r.Header.Get("Authorization") != "Bearer "+fakeToken {
				w.WriteHeader(http.StatusForbidden)
				return
			}
			query, _ := body["query"].(string)
			if strings.Contains(query, "CreateGroup") {
				vars, _ := body["variables"].(map[string]any)
				name, _ := vars["name"].(string)
				if name == "explode" {
					f.write(w, map[string]any{"errors": []map[string]string{{"message": "nope"}}})
					return
				}
				f.mu.Lock()
				f.created = append(f.created, name)
				f.mu.Unlock()
				f.write(w, map[string]any{"data": map[string]any{"createGroup": map[string]int{"id": 1}}})
				return
			}
			if strings.Contains(query, "CreateUser") {
				vars, _ := body["variables"].(map[string]any)
				u, _ := vars["u"].(map[string]any)
				name, _ := u["id"].(string)
				if name == "explode" {
					f.write(w, map[string]any{"errors": []map[string]string{{"message": "nope"}}})
					return
				}
				f.mu.Lock()
				f.createdUsers = append(f.createdUsers, name+"|"+asString(u["displayName"])+"|"+asString(u["email"]))
				f.mu.Unlock()
				f.write(w, map[string]any{"data": map[string]any{"createUser": map[string]string{"id": name}}})
				return
			}
			if strings.Contains(query, "AddUserToGroup") {
				vars, _ := body["variables"].(map[string]any)
				user, _ := vars["u"].(string)
				gid, _ := vars["g"].(float64)
				f.mu.Lock()
				f.memberships = append(f.memberships, fmt.Sprintf("%s->%d", user, int(gid)))
				f.mu.Unlock()
				f.write(w, map[string]any{"data": map[string]any{"addUserToGroup": map[string]bool{"ok": true}}})
				return
			}
			// A single user's memberships, which the script reads before
			// adding any, so an existing membership is not re-added.
			if strings.Contains(query, "user(userId:") {
				name := ""
				if i := strings.Index(query, "\""); i >= 0 {
					if j := strings.Index(query[i+1:], "\""); j >= 0 {
						name = query[i+1 : i+1+j]
					}
				}
				groups := []map[string]any{}
				f.mu.Lock()
				for _, g := range f.existingMembers[name] {
					groups = append(groups, map[string]any{"displayName": g})
				}
				f.mu.Unlock()
				f.write(w, map[string]any{"data": map[string]any{"user": map[string]any{"groups": groups}}})
				return
			}
			if strings.Contains(query, "{users {id}}") {
				users := []map[string]any{}
				for _, u := range f.existingUsers {
					users = append(users, map[string]any{"id": u})
				}
				f.write(w, map[string]any{"data": map[string]any{"users": users}})
				return
			}
			// The existing directory. photos_users_extra is here to
			// prove the presence check is an exact match: a group whose
			// name merely PREFIXES a desired one must not satisfy it.
			groups := []map[string]any{
				{"id": 1, "displayName": "lldap_admin"},
				{"id": 2, "displayName": "photos_users_extra"},
			}
			// Groups created earlier in this same run must appear, or the
			// id lookup for a membership finds nothing — which is exactly
			// the first-install case, where every group is new.
			f.mu.Lock()
			for i, c := range f.created {
				groups = append(groups, map[string]any{"id": 100 + i, "displayName": c})
			}
			f.mu.Unlock()
			f.write(w, map[string]any{"data": map[string]any{"groups": groups}})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
}

func asString(v any) string {
	s, _ := v.(string)
	return s
}

// runProvisionerWithUsers is runProvisioner plus a rendered user list.
// Separate rather than a changed signature so the existing group-only
// callers keep asserting exactly what they asserted before.
func runProvisionerWithUsers(t *testing.T, script, url, password string, groups, users []string) (string, int) {
	t.Helper()
	dir := t.TempDir()
	sh := filepath.Join(dir, "provision-groups.sh")
	if err := os.WriteFile(sh, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	list := filepath.Join(dir, "groups")
	if err := os.WriteFile(list, []byte("# managed\n"+strings.Join(groups, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ulist := filepath.Join(dir, "users")
	if err := os.WriteFile(ulist, []byte("# managed\n"+strings.Join(users, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("/bin/sh", sh)
	cmd.Env = append(os.Environ(),
		"LLDAP_URL="+url, "GROUPS_FILE="+list, "USERS_FILE="+ulist,
		"LLDAP_ADMIN_PASSWORD="+password, "LOGIN_ATTEMPTS=2")
	out, err := cmd.CombinedOutput()
	code := 0
	var ee *exec.ExitError
	if err != nil {
		if !asExitError(err, &ee) {
			t.Fatalf("running the provisioner: %v\n%s", err, out)
		}
		code = ee.ExitCode()
	}
	return string(out), code
}

// runProvisioner runs the shipped script — the rendered one, not a copy
// — against srv, with groups as the rendered list.
func runProvisioner(t *testing.T, script, url, password string, groups []string) (string, int) {
	t.Helper()
	dir := t.TempDir()
	sh := filepath.Join(dir, "provision-groups.sh")
	if err := os.WriteFile(sh, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	list := filepath.Join(dir, "groups")
	body := "# managed by wildstone-keystone\n" + strings.Join(groups, "\n") + "\n"
	if err := os.WriteFile(list, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command("/bin/sh", sh)
	cmd.Env = append(os.Environ(),
		"LLDAP_URL="+url, "GROUPS_FILE="+list, "LLDAP_ADMIN_PASSWORD="+password,
		// The shipped default is 10 attempts at 2s apart. The failures
		// exercised here are permanent, so waiting out the real retry
		// budget would add 20s to the suite to observe the same exit.
		"LOGIN_ATTEMPTS=2")
	out, err := cmd.CombinedOutput()
	code := 0
	var ee *exec.ExitError
	if err != nil {
		if !asExitError(err, &ee) {
			t.Fatalf("running the provisioner: %v\n%s", err, out)
		}
		code = ee.ExitCode()
	}
	return string(out), code
}

func asExitError(err error, target **exec.ExitError) bool {
	ee, ok := err.(*exec.ExitError)
	if ok {
		*target = ee
	}
	return ok
}

func provisionerScript(t *testing.T) string {
	t.Helper()
	for name, content := range shippedShellScripts(t) {
		if strings.HasSuffix(name, "/provision-groups.sh") {
			return content
		}
	}
	t.Fatal("the identity package ships no provision-groups.sh")
	return ""
}

// TestProvisionerCreatesOnlyMissingGroups is the test the script needed
// and did not have. It found a real bug: the token was pulled out with a
// sed that required compact JSON, so a perfectly valid spaced response
// yielded no token and the job failed every time.
func TestProvisionerCreatesOnlyMissingGroups(t *testing.T) {
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("no /bin/sh")
	}
	script := provisionerScript(t)

	for _, spaced := range []bool{false, true} {
		name := "compact json"
		if spaced {
			name = "json with whitespace"
		}
		t.Run(name, func(t *testing.T) {
			fake := &fakeLldap{spaced: spaced}
			srv := httptest.NewServer(fake.handler("hunter2"))
			defer srv.Close()

			out, code := runProvisioner(t, script, srv.URL, "hunter2",
				[]string{"photos_users", "media_users", "lldap_admin"})
			if code != 0 {
				t.Fatalf("exit %d, want 0\n%s", code, out)
			}
			got := strings.Join(fake.created, ",")
			// lldap_admin exists, so it is left alone. photos_users is
			// created despite photos_users_extra existing.
			if want := "photos_users,media_users"; got != want {
				t.Errorf("created %q, want %q — an existing group whose name merely prefixes a "+
					"desired one must not satisfy it\n%s", got, want, out)
			}
		})
	}
}

// TestProvisionerFailsLoudly: a partially provisioned directory must not
// look like a finished job, or the reconciler records the run as done
// and the missing group is never created.
func TestProvisionerFailsLoudly(t *testing.T) {
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("no /bin/sh")
	}
	script := provisionerScript(t)

	t.Run("a group that will not create", func(t *testing.T) {
		fake := &fakeLldap{}
		srv := httptest.NewServer(fake.handler("hunter2"))
		defer srv.Close()
		out, code := runProvisioner(t, script, srv.URL, "hunter2", []string{"media_users", "explode"})
		if code == 0 {
			t.Errorf("exit 0 despite a group that failed to create\n%s", out)
		}
		// The groups that CAN be created still are: one bad group must
		// not strand the rest.
		if strings.Join(fake.created, ",") != "media_users" {
			t.Errorf("created %v, want the good group still created", fake.created)
		}
	})

	t.Run("wrong admin password", func(t *testing.T) {
		fake := &fakeLldap{}
		srv := httptest.NewServer(fake.handler("hunter2"))
		defer srv.Close()
		out, code := runProvisioner(t, script, srv.URL, "wrong", []string{"media_users"})
		if code == 0 {
			t.Errorf("exit 0 despite failing to authenticate\n%s", out)
		}
		if len(fake.created) != 0 {
			t.Errorf("created %v after a failed login", fake.created)
		}
	})
}

// TestProvisionerCreatesUsers runs the shipped script against a directory
// that already holds one of the two people, and asserts the three things
// ADR-W020 A3 promises: the missing person is created, memberships are
// added, and a membership that already exists is not re-added.
//
// The second half is what makes it worth running the real script: the
// membership check reads a per-user query whose response shape the shell
// matches textually, and a textual match that drifts is invisible until
// someone is silently missing from a group.
func TestProvisionerCreatesUsers(t *testing.T) {
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("no /bin/sh")
	}
	script := provisionerScript(t)
	fake := &fakeLldap{
		existingUsers:   []string{"alice"},
		existingMembers: map[string][]string{"alice": {"photos_users"}},
	}
	srv := httptest.NewServer(fake.handler("pw"))
	defer srv.Close()

	out, code := runProvisionerWithUsers(t, script, srv.URL, "pw",
		[]string{"photos_users"},
		[]string{
			"alice\tAlice Example\talice@example.com\tphotos_users",
			"bob\tBob Example\tbob@example.com\tphotos_users",
		})
	if code != 0 {
		t.Fatalf("provisioner exited %d:\n%s", code, out)
	}

	// bob did not exist and must have been created, with his fields.
	if len(fake.createdUsers) != 1 || fake.createdUsers[0] != "bob|Bob Example|bob@example.com" {
		t.Errorf("want bob created once with his display name and email, got %v\n%s", fake.createdUsers, out)
	}
	// alice existed: not re-created, and already in photos_users, so not
	// re-added. bob is added exactly once.
	for _, m := range fake.memberships {
		if strings.HasPrefix(m, "alice->") {
			t.Errorf("alice was re-added to a group she is already in: %v\n%s", fake.memberships, out)
		}
	}
	if len(fake.memberships) != 1 || !strings.HasPrefix(fake.memberships[0], "bob->") {
		t.Errorf("want exactly one membership add for bob, got %v\n%s", fake.memberships, out)
	}
}
