package api

// The household portal's door and its two surfaces (#197, #198).
//
// Most of this file is about what the portal REFUSES, for the reason
// the Console's forward-auth tests are: this is an authentication built
// out of request headers, and a path that is safe only when every
// condition happens to hold is a path one refactor away from not being
// safe at all.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/chickenrat17/wildstone-homelab/keystone/resource"
	"github.com/chickenrat17/wildstone-homelab/keystone/runtimedb"
)

// portalHarness is a deployment with both household surfaces installed
// and a storage root to put things in.
func portalHarness(t *testing.T) (*harness, string) {
	t.Helper()
	h := newHarness(t)
	h.s.SetTrustedIngress([]string{"127.0.0.1", "::1"})
	root := t.TempDir()

	for _, r := range []resource.Resource{
		{
			ApiVersion: "wildstone.dev/v1", Kind: resource.KindLdapGroup,
			Metadata:  resource.Metadata{Name: "filedrop-users", Owner: "capability/filedrop", Package: "filedrop-wildstone", Node: "n1"},
			LdapGroup: &resource.LdapGroupSpec{Name: "filedrop_users"},
		},
		{
			ApiVersion: "wildstone.dev/v1", Kind: resource.KindConfigFile,
			Metadata:   resource.Metadata{Name: "filedrop-settings", Owner: "capability/filedrop", Package: "filedrop-wildstone", Node: "n1"},
			ConfigFile: &resource.ConfigFileSpec{Filename: "settings.yaml", Content: "root: " + root + "\nmaxUploadMB: 1\n"},
		},
		{
			ApiVersion: "wildstone.dev/v1", Kind: resource.KindLdapGroup,
			Metadata:  resource.Metadata{Name: "assistant-users", Owner: "capability/assistant", Package: "assistant-wildstone", Node: "n1"},
			LdapGroup: &resource.LdapGroupSpec{Name: "assistant_users"},
		},
	} {
		if err := h.s.State.Put(r); err != nil {
			t.Fatalf("Put(%s): %v", r.Metadata.Name, err)
		}
	}
	return h, root
}

