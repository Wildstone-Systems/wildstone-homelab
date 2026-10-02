package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/chickenrat17/wildstone-homelab/keystone/redact"
	"github.com/chickenrat17/wildstone-homelab/keystone/runtimedb"
)

func TestArtifactPreviewRedactsBeforePageBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name, value string
		prefixBytes int
	}{
		{"hex-fragments", strings.Repeat("b", 32), artifactPreviewBytes - 16},
		{"credential-value", "password: short-value-that-crosses-page", artifactPreviewBytes - 20},
		{"credential-key", "password: short-value", artifactPreviewBytes - len("password: ")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, w, token, data := artifactFixture(t, true)
			payload := strings.Repeat(" ", tc.prefixBytes) + tc.value + " " + strings.Repeat("é", 40)
			a, r := publishArtifact(t, h, w, data, 0, "txt", payload)
			base := "/api/v1/agents/writer/artifacts/" + a + "/revisions/" + r
			var text, rendered strings.Builder
			requestPath := base
			pages := 0
			for {
				resp, body := artifactRequest(t, h, requestPath, token)
				var page artifactPreviewResponse
				if resp.StatusCode != 200 {
					t.Fatalf("preview page: %d %s", resp.StatusCode, body)
				}
				if err := json.Unmarshal(body, &page); err != nil {
					t.Fatal(err)
				}
				if !utf8.ValidString(page.Text) || len(page.Text) > artifactPreviewBytes || page.HTML != "<pre>"+html.EscapeString(page.Text)+"</pre>" {
					t.Fatal("preview lost bounded UTF-8 or escaped HTML")
				}
				text.WriteString(page.Text)
				rendered.WriteString(strings.TrimSuffix(strings.TrimPrefix(page.HTML, "<pre>"), "</pre>"))
				pages++
				if page.NextCursor == "" {
					if page.Truncated {
						t.Fatal("truncated page has no continuation")
					}
					break
				}
				if !page.Truncated || pages >= 2 {
					t.Fatal("unexpected preview continuation")
				}
				requestPath = base + "?cursor=" + url.QueryEscape(page.NextCursor)
			}
			want := redact.String(payload)
			if pages != 2 || text.String() != want || html.UnescapeString(rendered.String()) != want || !strings.Contains(want, redact.Placeholder) {
				t.Fatal("preview pages reconstruct unredacted content or lose continuation text")
			}
			resp, body := artifactRequest(t, h, base+"/download", token)
			if resp.StatusCode != 200 || string(body) != payload {
				t.Fatal("authenticated attachment bytes changed")
			}
		})
	}
}

func artifactFixture(t *testing.T, owner bool) (*harness, runtimedb.Workspace, string, map[string][]byte) {
	t.Helper()
	h, _ := portalHarness(t)
	class, surface := runtimedb.AgentClassMember, runtimedb.ButlerSurfaceFamily
	token := ""
	if owner {
		class, surface = runtimedb.AgentClassOwner, runtimedb.ButlerSurfaceAdmin
		token = h.console(runtimedb.RoleAdmin, "reader")
	} else {
		h.person("reader")
	}
	if err := h.s.DB.UpsertModel(runtimedb.Model{Name: "artifact-model", Kind: runtimedb.ModelLocal, Provider: "fixture"}, "fixture"); err != nil {
		t.Fatal(err)
	}
	agent, err := h.s.DB.CreateButler(runtimedb.Butler{Name: "writer", Directive: "fixture", AgentClass: class, Surface: surface, Model: "artifact-model"}, "fixture")
	if err != nil {
		t.Fatal(err)
	}
	if err = h.s.DB.GrantButlerAccess("writer", "reader", "fixture"); err != nil {
		t.Fatal(err)
	}
	p, err := h.s.DB.GetButlerMember("reader")
	if err != nil {
		t.Fatal(err)
	}
	w := runtimedb.Workspace{ID: "reader-workspace", PersonID: p.PersonID, AgentID: agent}
	if err = h.s.DB.CreateWorkspace(w); err != nil {
		t.Fatal(err)
	}
	data := make(map[string][]byte)
	h.s.artifactReadBytes = func(_ context.Context, _ runtimedb.Workspace, r runtimedb.WorkspaceRevision) ([]byte, error) {
		return data[r.ID], nil
	}
	return h, w, token, data
}

