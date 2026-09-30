package api

// Epic #326 S01: the production mail adapter runtime — adapter.Runner
// driving mail.Upstream, exactly as keystone/cmd/wild-mail-adapter wires
// them — against a real Keystone over HTTP. A test Personal connection
// with a primary and a prepared fallback proves the runner claims only
// its own connection's work, only while its node holds the lease, that
// it survives failover and failback, and that a newer lease epoch fences
// a result already in flight. The dispatchable surface is read-only at
// the queue as well as in the adapter.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/chickenrat17/wildstone-homelab/keystone/adapter"
	"github.com/chickenrat17/wildstone-homelab/keystone/adapter/mail"
	"github.com/chickenrat17/wildstone-homelab/keystone/catalog"
	"github.com/chickenrat17/wildstone-homelab/keystone/runtimedb"
)

// countingReader is a fake mailbox: it answers, counts, and can be held
// mid-answer so a lease can move underneath it.
type countingReader struct {
	mu      sync.Mutex
	reads   int
	hold    chan struct{} // when set, Changes waits for it to close
	entered chan struct{}
}

func (c *countingReader) read() {
	c.mu.Lock()
	c.reads++
	c.mu.Unlock()
}

func (c *countingReader) Probe(context.Context) (mail.Probe, error) {
	c.read()
	return mail.Probe{Folders: []mail.FolderStatus{{Folder: "INBOX", Available: true}}}, nil
}

func (c *countingReader) Changes(ctx context.Context, _ mail.ChangesRequest) (adapter.Answer, error) {
	c.read()
	if c.hold != nil {
		close(c.entered)
		select {
		case <-c.hold:
		case <-ctx.Done():
			return adapter.Answer{}, ctx.Err()
		}
	}
	return adapter.Answer{Data: map[string]any{"messages": []any{map[string]any{"ref": "r-1"}}}}, nil
}

func (c *countingReader) Message(context.Context, mail.MessageRequest) (adapter.Answer, error) {
	c.read()
	return adapter.Answer{Data: map[string]any{"ref": "r-1"}}, nil
}

func (c *countingReader) Search(context.Context, mail.SearchRequest) (adapter.Answer, error) {
	c.read()
	return adapter.Answer{Empty: true, Data: map[string]any{"messages": []any{}}}, nil
}

func (c *countingReader) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.reads
}

// mailRunner is one placed adapter: its own principal, bound to one
// connection on one node.
type mailRunner struct {
	node   string
	reader *countingReader
	runner *adapter.Runner
}

func placeMailAdapter(t *testing.T, h *harness, connection, node string) *mailRunner {
	t.Helper()
	name := "mail-adapter-" + connection + "-" + node
	if err := h.s.Principals.Create(name, runtimedb.RoleIntegrationAdapter, ""); err != nil {
		t.Fatal(err)
	}
	tok, err := h.s.Principals.Mint(name, "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if res := h.do(runtimedb.RoleAdmin, http.MethodPost, "/api/v1/integrations/"+connection+"/adapter",
		map[string]any{"principal": name, "node": node}, nil); res.StatusCode != http.StatusOK {
		t.Fatalf("binding %s on %s = %d", name, node, res.StatusCode)
	}
	reader := &countingReader{}
	return &mailRunner{node: node, reader: reader, runner: &adapter.Runner{
		Client:   &adapter.Client{BaseURL: h.srv.URL, Token: tok},
		Upstream: &mail.Upstream{Settings: mail.Settings{Connection: connection, Node: node}, Reader: reader},
		PollWait: 50 * time.Millisecond,
	}}
}

func personalMailbox(t *testing.T, h *harness, name, owner string) {
	t.Helper()
	if res := h.do(runtimedb.RoleAdmin, http.MethodPost, "/api/v1/integrations", map[string]any{
		"name": name, "adapter": "wild-mail-adapter", "ownership": "personal", "owner": owner,
	}, nil); res.StatusCode != http.StatusOK {
		t.Fatalf("creating %s = %d", name, res.StatusCode)
	}
}

func prepare(t *testing.T, h *harness, connection, node, role string, rank int, ready bool) {
	t.Helper()
	db := h.s.DB
	if err := db.AddPlacement(connection, node, role, rank, "root-admin"); err != nil && !strings.Contains(err.Error(), "already") {
		t.Fatal(err)
	}
	if err := db.MarkPlacementPrepared(connection, node, ready, ready, ready); err != nil {
		t.Fatal(err)
	}
}