// member issues a request the way the identity proxy would, for a
// household member in the named groups.
func (h *harness) member(method, path, user, groups string, body io.Reader, contentType string) *http.Response {
	h.t.Helper()
	req, err := http.NewRequest(method, h.srv.URL+path, body)
	if err != nil {
		h.t.Fatalf("NewRequest: %v", err)
	}
	if user != "" {
		req.Header.Set(TrustedIngressHeader, user)
	}
	if groups != "" {
		req.Header.Set(ConsoleGroupsHeader, groups)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	// What a browser sends on a write from the portal's own page, and
	// the reason it is HERE rather than in each test: a write without it
	// is refused as cross-site, so a helper that omitted it would be
	// testing a request no member can make. The refusal has its own
	// test, which builds the request by hand.
	if method != http.MethodGet && method != http.MethodHead {
		req.Header.Set("Origin", h.srv.URL)
		req.Header.Set("Sec-Fetch-Site", "same-origin")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatalf("%s %s: %v", method, path, err)
	}
	return resp
}

// upload posts one file as the widget does: the body IS the file, the
// destination and the offered name are query parameters.
func (h *harness) upload(user, namespace, name, body string) (*http.Response, map[string]any) {
	h.t.Helper()
	path := fmt.Sprintf("/api/v1/filedrop/upload?namespace=%s&name=%s",
		url.QueryEscape(namespace), url.QueryEscape(name))
	res := h.member(http.MethodPost, path, user, "filedrop_users", strings.NewReader(body), "application/octet-stream")
	defer res.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(res.Body).Decode(&out)
	return res, out
}

// unthrottled empties the rate limiter.
//
// Refusals share one strict bucket — five, then one a second — so that
// nothing can be probed quickly. A test that exercises eight refusals
// in a row would spend most of itself asserting that, so it resets the
// buckets between cases and the limiter has its own test.
func (h *harness) unthrottled() {
	h.t.Helper()
	h.s.limiter = newLimiter()
}

// The acceptance criterion, end to end: a member of the group uploads
// from a browser, and what lands is a real file under the root an
// administrator configured — where the household's other applications
// read it.
func TestAMemberCanUploadAndSeeItBack(t *testing.T) {
	h, root := portalHarness(t)

	res, body := h.upload("sam", "shared", "IMG_4821.jpg", "pretend-jpeg")
	if res.StatusCode != http.StatusCreated {
		t.Fatalf("upload = %d (%v)", res.StatusCode, body)
	}
	stored, _ := body["name"].(string)
	if stored != "IMG_4821.jpg" {
		t.Fatalf("stored as %q", stored)
	}
	onDisk := filepath.Join(root, "Shared", stored)
	if b, err := os.ReadFile(onDisk); err != nil || string(b) != "pretend-jpeg" {
		t.Fatalf("the Files application would not find it at %s: %q, %v", onDisk, b, err)
	}

	var recent struct {
		Items []struct {
			Name      string `json:"name"`
			Namespace string `json:"namespace"`
		} `json:"items"`
	}
	res = h.member(http.MethodGet, "/api/v1/filedrop/recent", "sam", "filedrop_users", nil, "")
	defer res.Body.Close()
	if err := json.NewDecoder(res.Body).Decode(&recent); err != nil {
		t.Fatal(err)
	}
	if len(recent.Items) != 1 || recent.Items[0].Name != stored || recent.Items[0].Namespace != "shared" {
		t.Fatalf("recent = %+v, want the file that was just uploaded", recent.Items)
	}
}

// #197: a member without write authority cannot upload by manipulating
// the UI, the URL, or the API request. The group IS the authority, and
// it is checked on the endpoint rather than on the page.
func TestTheDoorRefusesEveryoneItShould(t *testing.T) {
	h, _ := portalHarness(t)

	cases := []struct {
		name, user, groups string
		trusted            []string
		want               int
	}{
		{name: "in the group", user: "sam", groups: "filedrop_users", trusted: []string{"127.0.0.1", "::1"}, want: http.StatusCreated},
		{name: "several groups", user: "sam", groups: "media_users,filedrop_users", trusted: []string{"127.0.0.1", "::1"}, want: http.StatusCreated},
		{name: "signed in, not in the group", user: "guest", groups: "media_users", trusted: []string{"127.0.0.1", "::1"}, want: http.StatusForbidden},
		{name: "no group header at all", user: "guest", groups: "", trusted: []string{"127.0.0.1", "::1"}, want: http.StatusForbidden},
		{name: "a group that merely starts the same", user: "guest", groups: "filedrop_users_ro", trusted: []string{"127.0.0.1", "::1"}, want: http.StatusForbidden},
		{name: "no forwarded user", user: "", groups: "filedrop_users", trusted: []string{"127.0.0.1", "::1"}, want: http.StatusForbidden},
		{name: "not from the proxy", user: "sam", groups: "filedrop_users", trusted: []string{"10.9.9.9"}, want: http.StatusForbidden},
		{name: "no trusted ingress configured", user: "sam", groups: "filedrop_users", trusted: nil, want: http.StatusForbidden},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h.s.SetTrustedIngress(c.trusted)
			h.unthrottled()
			res := h.member(http.MethodPost,
				"/api/v1/filedrop/upload?namespace=shared&name="+url.QueryEscape(c.name+".txt"),
				c.user, c.groups, strings.NewReader("x"), "application/octet-stream")
			res.Body.Close()
			if res.StatusCode != c.want {
				t.Errorf("upload as %q in %q = %d, want %d", c.user, c.groups, res.StatusCode, c.want)
			}
		})
	}
}

// The property that makes it safe to serve a household surface from the
// control plane's own listener: the identity the portal produces holds
// no RBAC verb, so the same browser, the same headers and one guessed
// URL do not read the house.
func TestAPortalMemberIsNotAPrincipal(t *testing.T) {
	h, _ := portalHarness(t)

	for _, path := range []string{
		"/api/v1/resources", "/api/v1/nodes", "/api/v1/audit", "/api/v1/butlers",
		"/api/v1/plans", "/api/v1/principals", "/api/v1/whoami",
	} {
		h.unthrottled()
		res := h.member(http.MethodGet, path, "sam", "filedrop_users,assistant_users", nil, "")
		res.Body.Close()
		if res.StatusCode != http.StatusUnauthorized {
			t.Errorf("GET %s as a portal member = %d, want 401: a household member is not a principal",
				path, res.StatusCode)
		}
	}
}