func publishArtifact(t *testing.T, h *harness, w runtimedb.Workspace, data map[string][]byte, n int, format, body string) (string, string) {
	t.Helper()
	b, err := h.s.DB.GetButler("writer")
	if err != nil {
		t.Fatal(err)
	}
	id, err := h.s.DB.InsertButlerTask(runtimedb.ButlerTask{Kind: runtimedb.ButlerTaskConversation, Author: "reader", Owner: "reader", Surface: b.Surface, AgentClass: b.AgentClass, Audience: runtimedb.ButlerAudiencePrivate, ObjectiveKind: runtimedb.ObjectiveInformational, Objective: "fixture"})
	if err != nil {
		t.Fatal(err)
	}
	if err = h.s.DB.SetTaskButler(id, "writer"); err != nil {
		t.Fatal(err)
	}
	task, err := strconv.ParseInt(strings.TrimPrefix(id, "bt-"), 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	artifact, revision := fmt.Sprintf("artifact-%02d", n), fmt.Sprintf("%032x", n+1)
	if err = h.s.DB.CreateWorkspaceArtifact(runtimedb.WorkspaceArtifact{ID: artifact, WorkspaceID: w.ID, Path: artifact + "." + format}); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(body))
	media := map[string]string{"md": "text/markdown", "txt": "text/plain", "json": "application/json", "csv": "text/csv"}[format]
	if err = h.s.DB.CreateWorkspaceRevision(runtimedb.WorkspaceRevision{ID: revision, ArtifactID: artifact, TaskID: task, Hash: hex.EncodeToString(sum[:]), SizeBytes: int64(len(body)), MediaType: media, Privacy: "normal"}); err != nil {
		t.Fatal(err)
	}
	if err = h.s.DB.ReserveWorkspaceBytes(runtimedb.WorkspaceReservation{ID: "reserve-" + revision, WorkspaceID: w.ID, TaskID: task, SizeBytes: int64(len(body)), Purpose: "publication"}); err != nil {
		t.Fatal(err)
	}
	p := runtimedb.WorkspacePublication{ID: "pub-" + revision, WorkspaceID: w.ID, ArtifactID: artifact, RevisionID: revision, TaskID: task, ReservationID: "reserve-" + revision, EffectKey: "key-" + revision}
	if err = h.s.DB.CreateWorkspacePublication(p); err != nil {
		t.Fatal(err)
	}
	if err = h.s.DB.PrepareWorkspacePublication(w.ID, p.ID); err != nil {
		t.Fatal(err)
	}
	if err = h.s.DB.MarkWorkspaceBytesPublished(w.ID, p.ID); err != nil {
		t.Fatal(err)
	}
	if err = h.s.DB.CommitWorkspacePublication(w.ID, p.ID, "receipt-"+revision); err != nil {
		t.Fatal(err)
	}
	data[revision] = []byte(body)
	return artifact, revision
}

