// Package writes adapts canonical typed workspace tools to the journaled store.
package writes

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/chickenrat17/wildstone-homelab/keystone/butler"
	"github.com/chickenrat17/wildstone-homelab/keystone/operation"
	"github.com/chickenrat17/wildstone-homelab/keystone/runtimedb"
	"github.com/chickenrat17/wildstone-homelab/keystone/workspace"
	"github.com/chickenrat17/wildstone-homelab/keystone/workspace/publication"
	"github.com/chickenrat17/wildstone-homelab/keystone/workspace/storage"
	"path"
	"strconv"
	"strings"
	"unicode/utf16"
	"unicode/utf8"
)

// Service owns one coordinator for local serialization. Settings reload current
// authoritative desired state; no request can supply owner, root or task identity.
type Service struct {
	DB          *runtimedb.DB
	Settings    func() (workspace.Settings, error)
	coordinator publication.Coordinator
}

func New(db *runtimedb.DB, settings func() (workspace.Settings, error)) *Service {
	s := &Service{DB: db, Settings: settings}
	s.coordinator = publication.Coordinator{DB: db, Settings: settings, Authorize: s.authorize}
	return s
}

type binding struct {
	task             runtimedb.ButlerTask
	agent, workspace string
	access           *runtimedb.WorkspaceAccess
}

func (s *Service) bind(ctx context.Context) (binding, error) {
	var b binding
	if s == nil || s.DB == nil || s.Settings == nil {
		return b, workspace.ErrUnavailable
	}
	id := operation.From(ctx)
	if id.TaskID == "" || id.Tool == "" || id.IdempotencyKey == "" || len(id.IdempotencyKey) > 128 || !utf8.ValidString(id.IdempotencyKey) {
		return b, publication.ErrContext
	}
	var err error
	b.task, err = s.DB.GetButlerTask(id.TaskID)
	if err != nil {
		return b, runtimedb.ErrWorkspaceDenied
	}
	b.agent, err = s.DB.TaskButler(id.TaskID)
	if err != nil {
		return b, runtimedb.ErrWorkspaceDenied
	}
	b.workspace, err = s.DB.ResolvePrivateWorkspaceTask(b.task.Owner, b.agent, id.TaskID, b.task.OriginConnection)
	if err != nil {
		return b, err
	}
	b.access = s.DB.BindWorkspaceAccess(b.task.Owner, b.agent, id.TaskID, b.task.OriginConnection)
	return b, nil
}
func (s *Service) authorize(ctx context.Context, session publication.Session) error {
	b, err := s.bind(ctx)
	if err != nil {
		return err
	}
	if b.workspace != session.WorkspaceID || b.task.Seq != session.TaskID {
		return runtimedb.ErrWorkspaceContext
	}
	_, err = s.DB.CheckWorkspaceWriteSession(ctx, b.access, session, operation.From(ctx).Tool)
	return err
}

// refs encode only verified server identities. Plain content/path strings still
// traverse the unchanged central secret-redaction and untrusted envelope pipeline.
type ref string

func (v ref) MarshalJSON() ([]byte, error) {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range utf16.Encode([]rune(v)) {
		fmt.Fprintf(&b, `\u%04x`, r)
	}
	b.WriteByte('"')
	return []byte(b.String()), nil
}

type file struct {
	ArtifactID ref    `json:"artifactId"`
	Revision   ref    `json:"revision"`
	Path       string `json:"path"`
	Scope      string `json:"scope"`
	Format     string `json:"format"`
	SizeBytes  int64  `json:"sizeBytes"`
	UpdatedAt  string `json:"updatedAt"`
}
type receipt struct {
	ID        ref    `json:"receiptId"`
	Operation string `json:"operation"`
	Artifact  struct {
		ArtifactID ref `json:"artifactId"`
		Revision   ref `json:"revision"`
	} `json:"artifact"`
}

