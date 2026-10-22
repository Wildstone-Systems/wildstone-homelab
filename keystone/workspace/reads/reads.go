// Package reads coordinates scoped, bounded native workspace read tools.
package reads

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/Wildstone-Systems/wildstone-homelab/keystone/butler"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/operation"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/runtimedb"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/workspace"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/workspace/storage"
)

var errCursor = errors.New("stale_cursor: workspace page is unavailable")
var errInput = errors.New("invalid_input: invalid workspace read request")

type cursor struct {
	task, tool, workspace, config, snapshot, after string
	afterDirectory                                 string
	call                                           butler.WorkspaceReadCall
	offset                                         int
	expires                                        time.Time
}

// Service is shared across gateway sessions. Settings reloads current desired
// state/readiness on every call. Cursor handles are bounded, temporary and
// scoped to the trusted task; restart invalidates them without restoring grants.
type Service struct {
	DB           *runtimedb.DB
	Settings     func() (workspace.Settings, error)
	mu           sync.Mutex
	cursors      map[string]cursor
	directoryKey []byte
	// Private fault seams keep fake storage and clocks out of production wiring.
	read      func(*storage.Store, storage.Object) ([]byte, error)
	now       func() time.Time
	scanFence func(string)
}

// selector renders only server-owned lookup identities as JSON escapes.
// Decoding restores their exact values; central redaction continues to process
// ordinary paths, snippets and text. Never use it for input or file content.
type selector string

func (v selector) MarshalJSON() ([]byte, error) {
	var out strings.Builder
	out.WriteByte('"')
	for _, r := range utf16.Encode([]rune(v)) {
		fmt.Fprintf(&out, `\u%04x`, r)
	}
	out.WriteByte('"')
	return []byte(out.String()), nil
}

type fileInfo struct {
	ArtifactID string `json:"artifactId"`
	Revision   string `json:"revision"`
	Path       string `json:"path"`
	Scope      string `json:"scope"`
	Format     string `json:"format"`
	SizeBytes  int64  `json:"sizeBytes"`
	UpdatedAt  string `json:"updatedAt"`
}

func (v fileInfo) MarshalJSON() ([]byte, error) {
	type plain fileInfo
	return json.Marshal(struct {
		*plain
		ArtifactID selector `json:"artifactId"`
		Revision   selector `json:"revision"`
	}{(*plain)(&v), selector(v.ArtifactID), selector(v.Revision)})
}

// Directory tokens describe committed nonempty child snapshots only. They
// are process-owned read evidence, never mkdir/rmdir mutation authority.
func (s *Service) directoryToken(workspaceID, directory, snapshot string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.directoryKey == nil {
		s.directoryKey = make([]byte, 32)
		if _, err := rand.Read(s.directoryKey); err != nil {
			s.directoryKey = nil
			return "", workspace.ErrUnavailable
		}
	}
	mac := hmac.New(sha256.New, s.directoryKey)
	for _, value := range []string{workspaceID, directory, snapshot} {
		mac.Write([]byte(strconv.Itoa(len(value)) + ":" + value))
	}
	return "read-" + hex.EncodeToString(mac.Sum(nil)), nil
}

type entry struct {
	Kind      string         `json:"kind"`
	File      *fileInfo      `json:"file,omitempty"`
	Directory *directoryInfo `json:"directory,omitempty"`
}
type directoryInfo struct {
	Scope    string `json:"scope"`
	Path     string `json:"path"`
	Revision string `json:"directoryRevision"`
}
type hit struct {
	File    fileInfo `json:"file"`
	Snippet string   `json:"snippet"`
}
type textPage struct {
	Text       string `json:"text"`
	NextCursor string `json:"nextCursor,omitempty"`
	Truncated  bool   `json:"truncated"`
}

func (v directoryInfo) MarshalJSON() ([]byte, error) {
	type plain directoryInfo
	return json.Marshal(struct {
		*plain
		Revision selector `json:"directoryRevision"`
	}{(*plain)(&v), selector(v.Revision)})
}

func (v textPage) MarshalJSON() ([]byte, error) {
	type plain textPage
	return json.Marshal(struct {
		*plain
		NextCursor selector `json:"nextCursor,omitempty"`
	}{(*plain)(&v), selector(v.NextCursor)})
}

