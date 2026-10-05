package api

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"html"
	"mime"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/chickenrat17/wildstone-homelab/keystone/principal"
	"github.com/chickenrat17/wildstone-homelab/keystone/redact"
	"github.com/chickenrat17/wildstone-homelab/keystone/resource"
	"github.com/chickenrat17/wildstone-homelab/keystone/runtimedb"
	"github.com/chickenrat17/wildstone-homelab/keystone/workspace"
	"github.com/chickenrat17/wildstone-homelab/keystone/workspace/storage"
)

const (
	artifactPreviewBytes = 32 << 10
	// Each page replaces the cursor it consumed, so a reader holds one live
	// cursor per listing or preview however long the artifact is. The per-Person
	// bound keeps one Person from exhausting pagination for everyone else.
	artifactCursorsPerPerson = 64
	artifactCursorsTotal     = 1024
)

type artifactCursor struct {
	Owner, Binding, Snapshot, After string
	Offset                          int
	Expires                         time.Time
}

type artifactInfo struct {
	ArtifactID  string `json:"artifactId"`
	Revision    string `json:"revision"`
	Path        string `json:"path"`
	Scope       string `json:"scope"`
	Format      string `json:"format"`
	MediaType   string `json:"mediaType"`
	SizeBytes   int64  `json:"sizeBytes"`
	UpdatedAt   string `json:"updatedAt"`
	PreviewURL  string `json:"previewUrl"`
	DownloadURL string `json:"downloadUrl"`
}

type artifactListResponse struct {
	Items      []artifactInfo `json:"items"`
	NextCursor string         `json:"nextCursor,omitempty"`
}
type artifactPreviewResponse struct {
	File       artifactInfo `json:"file"`
	Text       string       `json:"text"`
	HTML       string       `json:"html"`
	NextCursor string       `json:"nextCursor,omitempty"`
	Truncated  bool         `json:"truncated"`
}

// Metadata still passes through normal redaction. Authoritative artifact
// selectors, canonical links and server-issued cursors authorize nothing.
// Preview text is redacted in full before pagination; its escaped HTML and text
// must not be redacted again as fragments by the typed preview encoder.
func restoreArtifactSelectors(out *artifactInfo, original artifactInfo) {
	out.ArtifactID, out.Revision = original.ArtifactID, original.Revision
	out.PreviewURL, out.DownloadURL = original.PreviewURL, original.DownloadURL
}
func (v artifactListResponse) redacted() ([]byte, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	var out artifactListResponse
	if err = json.Unmarshal([]byte(redact.String(string(raw))), &out); err != nil {
		return nil, err
	}
	for i := range out.Items {
		restoreArtifactSelectors(&out.Items[i], v.Items[i])
	}
	out.NextCursor = v.NextCursor
	return json.Marshal(out)
}
func (v artifactPreviewResponse) redacted() ([]byte, error) {
	// Independently redact metadata, preserving only authoritative selectors.
	// Text/HTML were derived from the complete redacted source, so preserve their
	// stable pagination instead of running secret heuristics on page fragments.
	raw, err := (artifactListResponse{Items: []artifactInfo{v.File}}).redacted()
	if err != nil {
		return nil, err
	}
	var metadata artifactListResponse
	if err = json.Unmarshal(raw, &metadata); err != nil {
		return nil, err
	}
	v.File = metadata.Items[0]
	return json.Marshal(v)
}

func (s *Server) registerArtifactRoutes(mux *http.ServeMux) {
	for _, suffix := range []string{"", "/{artifact}/revisions", "/{artifact}/revisions/{revision}", "/{artifact}/revisions/{revision}/download"} {
		console := s.handleResponse(principal.VerbRead, func(w http.ResponseWriter, r *http.Request, id principal.Identity) (any, int, error) {
			// No machine-name fallback or delegated Agent bearer identity.
			if id.Role != runtimedb.RoleAdmin || id.ActingFor == "" {
				return artifactDenied()
			}
			return s.personArtifacts(w, r, id.ActingFor, false)
		})
		portal := s.portal(surfaceAssistant, func(w http.ResponseWriter, r *http.Request, member string, _ []resource.Resource) (any, int, error) {
			return s.personArtifacts(w, r, member, true)
		})
		mux.Handle("GET /api/v1/agents/{agent}/artifacts"+suffix, artifactHeaders(console))
		mux.Handle("GET /api/v1/assistant/agents/{agent}/artifacts"+suffix, artifactHeaders(portal))
	}
}

func artifactHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "private, no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Content-Security-Policy", "default-src 'none'; sandbox")
		w.Header().Set("Referrer-Policy", "no-referrer")
		next.ServeHTTP(w, r)
	})
}

func artifactDenied() (any, int, error) {
	return nil, http.StatusNotFound, errors.New("not_found: artifact unavailable")
}
func artifactUnavailable() (any, int, error) {
	return nil, http.StatusServiceUnavailable, errors.New("storage_unavailable: artifact unavailable")
}
func artifactInvalid() (any, int, error) {
	return nil, http.StatusBadRequest, errors.New("invalid_input: invalid artifact request")
}

func (s *Server) personArtifacts(w http.ResponseWriter, r *http.Request, member string, household bool) (any, int, error) {
	agent, artifact, revision := r.PathValue("agent"), r.PathValue("artifact"), r.PathValue("revision")
	for _, id := range []string{agent, artifact, revision} {
		if len(id) > 128 || !utf8.ValidString(id) {
			return artifactInvalid()
		}
	}
	query := r.URL.Query()
	for k, v := range query {
		if (k != "cursor" && k != "scope") || len(v) != 1 {
			return artifactInvalid()
		}
	}
	if scope := query.Get("scope"); scope != "" && scope != "private" {
		return artifactDenied()
	}
	token := query.Get("cursor")
	if len(token) > 512 {
		return artifactInvalid()
	}
	binding := member + "\x00" + agent + "\x00" + strconv.FormatBool(household) + "\x00" + artifact + "\x00" + revision
	var cursor artifactCursor
	if token != "" {
		var ok bool
		cursor, ok = s.artifactCursor(token, binding)
		if !ok {
			return artifactInvalid()
		}
	}
	if s.DB == nil {
		return artifactUnavailable()
	}
	ws, entries, snapshot, err := s.DB.PersonWorkspaceEntries(r.Context(), member, agent, household, artifact, revision, cursor.After)
	if errors.Is(err, runtimedb.ErrWorkspaceDenied) {
		return artifactDenied()
	}
	if err != nil {
		return artifactUnavailable()
	}
	for _, e := range entries {
		if storage.ValidatePath(e.Artifact.Path, false) != nil {
			return artifactUnavailable()
		}
	}
	if token != "" && cursor.Snapshot != snapshot {
		return nil, http.StatusConflict, errors.New("stale_cursor: artifact listing changed")
	}
	if revision == "" {
		if artifact != "" && len(entries) == 0 {
			return artifactDenied()
		}
		next := ""
		if len(entries) > 20 {
			last := entries[19]
			next, err = s.mintArtifactCursor(artifactCursor{Owner: member, Binding: binding, Snapshot: snapshot, After: last.Artifact.ID + "\x00" + last.Revision.ID}, token)
			if err != nil {
				return artifactUnavailable()
			}
			entries = entries[:20]
		}
		items := make([]artifactInfo, 0, len(entries))
		for _, e := range entries {
			items = append(items, artifactInformation(agent, e, household))
		}
		return artifactListResponse{items, next}, http.StatusOK, nil
	}
	if len(entries) != 1 {
		return artifactDenied()
	}
	e := entries[0]
	var data []byte
	if s.artifactReadBytes != nil {
		data, err = s.artifactReadBytes(r.Context(), ws, e.Revision)
	} else {
		data, err = s.readArtifactBytes(r.Context(), ws, e.Revision)
	}
	if err != nil || int64(len(data)) != e.Revision.SizeBytes || !utf8.Valid(data) || bytes.IndexByte(data, 0) >= 0 {
		return artifactUnavailable()
	}
	// Repeat current admission, class/surface, grant, committed state and source
	// authority after filesystem I/O. Removed/revoked/trash never uses cached bytes.
	_, checked, afterSnapshot, err := s.DB.PersonWorkspaceEntries(r.Context(), member, agent, household, artifact, revision, "")
	if errors.Is(err, runtimedb.ErrWorkspaceDenied) || (err == nil && len(checked) != 1) {
		return artifactDenied()
	}
	if err != nil {
		return artifactUnavailable()
	}
	if afterSnapshot != snapshot {
		return nil, http.StatusConflict, errors.New("conflict: artifact changed")
	}
	info := artifactInformation(agent, e, household)
	if strings.HasSuffix(r.URL.Path, "/download") {
		if token != "" {
			return artifactInvalid()
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": redact.String(path.Base(info.Path))}))
		w.Header().Set("Content-Length", strconv.Itoa(len(data)))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(data)
		return nil, http.StatusOK, nil
	}
	// Redact the complete preview before splitting it: a value crossing a
	// page boundary must not survive as individually unrecognizable fragments.
	// Cursor offsets address this deterministic redacted text; downloads above
	// retain the original authenticated revision bytes.
	preview := redact.String(string(data))
	if cursor.Offset < 0 || cursor.Offset > len(preview) {
		return artifactInvalid()
	}
	end := min(len(preview), cursor.Offset+artifactPreviewBytes)
	for end < len(preview) && !utf8.RuneStart(preview[end]) {
		end--
	}
	text := preview[cursor.Offset:end]
	next := ""
	if end < len(preview) {
		next, err = s.mintArtifactCursor(artifactCursor{Owner: member, Binding: binding, Snapshot: snapshot, Offset: end}, token)
		if err != nil {
			return artifactUnavailable()
		}
	}
	// Every format, including Markdown, uses escaped text only. No raw HTML,
	// script, link or remote embed is rendered; attachments remain inert bytes.
	return artifactPreviewResponse{info, text, "<pre>" + html.EscapeString(text) + "</pre>", next, end < len(preview)}, http.StatusOK, nil
}

func artifactInformation(agent string, e runtimedb.PersonWorkspaceEntry, household bool) artifactInfo {
	prefix := "/api/v1/agents/"
	if household {
		prefix = "/api/v1/assistant/agents/"
	}
	base := prefix + url.PathEscape(agent) + "/artifacts/" + url.PathEscape(e.Artifact.ID) + "/revisions/" + url.PathEscape(e.Revision.ID)
	return artifactInfo{e.Artifact.ID, e.Revision.ID, e.Artifact.Path, "private", strings.TrimPrefix(path.Ext(e.Artifact.Path), "."), e.Revision.MediaType, e.Revision.SizeBytes, e.Revision.CreatedAt, base, base + "/download"}
}

func (s *Server) readArtifactBytes(ctx context.Context, w runtimedb.Workspace, r runtimedb.WorkspaceRevision) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	settings, err := s.WorkspaceSettings()
	if err != nil {
		return nil, err
	}
	store, err := storage.New(settings, w.PersonID, strconv.FormatInt(w.AgentID, 10))
	if err != nil {
		return nil, err
	}
	return store.Read(storage.Object{ID: r.ID, SHA256: r.Hash, Size: r.SizeBytes})
}