func artifactRequest(t *testing.T, h *harness, path, token string) (*http.Response, []byte) {
	t.Helper()
	req, err := http.NewRequest("GET", h.srv.URL+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp, body
}

func TestArtifactConsolePreviewDownloadAndRevokedLinks(t *testing.T) {
	h, w, token, data := artifactFixture(t, true)
	payload := "# Report\n<script>alert(1)</script><img src=\"https://example.com/track\">\n![remote](https://example.com/remote)"
	a, r := publishArtifact(t, h, w, data, 0, "md", payload)
	base := "/api/v1/agents/writer/artifacts/" + a + "/revisions/" + r
	resp, body := artifactRequest(t, h, base, token)
	if resp.StatusCode != 200 {
		t.Fatalf("preview %d %s", resp.StatusCode, body)
	}
	var preview struct {
		File       artifactInfo `json:"file"`
		Text, HTML string
	}
	if err := json.Unmarshal(body, &preview); err != nil {
		t.Fatal(err)
	}
	if preview.Text != payload || strings.Contains(preview.HTML, "<script") || strings.Contains(preview.HTML, "<img") || strings.Contains(preview.HTML, "<a ") || preview.File.MediaType != "text/markdown" || preview.File.DownloadURL != base+"/download" {
		t.Fatalf("unsafe or incorrect preview %+v", preview)
	}
	resp, body = artifactRequest(t, h, "/api/v1/agents/writer/artifacts/"+a+"/revisions", token)
	var history artifactListResponse
	if err := json.Unmarshal(body, &history); err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 || len(history.Items) != 1 || history.Items[0].Revision != r {
		t.Fatalf("revisions %d %s", resp.StatusCode, body)
	}
	resp, body = artifactRequest(t, h, base+"/download", token)
	if resp.StatusCode != 200 || string(body) != payload || resp.Header.Get("Content-Type") != "application/octet-stream" || resp.Header.Get("X-Content-Type-Options") != "nosniff" || !strings.HasPrefix(resp.Header.Get("Content-Disposition"), "attachment;") || !strings.Contains(resp.Header.Get("Cache-Control"), "no-store") {
		t.Fatalf("download %d %v %s", resp.StatusCode, resp.Header, body)
	}
	resp, _ = artifactRequest(t, h, base, "")
	if resp.StatusCode != 401 {
		t.Fatalf("public link %d", resp.StatusCode)
	}
	if err := h.s.DB.RevokeButlerAccess("writer", "reader", "fixture"); err != nil {
		t.Fatal(err)
	}
	resp, body = artifactRequest(t, h, base+"/download", token)
	if resp.StatusCode != 404 || strings.Contains(string(body), "Report") {
		t.Fatalf("revoked %d %s", resp.StatusCode, body)
	}
}

func TestArtifactPortalPersonAndOwnerSurfaceIsolation(t *testing.T) {
	for _, owner := range []bool{false, true} {
		t.Run(strconv.FormatBool(owner), func(t *testing.T) {
			h, w, _, data := artifactFixture(t, owner)
			publishArtifact(t, h, w, data, 0, "txt", "private body")
			h.person("other")
			for _, tc := range []struct {
				person, groups string
				want           int
			}{{"reader", "assistant_users", 200}, {"other", "assistant_users", 404}, {"reader", "", 403}, {"", "assistant_users", 403}} {
				want := tc.want
				if owner && want == 200 {
					want = 404
				}
				resp := h.member("GET", "/api/v1/assistant/agents/writer/artifacts", tc.person, tc.groups, nil, "")
				body, _ := io.ReadAll(resp.Body)
				resp.Body.Close()
				if resp.StatusCode != want {
					t.Fatalf("%+v: %d %s", tc, resp.StatusCode, body)
				}
				if want != 200 && strings.Contains(string(body), "artifact-00") {
					t.Fatalf("metadata leak: %s", body)
				}
			}
		})
	}
}

func TestArtifactAuthorityRecheckedAfterRead(t *testing.T) {
	h, w, token, data := artifactFixture(t, true)
	a, r := publishArtifact(t, h, w, data, 0, "json", "{\"safe\":true}")
	h.s.artifactReadBytes = func(_ context.Context, _ runtimedb.Workspace, rev runtimedb.WorkspaceRevision) ([]byte, error) {
		if err := h.s.DB.RevokeButlerAccess("writer", "reader", "fixture"); err != nil {
			return nil, err
		}
		return data[rev.ID], nil
	}
	resp, body := artifactRequest(t, h, "/api/v1/agents/writer/artifacts/"+a+"/revisions/"+r+"/download", token)
	if resp.StatusCode != 404 || strings.Contains(string(body), "safe") {
		t.Fatalf("post-I/O revocation: %d %s", resp.StatusCode, body)
	}
}

func TestArtifactPaginationBindsPersonSurfaceAndSnapshot(t *testing.T) {
	h, w, token, data := artifactFixture(t, true)
	for i := 0; i < 22; i++ {
		publishArtifact(t, h, w, data, i, "csv", "name,value\na,1\n")
	}
	base := "/api/v1/agents/writer/artifacts"
	resp, body := artifactRequest(t, h, base, token)
	var first struct {
		Items      []artifactInfo
		NextCursor string
	}
	if err := json.Unmarshal(body, &first); err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 || len(first.Items) != 20 || first.NextCursor == "" {
		t.Fatalf("first: %d %s", resp.StatusCode, body)
	}
	page := base + "?cursor=" + url.QueryEscape(first.NextCursor)
	resp, body = artifactRequest(t, h, page, token)
	var second struct{ Items []artifactInfo }
	if err := json.Unmarshal(body, &second); err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 || len(second.Items) != 2 {
		t.Fatalf("second: %d %s", resp.StatusCode, body)
	}
	other := h.console(runtimedb.RoleAdmin, "other")
	resp, _ = artifactRequest(t, h, page, other)
	if resp.StatusCode == 200 {
		t.Fatal("cursor transferred between people")
	}
	resp, _ = artifactRequest(t, h, base+"?cursor=guessed", token)
	if resp.StatusCode != 400 {
		t.Fatalf("forged cursor %d", resp.StatusCode)
	}
	publishArtifact(t, h, w, data, 22, "txt", "new")
	resp, _ = artifactRequest(t, h, page, token)
	if resp.StatusCode != 409 {
		t.Fatalf("stale cursor %d", resp.StatusCode)
	}
}

func TestArtifactErrorConcealsStorageAndMachineIdentity(t *testing.T) {
	h, w, token, data := artifactFixture(t, true)
	a, r := publishArtifact(t, h, w, data, 0, "txt", "private body")
	base := "/api/v1/agents/writer/artifacts/" + a + "/revisions/" + r
	h.s.artifactReadBytes = func(context.Context, runtimedb.Workspace, runtimedb.WorkspaceRevision) ([]byte, error) {
		return nil, fmt.Errorf("host path /private/host/file")
	}
	resp, body := artifactRequest(t, h, base, token)
	if resp.StatusCode != 503 || strings.Contains(string(body), "/private/") {
		t.Fatalf("error leak %d %s", resp.StatusCode, body)
	}
	resp, _ = artifactRequest(t, h, base, h.tokens[runtimedb.RoleAdmin])
	if resp.StatusCode != 404 {
		t.Fatalf("machine inferred person %d", resp.StatusCode)
	}
	agentToken, err := h.s.Principals.Mint("butler", "reader", 0)
	if err != nil {
		t.Fatal(err)
	}
	resp, _ = artifactRequest(t, h, base, agentToken)
	if resp.StatusCode != 404 {
		t.Fatalf("delegated Agent person %d", resp.StatusCode)
	}
	resp, _ = artifactRequest(t, h, base+"?scope=shared", token)
	if resp.StatusCode != 404 {
		t.Fatalf("unimplemented shared %d", resp.StatusCode)
	}
}

func TestArtifactSelectorsSurviveWhileContentAndFilenameRedact(t *testing.T) {
	h, w, token, data := artifactFixture(t, true)
	private := strings.Repeat("b", 48)
	a, r := publishArtifact(t, h, w, data, 0, "txt", "private value: "+private)
	if err := h.s.DB.TrashWorkspaceArtifact(w.ID, a, r); err != nil {
		t.Fatal(err)
	}
	if err := h.s.DB.RestoreWorkspaceArtifact(w.ID, a, r, private+".txt"); err != nil {
		t.Fatal(err)
	}
	resp, body := artifactRequest(t, h, "/api/v1/agents/writer/artifacts/"+a+"/revisions/"+r, token)
	if resp.StatusCode != 200 || strings.Contains(string(body), private) {
		t.Fatalf("private preview %d %s", resp.StatusCode, body)
	}
	var preview artifactPreviewResponse
	if err := json.Unmarshal(body, &preview); err != nil {
		t.Fatal(err)
	}
	if preview.File.Revision != r || preview.File.ArtifactID != a || !strings.Contains(preview.Text, "[REDACTED]") || !strings.Contains(preview.File.Path, "[REDACTED]") {
		t.Fatalf("redaction/selector mismatch: %+v", preview)
	}
	resp, body = artifactRequest(t, h, preview.File.DownloadURL, token)
	if resp.StatusCode != 200 || string(body) != "private value: "+private {
		t.Fatalf("authenticated bytes %d %s", resp.StatusCode, body)
	}
	_, params, err := mime.ParseMediaType(resp.Header.Get("Content-Disposition"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(params["filename"], private) || !strings.Contains(params["filename"], "[REDACTED]") {
		t.Fatalf("filename leak %v", params)
	}
}

func TestArtifactPreviewPaginationAndPortalTrust(t *testing.T) {
	h, w, token, data := artifactFixture(t, true)
	payload := strings.Repeat("é", artifactPreviewBytes/2+1)
	a, r := publishArtifact(t, h, w, data, 0, "txt", payload)
	base := "/api/v1/agents/writer/artifacts/" + a + "/revisions/" + r
	resp, body := artifactRequest(t, h, base, token)
	var first artifactPreviewResponse
	if err := json.Unmarshal(body, &first); err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 || !first.Truncated || first.NextCursor == "" || len(first.Text) != artifactPreviewBytes {
		t.Fatalf("preview page %d %+v", resp.StatusCode, first)
	}
	resp, body = artifactRequest(t, h, base+"?cursor="+url.QueryEscape(first.NextCursor), token)
	var next artifactPreviewResponse
	if err := json.Unmarshal(body, &next); err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 || next.Truncated || first.Text+next.Text != payload {
		t.Fatalf("text continuation %d %+v", resp.StatusCode, next)
	}
	h.s.SetTrustedIngress([]string{"192.0.2.1"})
	resp = h.member("GET", "/api/v1/assistant/agents/writer/artifacts", "reader", "assistant_users", nil, "")
	resp.Body.Close()
	if resp.StatusCode != 403 {
		t.Fatalf("untrusted forwarded identity %d", resp.StatusCode)
	}
}