func (s *Service) clock() time.Time {
	if s.now != nil {
		return s.now()
	}
	return time.Now()
}
func (s *Service) save(c cursor) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cursors == nil {
		s.cursors = map[string]cursor{}
	}
	for token, old := range s.cursors {
		if !old.expires.After(s.clock()) {
			delete(s.cursors, token)
		}
	}
	if len(s.cursors) >= 1024 {
		return "", workspace.ErrUnavailable
	}
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", workspace.ErrUnavailable
	}
	token := hex.EncodeToString(value[:])
	c.expires = s.clock().Add(15 * time.Minute)
	s.cursors[token] = c
	return token, nil
}
func (s *Service) load(token string) (cursor, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.cursors[token]
	if !ok || !c.expires.After(s.clock()) {
		return cursor{}, errCursor
	}
	return c, nil
}

func result(data any) butler.ToolOutcome {
	content, err := json.Marshal(struct {
		Version int `json:"version"`
		Data    any `json:"data"`
	}{1, data})
	// Leave room for the complete existing untrusted envelope, including JSON
	// escaping. Never use gateway truncation to create a partial JSON document.
	if err != nil || len(content) > 3600 {
		return failure(workspace.ErrUnavailable)
	}
	return butler.ToolOutcome{Content: string(content)}
}
func failure(err error) butler.ToolOutcome {
	if errors.Is(err, runtimedb.ErrPrivacy) {
		return butler.ToolOutcome{Rejected: true, Content: "privacy_denied: workspace source policy refuses this destination"}
	}
	if errors.Is(err, runtimedb.ErrWorkspaceDenied) || errors.Is(err, runtimedb.ErrWorkspaceContext) {
		return butler.ToolOutcome{Rejected: true, Content: err.Error()}
	}
	for _, safe := range []error{errCursor, errInput, workspace.ErrUnavailable, workspace.ErrSettings, workspace.ErrDisabled, workspace.ErrFilesystem, workspace.ErrRoot, workspace.ErrMount, storage.ErrPath, storage.ErrFormat, storage.ErrIntegrity} {
		if errors.Is(err, safe) {
			return butler.ToolOutcome{Failed: true, Content: safe.Error()}
		}
	}
	return butler.ToolOutcome{Failed: true, Content: workspace.ErrUnavailable.Error()}
}

func info(a runtimedb.WorkspaceArtifact, r runtimedb.WorkspaceRevision) fileInfo {
	return fileInfo{ArtifactID: a.ID, Revision: r.ID, Path: a.Path, Scope: "private", Format: strings.TrimPrefix(path.Ext(a.Path), "."), SizeBytes: r.SizeBytes, UpdatedAt: r.CreatedAt}
}

func (s *Service) bytes(ctx context.Context, access *runtimedb.WorkspaceAccess, settings workspace.Settings, w runtimedb.Workspace, a runtimedb.WorkspaceArtifact, revision string) (fileInfo, []byte, error) {
	r, err := s.DB.ReadWorkspaceRevisionForArtifact(ctx, access, a, revision)
	if err != nil {
		if errors.Is(err, runtimedb.ErrWorkspaceConflict) {
			err = errCursor
		}
		return fileInfo{}, nil, err
	}
	store, err := storage.New(settings, w.PersonID, strconv.FormatInt(w.AgentID, 10))
	if err != nil {
		return fileInfo{}, nil, err
	}
	var data []byte
	if s.read == nil {
		data, err = ReadCommittedBytes(settings, w, r)
	} else {
		data, err = s.read(store, storage.Object{ID: r.ID, SHA256: r.Hash, Size: r.SizeBytes})
	}
	if err != nil {
		return fileInfo{}, nil, err
	}
	// Repeat authority after byte I/O; revoked access never reaches the model.
	if _, err = s.DB.ReadWorkspaceRevisionForArtifact(ctx, access, a, revision); err != nil {
		if errors.Is(err, runtimedb.ErrWorkspaceConflict) {
			err = errCursor
		}
		return fileInfo{}, nil, err
	}
	if !utf8.Valid(data) {
		return fileInfo{}, nil, storage.ErrFormat
	}
	return info(a, r), data, nil
}

