package api

// Epic #326 S02 at the API: the protected intake stores a Person's app
// password only on the placement nodes; only that Person may onboard,
// replace, inspect or authorize it; an Owner may revoke and erase and
// nothing more; revocation fences before it erases, erasure is proven by
// node evidence, and the value is absent from the database, responses,
// events, the audit chain and the log.

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/chickenrat17/wildstone-homelab/keystone/runtimedb"
	wsv1 "github.com/chickenrat17/wildstone-homelab/proto/gen/wardstone/v1"
	"github.com/chickenrat17/wildstone-homelab/proto/secreterase"
)

const (
	appPassword  = "REPLACED-app-password-4c1d"
	appPassword2 = "REPLACED-app-password-rotated-9e2b"
)

// credentialNodes is a fake node fleet with real write-only stores and
// real erase evidence.
type credentialNodes struct {
	mu         sync.Mutex
	store      map[string]map[string]string // node -> name -> value
	version    map[string]map[string]uint32
	containers map[string]map[string]bool
	putFails   map[string]bool
	badErase   map[string]bool
	// onErase runs before each erase, so a test can check the fence was
	// already in place when erasure began.
	onErase func(node, name string)
	ops     []string
}

func newCredentialNodes(nodes ...string) *credentialNodes {
	f := &credentialNodes{
		store: map[string]map[string]string{}, version: map[string]map[string]uint32{},
		containers: map[string]map[string]bool{}, putFails: map[string]bool{}, badErase: map[string]bool{},
	}
	for _, n := range nodes {
		f.store[n], f.version[n], f.containers[n] = map[string]string{}, map[string]uint32{}, map[string]bool{}
	}
	return f
}