// An uninstalled capability leaves no endpoint answering — and looks
// absent rather than forbidden, so nothing has to be inferred from the
// difference.
func TestAnUninstalledSurfaceIsAbsent(t *testing.T) {
	h := newHarness(t)
	h.s.SetTrustedIngress([]string{"127.0.0.1", "::1"})

	for _, path := range []string{
		"/filedrop/", "/filedrop/widget", "/api/v1/filedrop/recent",
		"/assistant/", "/assistant/widget", "/api/v1/assistant/agents",
	} {
		h.unthrottled()
		res := h.member(http.MethodGet, path, "sam", "filedrop_users,assistant_users", nil, "")
		res.Body.Close()
		if res.StatusCode != http.StatusNotFound {
			t.Errorf("GET %s with nothing installed = %d, want 404", path, res.StatusCode)
		}
	}
}

// #197, at the HTTP boundary rather than in the store: a name a caller
// controls entirely cannot put a file anywhere but the destination.
func TestUploadedNamesCannotEscapeOverHTTP(t *testing.T) {
	h, root := portalHarness(t)

	for _, name := range []string{"../escaped.txt", "/etc/escaped.txt", "..%2f..%2fescaped.txt", "....//escaped.txt"} {
		res, body := h.upload("sam", "shared", name, "x")
		res.Body.Close()
		if res.StatusCode != http.StatusCreated {
			t.Fatalf("upload %q = %d (%v)", name, res.StatusCode, body)
		}
		stored, _ := body["name"].(string)
		if strings.ContainsAny(stored, `/\`) {
			t.Errorf("%q was stored as %q", name, stored)
		}
	}
	// Nothing outside the Shared directory, at all.
	var stray []string
	if err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && filepath.Dir(p) != filepath.Join(root, "Shared") {
			stray = append(stray, p)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(stray) > 0 {
		t.Errorf("files landed outside the destination: %v", stray)
	}
}

// A destination that is not in the table is refused rather than
// coerced, and an oversized upload is refused rather than truncated.
func TestTheUploadEndpointIsBounded(t *testing.T) {
	h, _ := portalHarness(t)

	res, _ := h.upload("sam", "etc", "x.txt", "x")
	res.Body.Close()
	if res.StatusCode != http.StatusBadRequest {
		t.Errorf("upload to an unknown destination = %d, want 400", res.StatusCode)
	}
	// The harness configures a one-megabyte ceiling.
	res, _ = h.upload("sam", "shared", "big.bin", strings.Repeat("x", (1<<20)+1))
	res.Body.Close()
	if res.StatusCode != http.StatusRequestEntityTooLarge {
		t.Errorf("upload over the limit = %d, want 413", res.StatusCode)
	}
}

// What comes back out is an attachment and an opaque byte stream,
// whatever was uploaded. This endpoint shares an origin with the
// portal's own pages, so serving an uploaded file as what it claims to
// be would hand anyone who can upload a script on the surface that
// holds everyone's session.
func TestADownloadIsNeverServedAsItsOwnContentType(t *testing.T) {
	h, _ := portalHarness(t)
	res, body := h.upload("sam", "shared", "evil.html", "<script>alert(1)</script>")
	res.Body.Close()
	if res.StatusCode != http.StatusCreated {
		t.Fatalf("upload = %d (%v)", res.StatusCode, body)
	}

	res = h.member(http.MethodGet, "/api/v1/filedrop/file?namespace=shared&name=evil.html",
		"sam", "filedrop_users", nil, "")
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("download = %d", res.StatusCode)
	}
	if got := res.Header.Get("Content-Type"); got != "application/octet-stream" {
		t.Errorf("Content-Type %q, want application/octet-stream", got)
	}
	if got := res.Header.Get("Content-Disposition"); !strings.HasPrefix(got, "attachment;") {
		t.Errorf("Content-Disposition %q, want an attachment", got)
	}
	if got := res.Header.Get("X-Content-Type-Options"); got != "nosniff" {
		t.Errorf("X-Content-Type-Options %q, want nosniff", got)
	}
}

// The pages are framable by the household's own dashboard and by
// nothing else.
func TestAPortalPageMayOnlyBeFramedByTheDashboard(t *testing.T) {
	h, _ := portalHarness(t)
	if err := h.s.State.Put(resource.Resource{
		ApiVersion: "wildstone.dev/v1", Kind: resource.KindRoute,
		Metadata: resource.Metadata{Name: "home", Owner: "capability/dashboard", Node: "n1"},
		Route: &resource.RouteSpec{
			Host: "home.example.com", Upstream: "container/homepage:3000",
			Auth: "authelia_oidc", Classification: "family_public",
		},
	}); err != nil {
		t.Fatal(err)
	}

	res := h.member(http.MethodGet, "/filedrop/widget", "sam", "filedrop_users", nil, "")
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("the widget page = %d", res.StatusCode)
	}
	csp := res.Header.Get("Content-Security-Policy")
	if !strings.Contains(csp, "frame-ancestors 'self' https://home.example.com") {
		t.Errorf("frame-ancestors does not name the dashboard: %q", csp)
	}
	// Under the tailnet posture the dashboard is served on 443 and a
	// browser framing from it sends an origin with no port at all, so
	// both forms have to be named.
	if err := h.s.State.Put(resource.Resource{
		ApiVersion: "wildstone.dev/v1", Kind: resource.KindIngress,
		Metadata: resource.Metadata{Name: "ingress", Owner: "capability/ingress", Node: "n1"},
		Ingress:  &resource.IngressSpec{TLS: "tailscale", TailnetName: "house.tailnet.ts.net"},
	}); err != nil {
		t.Fatal(err)
	}
	res2 := h.member(http.MethodGet, "/filedrop/widget", "sam", "filedrop_users", nil, "")
	defer res2.Body.Close()
	csp = res2.Header.Get("Content-Security-Policy")
	for _, want := range []string{"https://house.tailnet.ts.net:443", "https://house.tailnet.ts.net "} {
		if !strings.Contains(csp, want) {
			t.Errorf("frame-ancestors is missing %q: %q", want, csp)
		}
	}
	if !strings.Contains(csp, "default-src 'none'") {
		t.Errorf("the page may fetch from anywhere: %q", csp)
	}
	page, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(page, []byte("Drop files or photos here")) {
		t.Errorf("the widget page is not the widget")
	}
}

// #198's acceptance criteria, as one test: the roster is what the
// member was granted, an operator Butler is not on it, and naming one
// by hand does not reach it either.
func TestTheAssistantSurfaceIsHouseholdOnly(t *testing.T) {
	h, _ := portalHarness(t)
	for _, m := range []runtimedb.Model{
		{Name: "kitchen-box", Kind: runtimedb.ModelLocal, Provider: "ollama", ProviderModel: "llama",
			Readiness: runtimedb.ModelReady, Enabled: true},
	} {
		if err := h.s.DB.UpsertModel(m, "root-admin"); err != nil {
			t.Fatal(err)
		}
	}
	for _, member := range []string{"sam", "giulia"} {
		if err := h.s.DB.RecordButlerMember(member, runtimedb.RoleMember, "", ""); err != nil {
			t.Fatal(err)
		}
		if err := h.s.DB.AdmitButlerMember(member, "root-admin"); err != nil {
			t.Fatal(err)
		}
	}
	for _, b := range []runtimedb.Butler{
		{Name: "sandy", Surface: runtimedb.ButlerSurfaceFamily, Profile: runtimedb.ProfileHousehold, Model: "kitchen-box"},
		{Name: "operator", Surface: runtimedb.ButlerSurfaceAdmin, Profile: runtimedb.ProfileOperator, Model: "kitchen-box"},
	} {
		if _, err := h.s.DB.CreateButler(b, "root-admin"); err != nil {
			t.Fatal(err)
		}
		// Both granted to the same person, deliberately: the surface
		// ceiling has to hold for somebody who legitimately holds the
		// operator Butler elsewhere.
		if err := h.s.DB.GrantButlerAccess(b.Name, "sam", "root-admin"); err != nil {
			t.Fatal(err)
		}
	}

	var roster struct {
		Agents []struct {
			Name string `json:"name"`
		} `json:"agents"`
	}
	res := h.member(http.MethodGet, "/api/v1/assistant/agents", "sam", "assistant_users", nil, "")
	if err := json.NewDecoder(res.Body).Decode(&roster); err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if len(roster.Agents) != 1 || roster.Agents[0].Name != "sandy" {
		t.Fatalf("roster = %+v, want only the household butler", roster.Agents)
	}

	// Naming the operator Butler by hand — the request the client would
	// never send — is refused, and refused in the shape that does not
	// confirm it exists.
	ask := func(agent string) (int, string) {
		body, _ := json.Marshal(map[string]string{"agent": agent, "text": "install a package"})
		res := h.member(http.MethodPost, "/api/v1/assistant/ask", "sam", "assistant_users",
			bytes.NewReader(body), "application/json")
		defer res.Body.Close()
		var out map[string]string
		_ = json.NewDecoder(res.Body).Decode(&out)
		return res.StatusCode, out["error"]
	}
	status, msg := ask("operator")
	if status != http.StatusNotFound {
		t.Errorf("asking the operator butler from the household surface = %d, want 404", status)
	}
	if strings.Contains(msg, "operator") && strings.Contains(strings.ToLower(msg), "surface") {
		t.Errorf("the refusal describes the butler it refused: %q", msg)
	}
	// A Butler nobody granted this person is the same answer.
	if status, _ := ask("sandy"); status == http.StatusNotFound {
		t.Errorf("the household butler this member WAS granted answered 404")
	}
	res = h.member(http.MethodGet, "/api/v1/assistant/agents", "giulia", "assistant_users", nil, "")
	if err := json.NewDecoder(res.Body).Decode(&roster); err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if len(roster.Agents) != 0 {
		t.Errorf("a member granted nothing sees %+v", roster.Agents)
	}
}

// The manager's two writes, through the door.
func TestTheFilesManagerDeletesAndRenamesThroughTheDoor(t *testing.T) {
	h, root := portalHarness(t)
	if _, got := h.upload("sandy", "shared", "holiday.jpg", "bytes"); got == nil {
		t.Fatal("upload failed")
	}

	// Rename, and the normalised result comes back.
	res := h.member(http.MethodPost,
		"/api/v1/filedrop/rename?namespace=shared&name=holiday.jpg&to="+url.QueryEscape("../summer trip.jpg"),
		"sandy", "filedrop_users", nil, "")
	var renamed struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(res.Body).Decode(&renamed); err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if strings.ContainsAny(renamed.Name, "/\\") {
		t.Fatalf("renamed to %q, which is a path", renamed.Name)
	}
	if _, err := os.Stat(filepath.Join(root, "Shared", renamed.Name)); err != nil {
		t.Fatalf("%q is not in the namespace: %v", renamed.Name, err)
	}

	// A member not in the group cannot delete, and this is the
	// acceptance criterion #197 states: not by altering the request.
	res = h.member(http.MethodPost,
		"/api/v1/filedrop/delete?namespace=shared&name="+url.QueryEscape(renamed.Name),
		"sandy", "media_users", nil, "")
	res.Body.Close()
	if res.StatusCode != http.StatusForbidden {
		t.Errorf("delete without the group: status %d, want 403", res.StatusCode)
	}
	if _, err := os.Stat(filepath.Join(root, "Shared", renamed.Name)); err != nil {
		t.Errorf("the file was deleted by a refused request: %v", err)
	}

	// And with it, it goes.
	res = h.member(http.MethodPost,
		"/api/v1/filedrop/delete?namespace=shared&name="+url.QueryEscape(renamed.Name),
		"sandy", "filedrop_users", nil, "")
	res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("delete: status %d", res.StatusCode)
	}
	if _, err := os.Stat(filepath.Join(root, "Shared", renamed.Name)); !os.IsNotExist(err) {
		t.Errorf("still on disk: %v", err)
	}
}

// Deleting is a thing that happened in the house; renaming is a thing
// that happened to a filename. Only one belongs in "Today at home".
func TestOnlyTheConsequentialFileWritesReachTheHouseholdStream(t *testing.T) {
	h, _ := portalHarness(t)
	h.upload("sandy", "shared", "receipt.pdf", "bytes")
	h.member(http.MethodPost, "/api/v1/filedrop/rename?namespace=shared&name=receipt.pdf&to=invoice.pdf",
		"sandy", "filedrop_users", nil, "").Body.Close()
	h.member(http.MethodPost, "/api/v1/filedrop/delete?namespace=shared&name=invoice.pdf",
		"sandy", "filedrop_users", nil, "").Body.Close()

	events, err := h.s.DB.HouseholdEventsAfter(0, 50)
	if err != nil {
		t.Fatal(err)
	}
	var types []string
	for _, e := range events {
		types = append(types, e.Type)
	}
	joined := strings.Join(types, ",")
	for _, want := range []string{"filedrop.uploaded", "filedrop.deleted"} {
		if !strings.Contains(joined, want) {
			t.Errorf("%s is not in the household stream: %v", want, types)
		}
	}
	if strings.Contains(joined, "filedrop.renamed") {
		t.Errorf("a rename reached the household stream: %v", types)
	}
	// And no event carries the filename: the operator's log is the one
	// that names the file.
	for _, e := range events {
		if strings.Contains(string(e.Payload), "invoice") || strings.Contains(e.Entity, "invoice") {
			t.Errorf("a household event carries the filename: %s %s", e.Type, e.Entity)
		}
	}
}

// The personal namespace, through the door.
//
// The store's own tests prove the isolation; these prove the DOOR does
// not undo it — that the member the space resolves from is the one the
// ingress proved, and not anything in the request.
func TestOneMembersPersonalFilesAreNotReachableByAnother(t *testing.T) {
	h, root := portalHarness(t)

	// Maya puts something in her own space.
	res := h.member(http.MethodPost, "/api/v1/filedrop/upload?namespace=me&name=diary.txt",
		"maya", "filedrop_users", strings.NewReader("maya's"), "application/octet-stream")
	res.Body.Close()
	if res.StatusCode != http.StatusCreated {
		t.Fatalf("maya's upload: status %d", res.StatusCode)
	}
	if _, err := os.Stat(filepath.Join(root, "Members", "maya", "diary.txt")); err != nil {
		t.Fatalf("maya's file is not in her own space: %v", err)
	}

	// Sandy's own space is empty, and hers is a different place.
	var mine struct {
		Items []map[string]any `json:"items"`
	}
	res = h.member(http.MethodGet, "/api/v1/filedrop/list?namespace=me", "sandy", "filedrop_users", nil, "")
	if err := json.NewDecoder(res.Body).Decode(&mine); err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if len(mine.Items) != 0 {
		t.Errorf("sandy sees %d files in her own space: %v", len(mine.Items), mine.Items)
	}

	// And nothing sandy can put in the request reaches maya's.
	for _, ns := range []string{"me/maya", "Members/maya", "../Members/maya", "maya"} {
		res = h.member(http.MethodGet,
			"/api/v1/filedrop/list?namespace="+url.QueryEscape(ns), "sandy", "filedrop_users", nil, "")
		res.Body.Close()
		if res.StatusCode == http.StatusOK {
			t.Errorf("namespace %q answered 200 for sandy", ns)
		}
		res = h.member(http.MethodGet,
			"/api/v1/filedrop/file?namespace="+url.QueryEscape(ns)+"&name=diary.txt",
			"sandy", "filedrop_users", nil, "")
		res.Body.Close()
		if res.StatusCode == http.StatusOK {
			t.Errorf("sandy downloaded maya's diary through namespace %q", ns)
		}
		res = h.member(http.MethodPost,
			"/api/v1/filedrop/delete?namespace="+url.QueryEscape(ns)+"&name=diary.txt",
			"sandy", "filedrop_users", nil, "")
		res.Body.Close()
	}
	if _, err := os.Stat(filepath.Join(root, "Members", "maya", "diary.txt")); err != nil {
		t.Errorf("maya's file was removed by a refused request: %v", err)
	}

	// Maya reads her own back.
	res = h.member(http.MethodGet, "/api/v1/filedrop/file?namespace=me&name=diary.txt",
		"maya", "filedrop_users", nil, "")
	body, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if res.StatusCode != http.StatusOK || string(body) != "maya's" {
		t.Errorf("maya cannot read her own file: status %d body %q", res.StatusCode, body)
	}
}

// The personal destination is offered with what it actually promises,
// and withheld from a member whose name cannot become a directory.
func TestThePersonalDestinationIsOfferedHonestly(t *testing.T) {
	h, _ := portalHarness(t)

	var caps struct {
		Namespaces []map[string]string `json:"namespaces"`
	}
	res := h.member(http.MethodGet, "/api/v1/filedrop/capabilities", "sandy", "filedrop_users", nil, "")
	if err := json.NewDecoder(res.Body).Decode(&caps); err != nil {
		t.Fatal(err)
	}
	res.Body.Close()

	var me map[string]string
	for _, n := range caps.Namespaces {
		if n["key"] == "me" {
			me = n
		}
	}
	if me == nil {
		t.Fatal("no personal destination offered")
	}
	// It must not promise privacy it cannot keep against whoever runs
	// the machine.
	if !strings.Contains(me["note"], "machine") {
		t.Errorf("note = %q, want it to name the limit of the promise", me["note"])
	}

	// A member name this cannot turn into a directory is offered no
	// personal tab at all, rather than one that answers 404.
	res = h.member(http.MethodGet, "/api/v1/filedrop/capabilities", "not a name", "filedrop_users", nil, "")
	var odd struct {
		Namespaces []map[string]string `json:"namespaces"`
	}
	if err := json.NewDecoder(res.Body).Decode(&odd); err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	for _, n := range odd.Namespaces {
		if n["key"] == "me" {
			t.Error("a personal destination was offered to a member with no usable name")
		}
	}
}
