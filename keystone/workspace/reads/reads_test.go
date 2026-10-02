package reads

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/chickenrat17/wildstone-homelab/keystone/butler"
	"github.com/chickenrat17/wildstone-homelab/keystone/internal/dbtemplate"
	"github.com/chickenrat17/wildstone-homelab/keystone/operation"
	"github.com/chickenrat17/wildstone-homelab/keystone/redact"
	"github.com/chickenrat17/wildstone-homelab/keystone/runtimedb"
	"github.com/chickenrat17/wildstone-homelab/keystone/workspace"
	"github.com/chickenrat17/wildstone-homelab/keystone/workspace/storage"
)

var migrated = dbtemplate.New(func(path string) error {
	db, err := runtimedb.Open(path)
	if err != nil {
		return err
	}
	return db.Close()
})

type readFixtureAuthority struct{}

func (readFixtureAuthority) AdmitPerson(butler.ToolAuthorityQuery) (butler.ToolDecision, error) {
	return butler.ToolDecision{Allowed: true}, nil
}
func (readFixtureAuthority) AdmitResource(q butler.ToolAuthorityQuery) (butler.ToolDecision, error) {
	return butler.ToolDecision{Allowed: q.Resource.Kind == "workspace"}, nil
}
func (readFixtureAuthority) AuthorizeTool(butler.ToolAuthorityQuery) (butler.ToolDecision, error) {
	return butler.ToolDecision{Allowed: true}, nil
}

func TestWorkspaceGatewaySelectorsRoundTripAndContentRedacts(t *testing.T) {
	f := newFixture(t)
	secret := strings.Repeat("a", 64)
	_, revision := f.publish(t, 1, "report.txt", secret+" "+strings.Repeat("ordinary fixture text ", 300), "normal")
	task, ctx := f.turn(t, runtimedb.ModelLocal)
	gw := butler.NewToolGateway(nil, nil)
	gw.WorkspaceReads = f.service.Call
	gw.SetAuthorizer(readFixtureAuthority{})
	gw.SetCaller(butler.CallerPersonFacing, "fixture-person", "console")
	gw.SetSessionTask(task)
	decode := func(tool string, args map[string]any, dest any) {
		t.Helper()
		out, err := gw.Execute(ctx, tool, args, true)
		must(t, err)
		if out.Failed || out.Rejected || len(out.Content) > 4000 {
			t.Fatalf("gateway refused: %+v", out)
		}
		if strings.Contains(out.Content, secret) {
			t.Fatal("secret-shaped content survived central redaction")
		}
		inner := strings.TrimSuffix(strings.TrimPrefix(out.Content, redact.UntrustedOpenTag+"\n"), "\n"+redact.UntrustedCloseTag)
		var envelope struct {
			Data json.RawMessage `json:"data"`
		}
		must(t, json.Unmarshal([]byte(inner), &envelope))
		must(t, json.Unmarshal(envelope.Data, dest))
	}
	var listing struct {
		Items []entry `json:"items"`
	}
	decode("workspace_list", map[string]any{"scope": "private"}, &listing)
	if len(listing.Items) != 1 || listing.Items[0].File.Revision != revision {
		t.Fatal("revision selector changed through redaction")
	}
	file := listing.Items[0].File
	var first struct {
		File fileInfo `json:"file"`
		Page textPage `json:"page"`
	}
	decode("workspace_read", map[string]any{"artifactId": file.ArtifactID, "revision": file.Revision}, &first)
	if !strings.Contains(first.Page.Text, redact.Placeholder) || first.Page.NextCursor == "" {
		t.Fatal("redaction or cursor missing")
	}
	var next struct {
		Page textPage `json:"page"`
	}
	decode("workspace_read", map[string]any{"artifactId": file.ArtifactID, "revision": file.Revision, "cursor": first.Page.NextCursor}, &next)
	if next.Page.Text == "" || strings.Contains(next.Page.Text, redact.Placeholder) {
		t.Fatal("returned cursor did not advance")
	}
}