func (s *Service) Call(ctx context.Context, tool string, call butler.WorkspaceReadCall) butler.ToolOutcome {
	if s == nil || s.DB == nil || s.Settings == nil {
		return failure(workspace.ErrUnavailable)
	}
	taskID := operation.From(ctx).TaskID
	if taskID == "" {
		return failure(runtimedb.ErrWorkspaceContext)
	}
	task, err := s.DB.GetButlerTask(taskID)
	if err != nil {
		return failure(runtimedb.ErrWorkspaceDenied)
	}
	agent, err := s.DB.TaskButler(taskID)
	if err != nil {
		return failure(runtimedb.ErrWorkspaceDenied)
	}
	workspaceID, err := s.DB.ResolvePrivateWorkspaceTask(task.Owner, agent, taskID, task.OriginConnection)
	if err != nil {
		return failure(err)
	}
	w, err := s.DB.WorkspaceByID(workspaceID)
	if err != nil {
		return failure(err)
	}
	settings, err := s.Settings()
	if err != nil {
		return failure(err)
	}
	if err = settings.Validate(); err != nil || settings.Revision == "" {
		return failure(workspace.ErrSettings)
	}
	access := s.DB.BindWorkspaceAccess(task.Owner, agent, taskID, task.OriginConnection)
	c := cursor{task: taskID, tool: tool, workspace: workspaceID, config: settings.Revision, call: call}
	if call.Cursor != "" {
		c, err = s.load(call.Cursor)
		check := call
		check.Cursor = ""
		if err != nil || c.task != taskID || c.tool != tool || c.workspace != workspaceID || c.config != settings.Revision || c.call != check {
			return failure(errCursor)
		}
	}
	if tool == "workspace_read" {
		a, err := s.DB.WorkspaceArtifact(workspaceID, call.ArtifactID)
		if err != nil {
			return failure(runtimedb.ErrWorkspaceDenied)
		}
		file, data, err := s.bytes(ctx, access, settings, w, a, call.Revision)
		if err != nil {
			return failure(err)
		}
		if c.offset < 0 || c.offset > len(data) {
			return failure(errCursor)
		}
		end := c.offset + 2500
		if end > len(data) {
			end = len(data)
		}
		for end > c.offset && !utf8.Valid(data[c.offset:end]) {
			end--
		}
		for {
			page := textPage{Text: string(data[c.offset:end]), Truncated: end < len(data)}
			if page.Truncated && end == c.offset {
				return failure(workspace.ErrUnavailable)
			}
			token := ""
			if page.Truncated {
				next := c
				next.offset = end
				token, err = s.save(next)
				if err != nil {
					return failure(err)
				}
				page.NextCursor = token
			}
			out := result(struct {
				File fileInfo `json:"file"`
				Page textPage `json:"page"`
			}{file, page})
			if !out.Failed {
				return out
			}
			if token != "" {
				s.mu.Lock()
				delete(s.cursors, token)
				s.mu.Unlock()
			}
			if end <= c.offset {
				return failure(workspace.ErrUnavailable)
			}
			end--
			for end > c.offset && !utf8.Valid(data[c.offset:end]) {
				end--
			}
		}
	}
	if call.Scope != "private" {
		return failure(runtimedb.ErrWorkspaceDenied)
	}
	if storage.ValidatePath(call.Path, true) != nil || (tool == "workspace_search" && strings.TrimSpace(call.Query) == "") {
		return failure(errInput)
	}
	if tool != "workspace_list" && tool != "workspace_search" {
		return failure(errInput)
	}
	return s.scan(ctx, access, settings, w, c)
}