func outcome(data any) butler.ToolOutcome {
	encoded, err := json.Marshal(struct {
		Version int `json:"version"`
		Data    any `json:"data"`
	}{1, data})
	if err != nil || len(encoded) > 3600 {
		return failure(workspace.ErrUnavailable)
	}
	return butler.ToolOutcome{Content: string(encoded), EffectConfirmed: true}
}
func failure(err error) butler.ToolOutcome {
	for _, safe := range []error{runtimedb.ErrPrivacy, runtimedb.ErrWorkspaceDenied, runtimedb.ErrWorkspaceContext, publication.ErrContext} {
		if errors.Is(err, safe) {
			return butler.ToolOutcome{Rejected: true, Content: safe.Error()}
		}
	}
	for _, safe := range []error{publication.ErrInput, publication.ErrUnavailable, runtimedb.ErrWorkspaceConflict, runtimedb.ErrWorkspaceQuota, workspace.ErrUnavailable, workspace.ErrSettings, workspace.ErrDisabled, workspace.ErrFilesystem, workspace.ErrRoot, workspace.ErrMount, storage.ErrPath, storage.ErrFormat, storage.ErrSize, storage.ErrConflict, storage.ErrIntegrity} {
		if errors.Is(err, safe) {
			return butler.ToolOutcome{Failed: true, Content: safe.Error()}
		}
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return butler.ToolOutcome{Failed: true, Content: "cancelled: workspace write interrupted"}
	}
	return butler.ToolOutcome{Failed: true, Content: workspace.ErrUnavailable.Error()}
}
func beginData(s publication.Session, next int) any {
	var expected *ref
	if s.BaseRevision != "" {
		r := ref(s.BaseRevision)
		expected = &r
	}
	return struct {
		ID       ref  `json:"writeId"`
		Expected *ref `json:"expectedRevision"`
		Next     int  `json:"nextIndex"`
	}{ref(s.ID), expected, next}
}
func (s *Service) begin(ctx context.Context, b binding, call butler.WorkspaceWriteCall, key, argumentHash string) (publication.Session, error) {
	base := ""
	if call.ExpectedRevision != nil {
		base = *call.ExpectedRevision
	}
	// A receipt must fit before creating staging for this path. Conservative
	// placeholders include escaped server IDs and the complete FileInfo fields.
	placeholder := ref(strings.Repeat("0", 32))
	artifactIdentity := placeholder
	if call.ArtifactID != "" {
		artifactIdentity = ref(call.ArtifactID)
	}
	rec := receipt{ID: placeholder, Operation: "workspace_write_commit"}
	rec.Artifact.ArtifactID = artifactIdentity
	rec.Artifact.Revision = placeholder
	probe := outcome(struct {
		Receipt receipt `json:"receipt"`
		File    file    `json:"file"`
	}{Receipt: rec, File: file{ArtifactID: artifactIdentity, Revision: placeholder, Path: call.Path, Scope: "private", Format: strings.TrimPrefix(path.Ext(call.Path), "."), SizeBytes: 1 << 62, UpdatedAt: "2000-01-01T00:00:00.999999999Z"}})
	if probe.Failed {
		return publication.Session{}, publication.ErrInput
	}
	draft := publication.Session{WorkspaceID: b.workspace, TaskID: b.task.Seq, ArtifactID: call.ArtifactID, Path: call.Path, BaseRevision: base, Privacy: "normal"}
	policy, err := s.DB.CheckWorkspaceWriteSession(ctx, b.access, draft, operation.From(ctx).Tool)
	if err != nil {
		return draft, err
	}
	privacy := "normal"
	if policy.LocalOnly {
		privacy = "local-only"
	}
	var edges []runtimedb.WorkspaceProvenance
	if base != "" {
		edges = []runtimedb.WorkspaceProvenance{{SourceRevision: base, Privacy: privacy, TrustedOnly: policy.TrustedOnly, Audience: "private"}}
	}
	return s.coordinator.Begin(ctx, publication.BeginRequest{WorkspaceID: b.workspace, ArtifactID: call.ArtifactID, Path: call.Path, ExpectedRevision: base, Key: key, TaskID: b.task.Seq, Privacy: privacy, TrustedOnly: policy.TrustedOnly, Provenance: edges, ArgumentHash: argumentHash})
}
func (s *Service) committed(ctx context.Context, b binding, r publication.Receipt, tool string) butler.ToolOutcome {
	a, err := s.DB.WorkspaceArtifact(b.workspace, r.ArtifactID)
	if err != nil {
		return failure(err)
	}
	revision, err := s.DB.ReadWorkspaceRevision(ctx, b.access, b.workspace, r.ArtifactID, r.Revision)
	if err != nil {
		return failure(err)
	}
	rec := receipt{ID: ref(r.ID), Operation: tool}
	rec.Artifact.ArtifactID = ref(r.ArtifactID)
	rec.Artifact.Revision = ref(r.Revision)
	return outcome(struct {
		Receipt receipt `json:"receipt"`
		File    file    `json:"file"`
	}{rec, file{ref(a.ID), ref(revision.ID), a.Path, "private", strings.TrimPrefix(path.Ext(a.Path), "."), revision.SizeBytes, revision.CreatedAt}})
}
func (s *Service) Call(ctx context.Context, tool string, call butler.WorkspaceWriteCall) butler.ToolOutcome {
	if operation.From(ctx).Tool != tool {
		return failure(publication.ErrContext)
	}
	b, err := s.bind(ctx)
	if err != nil {
		return failure(err)
	}
	key := operation.From(ctx).IdempotencyKey
	switch tool {
	case "workspace_write_begin":
		if call.Scope != "private" {
			return failure(publication.ErrInput)
		}
		session, err := s.begin(ctx, b, call, key, "")
		if err != nil {
			return failure(err)
		}
		chunks, err := s.DB.WorkspaceWriteChunks(session.ID)
		if err != nil {
			return failure(err)
		}
		next := 0
		for _, c := range chunks {
			if c.Index == next && c.State == "durable" {
				next++
			} else {
				break
			}
		}
		return outcome(beginData(session, next))
	case "workspace_write_chunk":
		next, err := s.coordinator.Chunk(ctx, b.workspace, call.WriteID, call.Index, call.Text, key)
		if err != nil {
			return failure(err)
		}
		return outcome(struct {
			ID   ref `json:"writeId"`
			Next int `json:"nextIndex"`
		}{ref(call.WriteID), next})
	case "workspace_write_commit":
		r, err := s.coordinator.Commit(ctx, b.workspace, call.WriteID, key)
		if err != nil {
			return failure(err)
		}
		return s.committed(ctx, b, r, tool)
	case "workspace_patch":
		return s.patch(ctx, b, call, key)
	default:
		return failure(publication.ErrInput)
	}
}
func patchKey(key string, index int) string {
	return fmt.Sprintf("patch-%x", sha256.Sum256([]byte(key+":"+strconv.Itoa(index))))
}
func (s *Service) patch(ctx context.Context, b binding, call butler.WorkspaceWriteCall, key string) butler.ToolOutcome {
	if call.OldText == "" || len(call.OldText)+len(call.NewText) > 1024 || !utf8.ValidString(call.OldText+call.NewText) {
		return failure(publication.ErrInput)
	}
	a, err := s.DB.WorkspaceArtifact(b.workspace, call.ArtifactID)
	if err != nil {
		return failure(runtimedb.ErrWorkspaceDenied)
	}
	draft := publication.Session{WorkspaceID: b.workspace, TaskID: b.task.Seq, ArtifactID: call.ArtifactID, Path: a.Path, BaseRevision: call.Revision, Privacy: "normal"}
	if _, err = s.DB.CheckWorkspaceWriteSession(ctx, b.access, draft, "workspace_patch"); err != nil {
		return failure(err)
	}
	// Begin's durable key is checked before reading/replacing bytes. A completed
	// patch reuses its exact original session/receipt even after the base advanced.
	encoded, _ := json.Marshal(call)
	argumentHash := fmt.Sprintf("%x", sha256.Sum256(encoded))
	session, err := s.begin(ctx, b, butler.WorkspaceWriteCall{Scope: "private", Path: a.Path, ArtifactID: a.ID, ExpectedRevision: &call.Revision}, patchKey(key, -1), argumentHash)
	if err != nil {
		return failure(err)
	}
	if session.State == "open" {
		r, err := s.DB.ReadWorkspaceRevision(ctx, b.access, b.workspace, a.ID, call.Revision)
		if err != nil {
			return failure(err)
		}
		w, err := s.DB.WorkspaceByID(b.workspace)
		if err != nil {
			return failure(err)
		}
		settings, err := s.Settings()
		if err != nil {
			return failure(err)
		}
		st, err := storage.New(settings, w.PersonID, strconv.FormatInt(w.AgentID, 10))
		if err != nil {
			return failure(err)
		}
		data, err := st.Read(storage.Object{ID: r.ID, SHA256: r.Hash, Size: r.SizeBytes})
		if err != nil {
			return failure(err)
		}
		text := string(data)
		first := strings.Index(text, call.OldText)
		if first < 0 || strings.LastIndex(text, call.OldText) != first {
			return failure(runtimedb.ErrWorkspaceConflict)
		}
		updated := text[:first] + call.NewText + text[first+len(call.OldText):]
		index := 0
		for len(updated) > 0 {
			end := len(updated)
			if end > 1024 {
				end = 1024
			}
			for end > 0 && !utf8.ValidString(updated[:end]) {
				end--
			}
			if end == 0 {
				return failure(storage.ErrFormat)
			}
			if _, err = s.coordinator.Chunk(ctx, b.workspace, session.ID, index, updated[:end], patchKey(key, index)); err != nil {
				return failure(err)
			}
			updated = updated[end:]
			index++
		}
	}
	r, err := s.coordinator.Commit(ctx, b.workspace, session.ID, patchKey(key, -2))
	if err != nil {
		return failure(err)
	}
	return s.committed(ctx, b, r, "workspace_patch")
}