type fixture struct {
	db        *runtimedb.DB
	service   *Service
	w         runtimedb.Workspace
	writer    string
	bodies    map[string][]byte
	byteReads int
	settings  workspace.Settings
	clock     time.Time
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	db, err := runtimedb.Open(migrated.Path(t))
	must(t, err)
	t.Cleanup(func() { db.Close() })
	must(t, db.RecordButlerMember("fixture-person", runtimedb.RoleOwner, "", ""))
	must(t, db.AdmitButlerMember("fixture-person", "root-admin"))
	must(t, db.UpsertModel(runtimedb.Model{Name: "local", Kind: runtimedb.ModelLocal, Provider: "ollama", ProviderModel: "fixture", Readiness: runtimedb.ModelReady, Enabled: true}, "root-admin"))
	agent, err := db.CreateButler(runtimedb.Butler{Name: "fixture-agent", Directive: "fixture", Surface: runtimedb.ButlerSurfaceAdmin, AgentClass: runtimedb.AgentClassOwner, Model: "local"}, "root-admin")
	must(t, err)
	must(t, db.GrantButlerAccess("fixture-agent", "fixture-person", "root-admin"))
	person, err := db.GetButlerMember("fixture-person")
	must(t, err)
	f := &fixture{db: db, w: runtimedb.Workspace{ID: "fixture-workspace", PersonID: person.PersonID, AgentID: agent}, bodies: map[string][]byte{}, clock: time.Now(), settings: workspace.Settings{Root: t.TempDir(), QuotaBytes: 1 << 30, MaxFileBytes: 10 << 20, ScratchRetentionDays: 7, TrashRetentionDays: 30, Revision: "config-1"}}
	must(t, db.CreateWorkspace(f.w))
	f.writer, _ = f.turn(t, runtimedb.ModelLocal)
	f.service = &Service{DB: db, Settings: func() (workspace.Settings, error) { return f.settings, nil }, now: func() time.Time { return f.clock }, read: func(_ *storage.Store, obj storage.Object) ([]byte, error) {
		f.byteReads++
		data, ok := f.bodies[obj.ID]
		if !ok {
			return nil, storage.ErrIntegrity
		}
		return data, nil
	}}
	return f
}
func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
func (f *fixture) turn(t *testing.T, model string) (string, context.Context) {
	t.Helper()
	id, err := f.db.InsertButlerTask(runtimedb.ButlerTask{Kind: runtimedb.ButlerTaskConversation, Author: "fixture-person", Owner: "fixture-person", Surface: runtimedb.ButlerSurfaceAdmin, AgentClass: runtimedb.AgentClassOwner, Audience: runtimedb.ButlerAudiencePrivate, OriginConnection: "console", Objective: "fixture read"})
	must(t, err)
	must(t, f.db.SetTaskButler(id, "fixture-agent"))
	ctx, p, err := f.db.BeginPrivacyTurn(context.Background(), model, runtimedb.MemoryDestination{BuiltIn: true}, "")
	must(t, err)
	must(t, p.SetTask(id))
	return id, operation.With(ctx, operation.Identity{TaskID: id})
}
func (f *fixture) publish(t *testing.T, index int, name, body, privacy string) (string, string) {
	t.Helper()
	artifact := fmt.Sprintf("artifact-%d", index)
	revision := fmt.Sprintf("%032x", index)
	must(t, f.db.CreateWorkspaceArtifact(runtimedb.WorkspaceArtifact{ID: artifact, WorkspaceID: f.w.ID, Path: name}))
	task, err := f.db.GetButlerTask(f.writer)
	must(t, err)
	hash := sha256.Sum256([]byte(body))
	must(t, f.db.CreateWorkspaceRevision(runtimedb.WorkspaceRevision{ID: revision, ArtifactID: artifact, TaskID: task.Seq, Hash: fmt.Sprintf("%x", hash), SizeBytes: int64(len(body)), MediaType: "text/plain", Privacy: privacy}))
	reservation := "reserve-" + revision
	publication := "publish-" + revision
	must(t, f.db.ReserveWorkspaceBytes(runtimedb.WorkspaceReservation{ID: reservation, WorkspaceID: f.w.ID, TaskID: task.Seq, SizeBytes: int64(len(body)), Purpose: "publication"}))
	must(t, f.db.CreateWorkspacePublication(runtimedb.WorkspacePublication{ID: publication, WorkspaceID: f.w.ID, ArtifactID: artifact, RevisionID: revision, TaskID: task.Seq, ReservationID: reservation, EffectKey: revision}))
	must(t, f.db.PrepareWorkspacePublication(f.w.ID, publication))
	must(t, f.db.MarkWorkspaceBytesPublished(f.w.ID, publication))
	must(t, f.db.CommitWorkspacePublication(f.w.ID, publication, "receipt-"+revision))
	f.bodies[revision] = []byte(body)
	return artifact, revision
}
func decodeData(t *testing.T, out butler.ToolOutcome, dest any) {
	t.Helper()
	if out.Failed || out.Rejected {
		t.Fatalf("unexpected refusal: %+v", out)
	}
	if len(redact.Envelope(out.Content)) > 4000 {
		t.Fatalf("complete result exceeds bound: %d", len(redact.Envelope(out.Content)))
	}
	var envelope struct {
		Version int             `json:"version"`
		Data    json.RawMessage `json:"data"`
	}
	must(t, json.Unmarshal([]byte(out.Content), &envelope))
	if envelope.Version != 1 {
		t.Fatal("wrong envelope version")
	}
	must(t, json.Unmarshal(envelope.Data, dest))
}
func TestReadPagesPreserveUTF8AndCompleteJSON(t *testing.T) {
	for _, body := range []string{strings.Repeat("x", 6000), strings.Repeat("世\"\n", 1800)} {
		f := newFixture(t)
		artifact, revision := f.publish(t, 1, "report.txt", body, "normal")
		_, ctx := f.turn(t, runtimedb.ModelLocal)
		call := butler.WorkspaceReadCall{ArtifactID: artifact, Revision: revision}
		got := ""
		for rounds := 0; ; rounds++ {
			if rounds > 30 {
				t.Fatal("cursor did not progress")
			}
			var data struct {
				File fileInfo `json:"file"`
				Page textPage `json:"page"`
			}
			decodeData(t, f.service.Call(ctx, "workspace_read", call), &data)
			if len(data.Page.Text) > 2500 || !utf8.ValidString(data.Page.Text) {
				t.Fatal("invalid text page")
			}
			if rounds == 0 && body[0] == 'x' && len(data.Page.Text) != 2500 {
				t.Fatal("default page target changed")
			}
			got += data.Page.Text
			if data.Page.NextCursor == "" {
				break
			}
			call.Cursor = data.Page.NextCursor
		}
		if got != body {
			t.Fatal("paging lost or duplicated bytes")
		}
	}
}
func TestReadCursorsRefuseTamperingScopeAndRevocation(t *testing.T) {
	f := newFixture(t)
	artifact, revision := f.publish(t, 1, "report.txt", strings.Repeat("x", 5000), "normal")
	_, ctx := f.turn(t, runtimedb.ModelLocal)
	call := butler.WorkspaceReadCall{ArtifactID: artifact, Revision: revision}
	var first struct {
		Page textPage `json:"page"`
	}
	decodeData(t, f.service.Call(ctx, "workspace_read", call), &first)
	call.Cursor = first.Page.NextCursor
	_, other := f.turn(t, runtimedb.ModelLocal)
	start := f.clock
	for _, scenario := range []string{"tampered", "other task", "wrong revision", "missing provenance", "expired", "config changed", "revoked"} {
		t.Run(scenario, func(t *testing.T) {
			f.clock, f.settings.Revision = start, "config-1"
			c := call
			requestContext := ctx
			reads := f.byteReads
			switch scenario {
			case "tampered":
				c.Cursor += "a"
			case "other task":
				requestContext = other
			case "wrong revision":
				c.Revision = "other-revision"
			case "missing provenance":
				requestContext = operation.With(context.Background(), operation.Identity{TaskID: operation.From(ctx).TaskID})
			case "expired":
				f.clock = f.clock.Add(16 * time.Minute)
			case "config changed":
				f.settings.Revision = "config-2"
			case "revoked":
				must(t, f.db.RevokeButlerAccess("fixture-agent", "fixture-person", "root-admin"))
			}
			out := f.service.Call(requestContext, "workspace_read", c)
			if !out.Failed && !out.Rejected {
				t.Fatalf("cursor bypass: %+v", out)
			}
			if f.byteReads != reads {
				t.Fatal("refusal loaded bytes")
			}
		})
	}
}
func TestListSearchAndDirectorySnapshots(t *testing.T) {
	f := newFixture(t)
	for i := 1; i <= 25; i++ {
		f.publish(t, i, fmt.Sprintf("docs/report-%02d.txt", i), "matching synthetic text", "normal")
	}
	_, ctx := f.turn(t, runtimedb.ModelLocal)
	var listing struct {
		Items      []entry `json:"items"`
		NextCursor string  `json:"nextCursor"`
	}
	call := butler.WorkspaceReadCall{Scope: "private"}
	decodeData(t, f.service.Call(ctx, "workspace_list", call), &listing)
	if len(listing.Items) != 1 || listing.Items[0].Directory == nil || !strings.HasPrefix(listing.Items[0].Directory.Revision, "read-") {
		t.Fatalf("invalid directory snapshot: %+v", listing)
	}
	firstToken := listing.Items[0].Directory.Revision
	call.Cursor = listing.NextCursor
	decodeData(t, f.service.Call(ctx, "workspace_list", call), &listing)
	if len(listing.Items) != 0 {
		t.Fatal("directory repeated across pages")
	}
	search := butler.WorkspaceReadCall{Scope: "private", Path: "docs", Query: "matching"}
	count := 0
	for pages := 0; ; pages++ {
		if pages > 30 {
			t.Fatal("scan did not progress")
		}
		var data struct {
			Items      []hit  `json:"items"`
			NextCursor string `json:"nextCursor"`
		}
		decodeData(t, f.service.Call(ctx, "workspace_search", search), &data)
		count += len(data.Items)
		if data.NextCursor == "" {
			break
		}
		search.Cursor = data.NextCursor
	}
	if count != 25 {
		t.Fatalf("search returned %d files", count)
	}
	f.publish(t, 26, "docs/new.txt", "matching", "normal")
	stale := f.service.Call(ctx, "workspace_list", call)
	if !stale.Failed || !strings.Contains(stale.Content, "stale_cursor") {
		t.Fatalf("namespace mutation accepted cursor: %+v", stale)
	}
	decodeData(t, f.service.Call(ctx, "workspace_list", butler.WorkspaceReadCall{Scope: "private"}), &listing)
	if listing.Items[0].Directory.Revision == firstToken {
		t.Fatal("directory snapshot did not change")
	}
	for _, bad := range []butler.WorkspaceReadCall{{Scope: "shared"}, {Scope: "private", Path: "../escape"}} {
		out := f.service.Call(ctx, "workspace_list", bad)
		if !out.Failed && !out.Rejected {
			t.Fatal("invalid scope/path accepted")
		}
	}
}
func TestProtectedMetadataAndBytesFailClosed(t *testing.T) {
	f := newFixture(t)
	artifact, revision := f.publish(t, 1, "protected.txt", "protected synthetic text", "local-only")
	_, ctx := f.turn(t, runtimedb.ModelCloud)
	for tool, call := range map[string]butler.WorkspaceReadCall{"workspace_list": {Scope: "private"}, "workspace_read": {ArtifactID: artifact, Revision: revision}, "workspace_search": {Scope: "private", Query: "protected"}} {
		out := f.service.Call(ctx, tool, call)
		if !out.Rejected || strings.Contains(out.Content, "protected.txt") || f.byteReads != 0 {
			t.Fatalf("protected metadata leaked: %+v reads=%d", out, f.byteReads)
		}
	}
}