func queueMail(t *testing.T, h *harness, connection, kind, capability string) (int64, int) {
	t.Helper()
	var out struct {
		Seq int64 `json:"seq"`
	}
	res := h.do(runtimedb.RoleAdmin, http.MethodPost, "/api/v1/integrations/"+connection+"/work",
		map[string]any{"kind": kind, "capability": capability, "request": `{}`, "timeoutMillis": 30000}, &out)
	return out.Seq, res.StatusCode
}

func (m *mailRunner) once(t *testing.T) bool {
	t.Helper()
	did, err := m.runner.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("%s RunOnce: %v", m.node, err)
	}
	return did
}

func TestMailAdapterRunnerPlacementFailoverAndFencing(t *testing.T) {
	h := newHarness(t)
	ownExecutionFixture(t, h)
	db := h.s.DB
	personalMailbox(t, h, "ada-mailbox", "ada")
	personalMailbox(t, h, "bo-mailbox", "bo")
	prepare(t, h, "ada-mailbox", "node-a", runtimedb.PlacementPrimary, 0, true)
	prepare(t, h, "ada-mailbox", "node-b", runtimedb.PlacementFallback, 1, true)
	prepare(t, h, "bo-mailbox", "node-a", runtimedb.PlacementPrimary, 0, true)
	for _, c := range []string{"ada-mailbox", "bo-mailbox"} {
		if _, _, err := db.PlaceConnection(c, time.Hour); err != nil {
			t.Fatal(err)
		}
	}
	primary := placeMailAdapter(t, h, "ada-mailbox", "node-a")
	fallback := placeMailAdapter(t, h, "ada-mailbox", "node-b")
	other := placeMailAdapter(t, h, "bo-mailbox", "node-a")

	t.Run("only the lease holder claims, and only its own connection", func(t *testing.T) {
		boSeq, _ := queueMail(t, h, "bo-mailbox", "query", mail.CapChanges)
		adaSeq, code := queueMail(t, h, "ada-mailbox", "query", mail.CapChanges)
		if code != http.StatusOK {
			t.Fatalf("queue = %d", code)
		}
		// The fallback is prepared and running, and bound to this very
		// connection — and it gets nothing while the primary holds.
		if fallback.once(t) {
			t.Fatal("the fallback claimed work while the primary holds the lease")
		}
		if !primary.once(t) {
			t.Fatal("the primary claimed nothing")
		}
		if got := readWork(t, h, adaSeq); got.ResultType != "ok" || got.ClaimedBy != "mail-adapter-ada-mailbox-node-a" {
			t.Fatalf("ada's work = %+v", got)
		}
		// Bo's work waits for Bo's adapter: Ada's never saw it.
		if primary.once(t) {
			t.Fatal("ada's adapter claimed another Person's connection's work")
		}
		if got := readWork(t, h, boSeq); got.State != runtimedb.WorkQueued {
			t.Fatalf("bo's work = %+v", got)
		}
		if !other.once(t) {
			t.Fatal("bo's adapter claimed nothing")
		}
		if fallback.reader.count() != 0 {
			t.Fatal("the inactive fallback read the mailbox")
		}
	})

	t.Run("failover moves the lease, and failback waits to stabilize", func(t *testing.T) {
		before, _ := db.GetConnection("ada-mailbox")
		// The primary's container stopped.
		prepare(t, h, "ada-mailbox", "node-a", runtimedb.PlacementPrimary, 0, false)
		if _, moved, err := db.PlaceConnection("ada-mailbox", time.Hour); err != nil || !moved {
			t.Fatalf("PlaceConnection = moved %v, %v", moved, err)
		}
		after, _ := db.GetConnection("ada-mailbox")
		if after.LeaseEpoch <= before.LeaseEpoch {
			t.Fatalf("epoch %d -> %d; failover must fence", before.LeaseEpoch, after.LeaseEpoch)
		}
		seq, _ := queueMail(t, h, "ada-mailbox", "query", mail.CapSearch)
		if primary.once(t) {
			t.Fatal("the old primary claimed after losing the lease")
		}
		if !fallback.once(t) {
			t.Fatal("the fallback claimed nothing after failover")
		}
		if got := readWork(t, h, seq); got.ResultType != "empty" || got.ClaimedBy != "mail-adapter-ada-mailbox-node-b" {
			t.Fatalf("work after failover = %+v", got)
		}
		// The primary comes back, but has not been healthy for the
		// stabilization window: the lease stays on the fallback.
		prepare(t, h, "ada-mailbox", "node-a", runtimedb.PlacementPrimary, 0, true)
		if p, moved, err := db.PlaceConnection("ada-mailbox", time.Hour); err != nil || moved || p.Node != "node-b" {
			t.Fatalf("failback = %s moved %v, %v; want to stay on node-b", p.Node, moved, err)
		}
	})

	t.Run("a newer lease epoch fences a result already in flight", func(t *testing.T) {
		fallback.reader.hold = make(chan struct{})
		fallback.reader.entered = make(chan struct{})
		seq, _ := queueMail(t, h, "ada-mailbox", "query", mail.CapChanges)
		done := make(chan error, 1)
		go func() {
			did, err := fallback.runner.RunOnce(context.Background())
			if err == nil && !did {
				err = errors.New("claimed nothing")
			}
			done <- err
		}()
		select {
		case <-fallback.reader.entered:
		case err := <-done:
			t.Fatalf("the holder never reached the mailbox: %v", err)
		case <-time.After(10 * time.Second):
			t.Fatal("timed out waiting for the holder to claim")
		}
		// While the adapter is mid-answer, the lease moves on.
		if _, err := db.BumpConnectionEpoch("ada-mailbox"); err != nil {
			t.Fatal(err)
		}
		close(fallback.reader.hold)
		if err := <-done; err != nil {
			t.Fatalf("a fenced result must be dropped quietly, not fail the runner: %v", err)
		}
		fallback.reader.hold = nil
		got := readWork(t, h, seq)
		if got.State != runtimedb.WorkClaimed || got.ResultType != "" || (got.ResultJSON != "" && got.ResultJSON != "{}") {
			t.Fatalf("fenced work = %+v; the superseded answer must not land", got)
		}
	})

	t.Run("the queue refuses a mutating mail operation", func(t *testing.T) {
		for _, tc := range []struct{ kind, name string }{
			{"action", "mail.send"}, {"action", "mail.move"}, {"query", "mail.delete"},
			{"query", "mail.mark_read"}, {"action", "mail.changes"},
		} {
			if _, code := queueMail(t, h, "ada-mailbox", tc.kind, tc.name); code != http.StatusBadRequest {
				t.Errorf("queueing %s %s = %d, want 400", tc.kind, tc.name, code)
			}
		}
		// Even widened, a Personal mailbox has no action to dispatch.
		if err := db.WidenConnection("ada-mailbox", true, false, false); err != nil {
			t.Fatal(err)
		}
		if _, code := queueMail(t, h, "ada-mailbox", "action", "mail.send"); code != http.StatusBadRequest {
			t.Errorf("widened mail.send = %d, want 400", code)
		}
	})
}