func (f *credentialNodes) Apply(_ context.Context, node string, op *wsv1.Operation) (*wsv1.OpResult, error) {
	f.mu.Lock()
	switch o := op.GetOp().(type) {
	case *wsv1.Operation_PutSecret:
		f.ops = append(f.ops, node+" put "+o.PutSecret.GetName())
		if f.putFails[node] {
			f.mu.Unlock()
			return &wsv1.OpResult{Result: &wsv1.OpResult_Error{Error: &wsv1.OpError{Code: wsv1.ErrorCode_ERROR_CODE_RUNTIME, Message: "disk full"}}}, nil
		}
		f.store[node][o.PutSecret.GetName()] = o.PutSecret.GetValue()
		f.version[node][o.PutSecret.GetName()]++
		v := f.version[node][o.PutSecret.GetName()]
		f.mu.Unlock()
		return &wsv1.OpResult{Result: &wsv1.OpResult_PutSecret{PutSecret: &wsv1.PutSecretResult{Version: v}}}, nil
	case *wsv1.Operation_EraseSecret:
		name, nonce := o.EraseSecret.GetName(), o.EraseSecret.GetNonce()
		f.ops = append(f.ops, node+" erase "+name)
		hook := f.onErase
		f.mu.Unlock()
		if hook != nil {
			hook(node, name)
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		v, existed := f.version[node][name]
		delete(f.store[node], name)
		delete(f.version[node], name)
		ev := secreterase.Evidence(name, nonce, v, true)
		if f.badErase[node] {
			ev = []byte("forged")
		}
		return &wsv1.OpResult{Result: &wsv1.OpResult_EraseSecret{EraseSecret: &wsv1.EraseSecretResult{
			Existed: existed, ErasedVersion: v, Absent: true, Evidence: ev}}}, nil
	case *wsv1.Operation_StopContainer:
		f.mu.Unlock()
		return &wsv1.OpResult{Result: &wsv1.OpResult_StopContainer{StopContainer: &wsv1.StopContainerResult{}}}, nil
	case *wsv1.Operation_RemoveContainer:
		defer f.mu.Unlock()
		f.ops = append(f.ops, node+" remove "+o.RemoveContainer.GetName())
		delete(f.containers[node], o.RemoveContainer.GetName())
		return &wsv1.OpResult{Result: &wsv1.OpResult_RemoveContainer{RemoveContainer: &wsv1.RemoveContainerResult{}}}, nil
	case *wsv1.Operation_ListSecrets:
		defer f.mu.Unlock()
		res := &wsv1.ListSecretsResult{}
		for name, v := range f.version[node] {
			res.Secrets = append(res.Secrets, &wsv1.SecretInfo{Name: name, Version: v})
		}
		return &wsv1.OpResult{Result: &wsv1.OpResult_ListSecrets{ListSecrets: res}}, nil
	}
	f.mu.Unlock()
	return &wsv1.OpResult{Result: &wsv1.OpResult_Error{Error: &wsv1.OpError{Code: wsv1.ErrorCode_ERROR_CODE_INVALID, Message: "unexpected op"}}}, nil
}

func (f *credentialNodes) holds(node, name string) (string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	v, ok := f.store[node][name]
	return v, ok
}

// credentialHarness: a homepage deployment with sam (Member) owning
// sam-mail and olive (Owner) owning olive-mail, each placed on n1 and n2,
// bo another Member, and mika an Owner.
type credentialHarness struct {
	*harness
	nodes *credentialNodes
	logs  *bytes.Buffer
	mika string // Owner Console token acting for mika
	olive string // Owner Console token acting for olive
	agent int64
}

func newCredentialHarness(t *testing.T) *credentialHarness {
	t.Helper()
	h := homeHarness(t)
	logs := &bytes.Buffer{}
	h.s.Log = slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	nodes := newCredentialNodes("n1", "n2")
	h.s.Agents = nodes
	h.person("sam", "bo")
	c := &credentialHarness{harness: h, nodes: nodes, logs: logs,
		mika: h.console(runtimedb.RoleAdmin, "mika"), olive: h.console(runtimedb.RoleAdmin, "olive")}
	db := h.s.DB
	if err := db.UpsertModel(runtimedb.Model{Name: "kitchen-box", Kind: runtimedb.ModelLocal, Provider: "ollama",
		ProviderModel: "llama", Readiness: runtimedb.ModelReady, Enabled: true}, "root-admin"); err != nil {
		t.Fatal(err)
	}
	agent, err := db.CreateButler(runtimedb.Butler{Name: "hobson", Directive: "watch the post",
		Surface: runtimedb.ButlerSurfaceFamily, AgentClass: runtimedb.AgentClassMember, Model: "kitchen-box"}, "root-admin")
	if err != nil {
		t.Fatal(err)
	}
	c.agent = agent
	for _, m := range []string{"sam", "bo", "olive", "mika"} {
		if err := db.GrantButlerAccess("hobson", m, "root-admin"); err != nil {
			t.Fatal(err)
		}
	}
	for _, conn := range []struct{ name, owner string }{{"sam-mail", "sam"}, {"olive-mail", "olive"}} {
		if _, err := db.CreateConnection(runtimedb.IntegrationConnection{Name: conn.name, Adapter: "wild-mail-adapter",
			Ownership: runtimedb.OwnershipPersonal, Owner: conn.owner, CreatedBy: "root-admin"}); err != nil {
			t.Fatal(err)
		}
		prepare(t, h, conn.name, "n1", runtimedb.PlacementPrimary, 0, true)
		prepare(t, h, conn.name, "n2", runtimedb.PlacementFallback, 1, true)
		if err := db.SetConnectionExecutable(conn.name, "mail-roundcube@1.1.0/wild-mail-adapter", "root-admin"); err != nil {
			t.Fatal(err)
		}
		for _, n := range []string{"n1", "n2"} {
			nodes.containers[n]["mail-adapter-"+conn.name+"-"+n] = true
		}
	}
	return c
}

// consoleCall issues a Console request with a token and decodes the body.
func (c *credentialHarness) consoleCall(token, method, path string, body any, out any) (int, string) {
	c.t.Helper()
	var buf bytes.Buffer
	if body != nil {
		json.NewEncoder(&buf).Encode(body)
	}
	req, _ := http.NewRequest(method, c.srv.URL+path, &buf)
	req.Header.Set("Authorization", "Bearer "+token)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)
	if out != nil {
		json.Unmarshal(raw, out)
	}
	return res.StatusCode, string(raw)
}