func TestForeignPersonAndRevocationDuringByteRead(t *testing.T) {
	f := newFixture(t)
	artifact, revision := f.publish(t, 1, "private.txt", "private synthetic body", "normal")
	must(t, f.db.RecordButlerMember("other-person", runtimedb.RoleOwner, "", ""))
	must(t, f.db.AdmitButlerMember("other-person", "root-admin"))
	must(t, f.db.GrantButlerAccess("fixture-agent", "other-person", "root-admin"))
	other, err := f.db.GetButlerMember("other-person")
	must(t, err)
	must(t, f.db.CreateWorkspace(runtimedb.Workspace{ID: "other-workspace", PersonID: other.PersonID, AgentID: f.w.AgentID}))
	task, err := f.db.InsertButlerTask(runtimedb.ButlerTask{Kind: runtimedb.ButlerTaskConversation, Author: "other-person", Owner: "other-person", Surface: runtimedb.ButlerSurfaceAdmin, AgentClass: runtimedb.AgentClassOwner, Audience: runtimedb.ButlerAudiencePrivate, OriginConnection: "console", Objective: "fixture read"})
	must(t, err)
	must(t, f.db.SetTaskButler(task, "fixture-agent"))
	ctx, p, err := f.db.BeginPrivacyTurn(context.Background(), runtimedb.ModelLocal, runtimedb.MemoryDestination{BuiltIn: true}, "")
	must(t, err)
	must(t, p.SetTask(task))
	ctx = operation.With(ctx, operation.Identity{TaskID: task})
	call := butler.WorkspaceReadCall{ArtifactID: artifact, Revision: revision}
	out := f.service.Call(ctx, "workspace_read", call)
	if !out.Rejected || f.byteReads != 0 || strings.Contains(out.Content, "private.txt") {
		t.Fatalf("foreign Owner read leaked: %+v", out)
	}
	_, own := f.turn(t, runtimedb.ModelLocal)
	f.service.read = func(_ *storage.Store, obj storage.Object) ([]byte, error) {
		f.byteReads++
		must(t, f.db.RevokeButlerAccess("fixture-agent", "fixture-person", "root-admin"))
		return f.bodies[obj.ID], nil
	}
	out = f.service.Call(own, "workspace_read", call)
	if !out.Rejected || strings.Contains(out.Content, "private synthetic body") {
		t.Fatalf("authority changed during I/O: %+v", out)
	}
}