func (s *Service) scan(ctx context.Context, access *runtimedb.WorkspaceAccess, settings workspace.Settings, w runtimedb.Workspace, c cursor) butler.ToolOutcome {
	after := c.after
	files, snapshot, err := s.DB.WorkspaceReadFiles(ctx, access, w.ID, c.call.Path, c.after, 21)
	if err != nil {
		return failure(err)
	}
	if c.snapshot != "" && c.snapshot != snapshot {
		return failure(errCursor)
	}
	c.snapshot = snapshot
	if s.scanFence != nil {
		s.scanFence("queried")
	}
	entries := []entry{}
	hits := []hit{}
	seen := map[string]bool{}
	last := c.after
	for i, a := range files {
		if i == 20 {
			break
		}
		last = a.Path
		prefix := c.call.Path
		if prefix != "" {
			prefix += "/"
		}
		if !strings.HasPrefix(a.Path, prefix) {
			continue
		}
		previousDirectory := c.afterDirectory
		if c.tool == "workspace_list" {
			rest := strings.TrimPrefix(a.Path, prefix)
			if strings.Contains(rest, "/") {
				dir := prefix + strings.SplitN(rest, "/", 2)[0]
				if !seen[dir] && dir != c.afterDirectory {
					token, err := s.directoryToken(w.ID, dir, snapshot)
					if err != nil {
						return failure(err)
					}
					entries = append(entries, entry{Kind: "directory", Directory: &directoryInfo{Scope: "private", Path: dir, Revision: token}})
					c.afterDirectory = dir
					seen[dir] = true
				}
			} else {
				r, err := s.DB.ReadWorkspaceRevisionForArtifact(ctx, access, a, a.CurrentRevision)
				if err != nil {
					if errors.Is(err, runtimedb.ErrWorkspaceConflict) {
						err = errCursor
					}
					return failure(err)
				}
				file := info(a, r)
				entries = append(entries, entry{Kind: "file", File: &file})
			}
		} else {
			file, data, err := s.bytes(ctx, access, settings, w, a, a.CurrentRevision)
			if err != nil {
				return failure(err)
			}
			text := string(data)
			pos := strings.Index(text, c.call.Query)
			if pos < 0 && !strings.Contains(a.Path, c.call.Query) {
				continue
			}
			if pos < 0 {
				pos = 0
			}
			for pos > 0 && !utf8.RuneStart(data[pos]) {
				pos--
			}
			end := pos + 160
			if end > len(data) {
				end = len(data)
			}
			for end > pos && !utf8.Valid(data[pos:end]) {
				end--
			}
			hits = append(hits, hit{File: file, Snippet: text[pos:end]})
		}
		// Stop before a result could need gateway truncation. The last item
		// is retried on the next page if its metadata cannot fit here.
		probe := result(map[string]any{"items": entries, "nextCursor": selector(strings.Repeat("x", 32))})
		if c.tool == "workspace_search" {
			probe = result(map[string]any{"items": hits, "nextCursor": selector(strings.Repeat("x", 32))})
		}
		if probe.Failed {
			c.afterDirectory = previousDirectory
			if c.tool == "workspace_list" {
				entries = entries[:len(entries)-1]
			} else {
				hits = hits[:len(hits)-1]
			}
			last = c.after
			if i > 0 {
				last = files[i-1].Path
			}
			files = files[:i+1]
			break
		}
	}
	// Repeat the original scoped query once after assembling the page. This
	// checks current authority and catches changes to earlier files or derived
	// directories while later entries were processed, before publishing cursors.
	if s.scanFence != nil {
		s.scanFence("assembled")
	}
	_, currentSnapshot, err := s.DB.WorkspaceReadFiles(ctx, access, w.ID, c.call.Path, after, 21)
	if err != nil {
		return failure(err)
	}
	if currentSnapshot != snapshot {
		return failure(errCursor)
	}
	next := ""
	if len(files) > 20 || (len(files) > 0 && last != files[len(files)-1].Path) {
		if last == c.after && len(entries) == 0 && len(hits) == 0 {
			return failure(workspace.ErrUnavailable)
		}
		c.after = last
		next, err = s.save(c)
		if err != nil {
			return failure(err)
		}
	}
	if c.tool == "workspace_list" {
		return result(struct {
			Items      []entry  `json:"items"`
			NextCursor selector `json:"nextCursor,omitempty"`
		}{entries, selector(next)})
	}
	return result(struct {
		Items      []hit    `json:"items"`
		NextCursor selector `json:"nextCursor,omitempty"`
	}{hits, selector(next)})
}