// Installing the catalog, and inspecting it, creates nothing: no
// connection, no credential reference, no work — and the offered
// surface is visibly the four read queries.
func TestMailPackageIsInspectableAndDormant(t *testing.T) {
	h := newHarness(t)
	c, err := catalog.LoadDir("../../packages")
	if err != nil {
		t.Fatal(err)
	}
	h.s.Catalog = c
	var pkgs []map[string]json.RawMessage
	if res := h.do(runtimedb.RoleAdmin, http.MethodGet, "/api/v1/packages", nil, &pkgs); res.StatusCode != http.StatusOK {
		t.Fatalf("packages = %d", res.StatusCode)
	}
	found := false
	for _, p := range pkgs {
		var name string
		_ = json.Unmarshal(p["name"], &name)
		if name != "mail-roundcube" {
			continue
		}
		found = true
		var integrates []struct {
			Domain  string `json:"domain"`
			Adapter struct {
				Program string `json:"program"`
			} `json:"adapter"`
			Answers []struct {
				Query string `json:"query"`
			} `json:"answers"`
		}
		if err := json.Unmarshal(p["integrates"], &integrates); err != nil || len(integrates) != 1 {
			t.Fatalf("integrates = %s (%v)", p["integrates"], err)
		}
		var offered []string
		for _, a := range integrates[0].Answers {
			offered = append(offered, a.Query)
		}
		if strings.Join(offered, ",") != strings.Join(mail.Operations, ",") {
			t.Fatalf("offered %v, dispatchable %v", offered, mail.Operations)
		}
	}
	if !found {
		t.Fatal("mail-roundcube is not in the catalog")
	}
	conns, err := h.s.DB.ListConnections()
	if err != nil || len(conns) != 0 {
		t.Fatalf("connections after loading the catalog = %+v, %v", conns, err)
	}
}