func TestStorageFaultAndDirectoryRestartFailClosed(t *testing.T) {
	f := newFixture(t)
	artifact, revision := f.publish(t, 1, "docs/private.txt", "synthetic text", "normal")
	_, ctx := f.turn(t, runtimedb.ModelLocal)
	f.service.read = func(*storage.Store, storage.Object) ([]byte, error) { return nil, storage.ErrIntegrity }
	out := f.service.Call(ctx, "workspace_read", butler.WorkspaceReadCall{ArtifactID: artifact, Revision: revision})
	if !out.Failed || strings.Contains(out.Content, "private.txt") {
		t.Fatalf("corrupt storage disclosed: %+v", out)
	}
	first, err := f.service.directoryToken(f.w.ID, "docs", "child-snapshot")
	must(t, err)
	restarted := &Service{}
	second, err := restarted.directoryToken(f.w.ID, "docs", "child-snapshot")
	must(t, err)
	if first == second {
		t.Fatal("directory tokens survived service restart")
	}
}

func TestScopedReadsExcludeUnrelatedProtectedSources(t *testing.T) {
	for _, tool := range []string{"workspace_list", "workspace_search"} {
		t.Run(tool, func(t *testing.T) {
			f := newFixture(t)
			f.publish(t, 1, "folder-a/normal.txt", "matching synthetic body", "normal")
			// Independent origin tasks: shared origin provenance would correctly
			// make these revisions related regardless of their directory paths.
			f.writer, _ = f.turn(t, runtimedb.ModelLocal)
			f.publish(t, 2, "folder-b/protected.txt", "matching protected body", "local-only")
			f.writer, _ = f.turn(t, runtimedb.ModelLocal)
			f.publish(t, 3, "folder-a-other/protected.txt", "matching protected body", "local-only")
			task, local := f.turn(t, runtimedb.ModelLocal)
			call := butler.WorkspaceReadCall{Scope: "private", Path: "folder-a"}
			if tool == "workspace_search" {
				call.Query = "matching"
			}
			var page struct {
				Items []json.RawMessage `json:"items"`
			}
			decodeData(t, f.service.Call(local, tool, call), &page)
			if len(page.Items) != 1 {
				t.Fatal("scoped read included an unrelated namespace")
			}
			// Restore the same task with a cloud disclosure turn. An unrelated protected
			// revision consumed by the local page would now deny even its normal match.
			cloud, p, err := f.db.BeginPrivacyTurn(context.Background(), runtimedb.ModelCloud, runtimedb.MemoryDestination{BuiltIn: true}, "")
			must(t, err)
			must(t, p.SetTask(task))
			cloud = operation.With(cloud, operation.Identity{TaskID: task})
			decodeData(t, f.service.Call(cloud, tool, call), &page)
			if len(page.Items) != 1 {
				t.Fatal("normal scoped page changed on resume")
			}
			// A matching protected namespace must still refuse before loading bytes.
			_, protected := f.turn(t, runtimedb.ModelCloud)
			call.Path = "folder-b"
			before := f.byteReads
			out := f.service.Call(protected, tool, call)
			if !out.Rejected || f.byteReads != before || strings.Contains(out.Content, "protected.txt") {
				t.Fatalf("protected match leaked: %+v", out)
			}
		})
	}
}