// homeCall issues a Homepage request as a member.
func (c *credentialHarness) homeCall(user, method, path string, body any, out any) (int, string) {
	c.t.Helper()
	var r io.Reader = strings.NewReader("")
	if body != nil {
		raw, _ := json.Marshal(body)
		r = bytes.NewReader(raw)
	}
	res := c.member(method, path, user, "homepage_users", r, "application/json")
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)
	if out != nil {
		json.Unmarshal(raw, out)
	}
	return res.StatusCode, string(raw)
}

// dbBytes is everything the runtime database holds, as one file.
func (c *credentialHarness) dbBytes(t *testing.T) []byte {
	t.Helper()
	copyPath := filepath.Join(t.TempDir(), "copy.db")
	if _, err := c.s.DB.ExecForTest(`VACUUM INTO ?`, copyPath); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(copyPath)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func (c *credentialHarness) assertValueNowhere(t *testing.T, responses []string, values ...string) {
	t.Helper()
	db := c.dbBytes(t)
	for _, v := range values {
		if bytes.Contains(db, []byte(v)) {
			t.Errorf("the runtime database contains the credential value")
		}
		if strings.Contains(c.logs.String(), v) {
			t.Errorf("the log contains the credential value")
		}
		for _, r := range responses {
			if strings.Contains(r, v) {
				t.Errorf("an API response contains the credential value: %s", r)
			}
		}
	}
}

func TestCredentialIntakeStoresOnlyOnNodes(t *testing.T) {
	c := newCredentialHarness(t)
	var responses []string
	code, body := c.homeCall("sam", http.MethodPost, "/api/v1/home/integrations/sam-mail/credential",
		map[string]string{"login": "sam@example.internal", "password": appPassword}, nil)
	responses = append(responses, body)
	if code != http.StatusOK {
		t.Fatalf("intake = %d %s", code, body)
	}
	ref := "pc.sam-mail.v1"
	for _, n := range []string{"n1", "n2"} {
		if v, ok := c.nodes.holds(n, ref); !ok || v != appPassword {
			t.Fatalf("%s does not hold the credential", n)
		}
	}
	// The fingerprint key: generated by Keystone, the same on every
	// placement node, never returned.
	k1, ok1 := c.nodes.holds("n1", "pc.sam-mail.fk.v1")
	k2, ok2 := c.nodes.holds("n2", "pc.sam-mail.fk.v1")
	if !ok1 || !ok2 || k1 != k2 || len(k1) < 43 {
		t.Fatalf("fingerprint key not provisioned identically: %v %v", ok1, ok2)
	}
	_, view := c.homeCall("sam", http.MethodGet, "/api/v1/home/integrations/sam-mail/credential", nil, nil)
	responses = append(responses, view)
	if !strings.Contains(view, "sam@example.internal") || !strings.Contains(view, `"state":"active"`) {
		t.Fatalf("sam's own view = %s", view)
	}
	_, list := c.consoleCall(c.mika, http.MethodGet, "/api/v1/integrations", nil, nil)
	_, placements := c.consoleCall(c.mika, http.MethodGet, "/api/v1/integrations/sam-mail/placement", nil, nil)
	_, events := c.consoleCall(c.mika, http.MethodGet, "/api/v1/events", nil, nil)
	_, audit := c.consoleCall(c.mika, http.MethodGet, "/api/v1/audit", nil, nil)
	responses = append(responses, list, placements, events, audit)
	if strings.Contains(placements, "sam@example.internal") {
		t.Error("the administrator's placement listing shows sam's login")
	}
	c.assertValueNowhere(t, responses, appPassword, k1)
}

// The denial matrix: another Person and an Owner acting alone cannot
// inspect, replace, or authorize; an Owner may revoke.
func TestCredentialDenialMatrix(t *testing.T) {
	c := newCredentialHarness(t)
	if code, body := c.homeCall("sam", http.MethodPost, "/api/v1/home/integrations/sam-mail/credential",
		map[string]string{"login": "sam@example.internal", "password": appPassword}, nil); code != http.StatusOK {
		t.Fatalf("intake = %d %s", code, body)
	}
	if code, body := c.consoleCall(c.olive, http.MethodPost, "/api/v1/integrations/olive-mail/credential",
		map[string]string{"login": "olive@example.internal", "password": appPassword2}, nil); code != http.StatusOK {
		t.Fatalf("olive's own intake through the Console = %d %s", code, body)
	}
	intake := map[string]string{"login": "attacker@example.internal", "password": "REPLACED-attacker"}
	authorize := map[string]any{"agent": c.agent, "targetCalendar": "x", "dryRun": true}
	for _, tc := range []struct {
		who  string
		call func(method, path string, body any) (int, string)
		base string
	}{
		{"bo on the Homepage", func(m, p string, b any) (int, string) { return c.homeCall("bo", m, p, b, nil) }, "/api/v1/home/integrations/sam-mail"},
		{"an Owner on the Console", func(m, p string, b any) (int, string) { return c.consoleCall(c.mika, m, p, b, nil) }, "/api/v1/integrations/sam-mail"},
		{"sam reaching for olive's", func(m, p string, b any) (int, string) { return c.homeCall("sam", m, p, b, nil) }, "/api/v1/home/integrations/olive-mail"},
		{"the Butler", func(m, p string, b any) (int, string) { return c.do2(runtimedb.RoleButler, m, p, b) }, "/api/v1/integrations/sam-mail"},
	} {
		for _, op := range []struct {
			name, method, suffix string
			body                 any
		}{
			{"inspect", http.MethodGet, "/credential", nil},
			{"replace", http.MethodPost, "/credential", intake},
			{"authorize", http.MethodPost, "/authorization", authorize},
		} {
			if code, body := tc.call(op.method, tc.base+op.suffix, op.body); code != http.StatusForbidden {
				t.Errorf("%s may %s: %d %s", tc.who, op.name, code, body)
			}
		}
	}
	// Nothing an intruder sent reached a node.
	for _, n := range []string{"n1", "n2"} {
		for _, name := range []string{"pc.sam-mail.v2", "pc.olive-mail.v2"} {
			if _, ok := c.nodes.holds(n, name); ok {
				t.Fatalf("a refused intake stored %s on %s", name, n)
			}
		}
	}
	// bo cannot revoke sam's either; an Owner administering can.
	if code, _ := c.homeCall("bo", http.MethodPost, "/api/v1/home/integrations/sam-mail/credential/revoke", nil, nil); code != http.StatusForbidden {
		t.Fatalf("bo revoked sam's credential: %d", code)
	}
	if code, body := c.consoleCall(c.mika, http.MethodPost, "/api/v1/integrations/sam-mail/credential/revoke",
		map[string]string{"reason": "household decision"}, nil); code != http.StatusOK {
		t.Fatalf("an Owner could not withdraw: %d %s", code, body)
	}
}

// do2 is h.do returning the status and body.
func (c *credentialHarness) do2(role runtimedb.Role, method, path string, body any) (int, string) {
	return c.consoleCall(c.tokens[role], method, path, body, nil)
}

func TestReplaceRevokeFenceFirstThenProveErasure(t *testing.T) {
	c := newCredentialHarness(t)
	db := c.s.DB
	if code, body := c.homeCall("sam", http.MethodPost, "/api/v1/home/integrations/sam-mail/credential",
		map[string]string{"login": "sam@example.internal", "password": appPassword}, nil); code != http.StatusOK {
		t.Fatalf("intake = %d %s", code, body)
	}
	// Every erase must find the fence already in place: the credential
	// it erases is no longer active and the epoch has moved.
	before, _ := db.GetConnection("sam-mail")
	c.nodes.onErase = func(node, name string) {
		now, _ := db.GetConnection("sam-mail")
		if now.LeaseEpoch <= before.LeaseEpoch {
			t.Errorf("erasing %s on %s before the connection was fenced", name, node)
		}
		if active, err := db.ActivePersonalCredential("sam-mail", runtimedb.MailboxCredentialNeed); err == nil && active.SecretRef == name {
			t.Errorf("erasing the ACTIVE credential %s", name)
		}
	}

	// Replace: v2 lands everywhere, then v1 is fenced and erased, and the
	// adapters that were started with v1 are removed.
	var replaced struct {
		Erasures []erasureOutcome `json:"erasures"`
	}
	code, body := c.homeCall("sam", http.MethodPost, "/api/v1/home/integrations/sam-mail/credential",
		map[string]string{"login": "sam@example.internal", "password": appPassword2}, &replaced)
	if code != http.StatusOK {
		t.Fatalf("replace = %d %s", code, body)
	}
	if len(replaced.Erasures) != 2 {
		t.Fatalf("replace erasures = %+v", replaced.Erasures)
	}
	for _, e := range replaced.Erasures {
		if !e.Verified {
			t.Fatalf("erasure of v1 on %s unverified: %s", e.Node, e.Detail)
		}
	}
	for _, n := range []string{"n1", "n2"} {
		if _, ok := c.nodes.holds(n, "pc.sam-mail.v1"); ok {
			t.Fatalf("v1 still on %s", n)
		}
		if v, _ := c.nodes.holds(n, "pc.sam-mail.v2"); v != appPassword2 {
			t.Fatalf("v2 missing on %s", n)
		}
		if _, ok := c.nodes.holds(n, "pc.sam-mail.fk.v1"); !ok {
			t.Fatalf("replacing the password rotated the fingerprint key on %s", n)
		}
		if c.nodes.containers[n]["mail-adapter-sam-mail-"+n] {
			t.Fatalf("the adapter started with v1 still runs on %s", n)
		}
	}
	putV2, eraseV1 := -1, -1
	for i, op := range c.nodes.ops {
		if strings.HasSuffix(op, "put pc.sam-mail.v2") && putV2 < 0 {
			putV2 = i
		}
		if strings.HasSuffix(op, "erase pc.sam-mail.v1") && eraseV1 < 0 {
			eraseV1 = i
		}
	}
	if putV2 < 0 || eraseV1 < 0 || eraseV1 < putV2 {
		t.Fatalf("the replacement was not staged before the old one was erased: %v", c.nodes.ops)
	}

	// Revoke with n2 unable to prove its erase: access ends anyway, and
	// the connection says erasure_unverified until a retry proves it.
	before, _ = db.GetConnection("sam-mail")
	c.nodes.badErase["n2"] = true
	var revoked struct {
		State          string `json:"state"`
		DegradedReason string `json:"degradedReason"`
	}
	if code, body := c.homeCall("sam", http.MethodPost, "/api/v1/home/integrations/sam-mail/credential/revoke",
		map[string]string{"reason": "new phone"}, &revoked); code != http.StatusOK {
		t.Fatalf("revoke = %d %s", code, body)
	}
	if revoked.State != runtimedb.ConnectionDegraded || !strings.HasPrefix(revoked.DegradedReason, runtimedb.ErasureUnverifiedReason) ||
		!strings.Contains(revoked.DegradedReason, "n2") {
		t.Fatalf("after an unverifiable erase: %s %q", revoked.State, revoked.DegradedReason)
	}
	c.nodes.badErase["n2"] = false
	var retried struct {
		State string `json:"state"`
	}
	if code, body := c.consoleCall(c.mika, http.MethodPost, "/api/v1/integrations/sam-mail/credential/erase", nil, &retried); code != http.StatusOK {
		t.Fatalf("erase retry = %d %s", code, body)
	}
	if retried.State != runtimedb.ConnectionConfigured {
		t.Fatalf("after a proven retry the connection is %s", retried.State)
	}
	c.assertValueNowhere(t, nil, appPassword, appPassword2)
}

func TestFailedStagingLeavesTheWorkingCredential(t *testing.T) {
	c := newCredentialHarness(t)
	if code, _ := c.homeCall("sam", http.MethodPost, "/api/v1/home/integrations/sam-mail/credential",
		map[string]string{"login": "sam@example.internal", "password": appPassword}, nil); code != http.StatusOK {
		t.Fatal("intake failed")
	}
	c.nodes.putFails["n2"] = true
	code, body := c.homeCall("sam", http.MethodPost, "/api/v1/home/integrations/sam-mail/credential",
		map[string]string{"login": "sam@example.internal", "password": appPassword2}, nil)
	if code != http.StatusBadGateway {
		t.Fatalf("a partial staging = %d %s", code, body)
	}
	active, err := c.s.DB.ActivePersonalCredential("sam-mail", runtimedb.MailboxCredentialNeed)
	if err != nil || active.Version != 1 {
		t.Fatalf("the working credential changed: %+v %v", active, err)
	}
	if _, ok := c.nodes.holds("n1", "pc.sam-mail.v2"); ok {
		t.Fatal("the half-staged version was left on n1")
	}
	if v, _ := c.nodes.holds("n1", "pc.sam-mail.v1"); v != appPassword {
		t.Fatal("the working credential was erased by a failed replacement")
	}
	if !c.nodes.containers["n1"]["mail-adapter-sam-mail-n1"] {
		t.Fatal("a failed replacement took down the working adapter")
	}
	c.assertValueNowhere(t, []string{body}, appPassword, appPassword2)
}

func TestOldAgentCannotProveErasure(t *testing.T) {
	c := newCredentialHarness(t)
	c.s.LiveNodes = func() []LiveNode {
		return []LiveNode{{NodeID: "n1", ProtocolMinor: EraseSecretProtocolMinor}, {NodeID: "n2", ProtocolMinor: 21}}
	}
	if code, _ := c.homeCall("sam", http.MethodPost, "/api/v1/home/integrations/sam-mail/credential",
		map[string]string{"login": "sam@example.internal", "password": appPassword}, nil); code != http.StatusOK {
		t.Fatal("intake failed")
	}
	var out struct {
		Erasures       []erasureOutcome `json:"erasures"`
		DegradedReason string           `json:"degradedReason"`
	}
	c.homeCall("sam", http.MethodPost, "/api/v1/home/integrations/sam-mail/credential/revoke", nil, &out)
	for _, e := range out.Erasures {
		if e.Node == "n2" && (e.Verified || !strings.Contains(e.Detail, "minor 21")) {
			t.Fatalf("an agent below minor %d was trusted: %+v", EraseSecretProtocolMinor, e)
		}
	}
	if !strings.HasPrefix(out.DegradedReason, runtimedb.ErasureUnverifiedReason) {
		t.Fatalf("degraded reason = %q", out.DegradedReason)
	}
}

func TestOffboardingThroughTheAPIErasesOnNodes(t *testing.T) {
	c := newCredentialHarness(t)
	if code, _ := c.homeCall("sam", http.MethodPost, "/api/v1/home/integrations/sam-mail/credential",
		map[string]string{"login": "sam@example.internal", "password": appPassword}, nil); code != http.StatusOK {
		t.Fatal("intake failed")
	}
	var out struct {
		CredentialErasures map[string][]erasureOutcome `json:"credentialErasures"`
	}
	// Offboarding needs a fresh Owner Console session, not a token.
	session := c.s.StartConsoleSession("mika", time.Now())
	res := c.consoleRequest(http.MethodPost, "/api/v1/butler/members/sam/offboarding", "mika", sessionCookie(session))
	raw, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("offboarding = %d %s", res.StatusCode, raw)
	}
	json.Unmarshal(raw, &out)
	if len(out.CredentialErasures["sam-mail"]) != 4 {
		t.Fatalf("offboarding erasures = %+v", out.CredentialErasures)
	}
	for _, n := range []string{"n1", "n2"} {
		for _, name := range []string{"pc.sam-mail.v1", "pc.sam-mail.fk.v1"} {
			if _, ok := c.nodes.holds(n, name); ok {
				t.Fatalf("an offboarded Person's %s is still on %s", name, n)
			}
		}
	}
	conn, _ := c.s.DB.GetConnection("sam-mail")
	if conn.State != runtimedb.ConnectionUnavailable {
		t.Fatalf("offboarded connection = %s %q", conn.State, conn.DegradedReason)
	}
}