func (s *Server) artifactCursor(token, binding string) (artifactCursor, bool) {
	s.artifactCursorMu.Lock()
	defer s.artifactCursorMu.Unlock()
	c, ok := s.artifactCursors[token]
	return c, ok && c.Binding == binding && time.Now().Before(c.Expires)
}

// mintArtifactCursor stores c and removes replaces, the already validated cursor
// this page consumed. A Person at their bound loses their own oldest cursor
// rather than being refused; only the process-wide bound refuses.
func (s *Server) mintArtifactCursor(c artifactCursor, replaces string) (string, error) {
	s.artifactCursorMu.Lock()
	defer s.artifactCursorMu.Unlock()
	if s.artifactCursors == nil {
		s.artifactCursors = make(map[string]artifactCursor)
	}
	delete(s.artifactCursors, replaces)
	now := time.Now()
	owned, oldest := 0, ""
	for k, v := range s.artifactCursors {
		if !now.Before(v.Expires) {
			delete(s.artifactCursors, k)
			continue
		}
		if v.Owner == c.Owner {
			owned++
			if oldest == "" || v.Expires.Before(s.artifactCursors[oldest].Expires) {
				oldest = k
			}
		}
	}
	if owned >= artifactCursorsPerPerson {
		delete(s.artifactCursors, oldest)
	}
	if len(s.artifactCursors) >= artifactCursorsTotal {
		return "", workspace.ErrUnavailable
	}
	var id [24]byte
	if _, err := rand.Read(id[:]); err != nil {
		return "", err
	}
	c.Expires = now.Add(15 * time.Minute)
	token := hex.EncodeToString(id[:])
	s.artifactCursors[token] = c
	return token, nil
}