func TestScopedCursorSnapshotExcludesUnrelatedMutations(t *testing.T) {
	f := newFixture(t)
	for i := 1; i <= 25; i++ {
		f.publish(t, i, fmt.Sprintf("folder-a/file-%02d.txt", i), "matching synthetic body", "normal")
	}
	f.writer, _ = f.turn(t, runtimedb.ModelLocal)
	f.publish(t, 26, "folder-b/protected.txt", "matching protected body", "local-only")
	_, ctx := f.turn(t, runtimedb.ModelCloud)
	for _, tool := range []string{"workspace_list", "workspace_search"} {
		call := butler.WorkspaceReadCall{Scope: "private", Path: "folder-a"}
		if tool == "workspace_search" {
			call.Query = "matching"
		}
		var page struct {
			NextCursor string `json:"nextCursor"`
		}
		decodeData(t, f.service.Call(ctx, tool, call), &page)
		if page.NextCursor == "" {
			t.Fatal("fixture did not paginate")
		}
		f.writer, _ = f.turn(t, runtimedb.ModelLocal)
		f.publish(t, 27+len(tool), "folder-b/unrelated-"+tool+".txt", "protected synthetic mutation", "local-only")
		call.Cursor = page.NextCursor
		decodeData(t, f.service.Call(ctx, tool, call), &page)
	}
}