// A version a restored snapshot never heard of is still erased: the
// sweep asks the nodes rather than trusting the database.
func TestOrphanCredentialOnANodeIsErased(t *testing.T) {
	c := newCredentialHarness(t)
	c.nodes.store["n2"]["pc.sam-mail.v7"] = "REPLACED-orphan"
	c.nodes.version["n2"]["pc.sam-mail.v7"] = 1
	c.nodes.store["n2"]["pc.olive-mail.v1"] = "REPLACED-other-connection"
	c.nodes.version["n2"]["pc.olive-mail.v1"] = 1
	var out struct {
		Erasures []erasureOutcome `json:"erasures"`
	}
	if code, body := c.consoleCall(c.mika, http.MethodPost, "/api/v1/integrations/sam-mail/credential/erase", nil, &out); code != http.StatusOK {
		t.Fatalf("erase = %d %s", code, body)
	}
	if _, ok := c.nodes.holds("n2", "pc.sam-mail.v7"); ok {
		t.Fatal("the orphaned version survived the sweep")
	}
	if _, ok := c.nodes.holds("n2", "pc.olive-mail.v1"); !ok {
		t.Fatal("the sweep erased another connection's credential")
	}
}

func TestAuthorizationFlowThroughTheAPI(t *testing.T) {
	c := newCredentialHarness(t)
	if code, _ := c.homeCall("sam", http.MethodPost, "/api/v1/home/integrations/sam-mail/credential",
		map[string]string{"login": "sam@example.internal", "password": appPassword}, nil); code != http.StatusOK {
		t.Fatal("intake failed")
	}
	req := map[string]any{"agent": c.agent, "targetCalendar": "sam-family",
		"scope": map[string]any{"folders": []string{"INBOX"}, "senders": []string{"school.example.internal"}}}
	var preview struct {
		SubscriptionMatch map[string]string `json:"subscriptionMatch"`
	}
	if code, body := c.homeCall("sam", http.MethodPost, "/api/v1/home/integrations/sam-mail/authorization", req, &preview); code != http.StatusOK {
		t.Fatalf("preview = %d %s", code, body)
	}
	match, _ := json.Marshal(preview.SubscriptionMatch)
	sub, err := c.s.DB.InsertAgentSubscription(c.agent, "schedule", string(match), runtimedb.AudienceOwner, []string{"mail.changes"}, "")
	if err != nil {
		t.Fatal(err)
	}
	req["subscription"] = sub
	if code, body := c.consoleCall(c.mika, http.MethodPost, "/api/v1/integrations/sam-mail/authorization", req, nil); code != http.StatusForbidden {
		t.Fatalf("an Owner authorized sam's mailbox: %d %s", code, body)
	}
	if code, body := c.homeCall("sam", http.MethodPost, "/api/v1/home/integrations/sam-mail/authorization", req, nil); code != http.StatusOK {
		t.Fatalf("authorize = %d %s", code, body)
	}
	if _, err := c.s.DB.AuthorizeBackgroundUse(context.Background(), "sam-mail", sub, 1); err == nil {
		t.Fatal("background use before the Owner approved")
	}
	if err := c.s.DB.ApproveAgentSubscription(sub, "mika"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.s.DB.AuthorizeBackgroundUse(context.Background(), "sam-mail", sub, 1); err != nil {
		t.Fatalf("both halves hold: %v", err)
	}
	if code, body := c.homeCall("sam", http.MethodPost, "/api/v1/home/integrations/sam-mail/authorization/revoke", nil, nil); code != http.StatusOK {
		t.Fatalf("revoke = %d %s", code, body)
	}
	if _, err := c.s.DB.AuthorizeBackgroundUse(context.Background(), "sam-mail", sub, 1); err == nil {
		t.Fatal("background use after the Person revoked")
	}
}
