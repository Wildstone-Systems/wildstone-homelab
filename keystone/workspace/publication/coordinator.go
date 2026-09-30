// Package publication coordinates ADR-W047 journaled workspace writes. It is
// dormant platform machinery, not a tool or authority resolver. A database
// transaction never covers filesystem I/O; durable journal states bridge them.
package publication

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"path"
	"strconv"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/chickenrat17/wildstone-homelab/keystone/runtimedb"
	"github.com/chickenrat17/wildstone-homelab/keystone/workspace"
	"github.com/chickenrat17/wildstone-homelab/keystone/workspace/storage"
)

var (
	ErrInput       = errors.New("invalid_input: invalid workspace write request")
	ErrContext     = errors.New("context_missing: workspace write requires current authority")
	ErrUnavailable = errors.New("not_found: workspace write session unavailable")
)

type Session = runtimedb.WorkspaceWriteSession

// Authority is a mandatory current resource/provenance check, called on every
// operation and retry, again at byte publication and before metadata commit.
// It must bind the originating task, Person, Agent, surface and write grant.
// Settings must reload current authoritative configuration each time.
type Coordinator struct {
	DB        *runtimedb.DB
	Settings  func() (workspace.Settings, error)
	Authorize func(context.Context, Session) error
	locks     [64]sync.Mutex
	// Fault injection stays private to package tests.
	fault func(string) error
}
type BeginRequest struct {
	WorkspaceID, ArtifactID, Path, ExpectedRevision, Key string
	TaskID                                               int64
	Privacy                                              string
	TrustedOnly                                          bool
	Provenance                                           []runtimedb.WorkspaceProvenance
}
type Receipt struct{ ID, Operation, ArtifactID, Revision string }

func randomID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}
func hash(data []byte) string  { s := sha256.Sum256(data); return hex.EncodeToString(s[:]) }
func validKey(key string) bool { return len(key) > 0 && len(key) <= 128 && utf8.ValidString(key) }
func (c *Coordinator) lock(id string) func() {
	sum := sha256.Sum256([]byte(id))
	m := &c.locks[int(sum[0])%len(c.locks)]
	m.Lock()
	return m.Unlock
}
func (c *Coordinator) check(ctx context.Context, s Session) (workspace.Settings, error) {
	if err := ctx.Err(); err != nil {
		return workspace.Settings{}, err
	}
	if c.DB == nil || c.Settings == nil || c.Authorize == nil {
		return workspace.Settings{}, ErrContext
	}
	if err := c.DB.CheckHouseholdExecution(); err != nil {
		return workspace.Settings{}, err
	}
	if err := c.Authorize(ctx, s); err != nil {
		return workspace.Settings{}, err
	}
	settings, err := c.Settings()
	if err != nil {
		return settings, err
	}
	if err = settings.Validate(); err != nil {
		return settings, err
	}
	if settings.Revision == "" || (s.ConfigRevision != "" && settings.Revision != s.ConfigRevision) {
		return settings, workspace.ErrSettings
	}
	return settings, nil
}
func (c *Coordinator) store(s Session, settings workspace.Settings) (*storage.Store, error) {
	w, err := c.DB.WorkspaceByID(s.WorkspaceID)
	if err != nil {
		return nil, err
	}
	return storage.New(settings, w.PersonID, strconv.FormatInt(w.AgentID, 10))
}
func (c *Coordinator) fence(ctx context.Context, s Session) func() error {
	return func() error { _, err := c.check(ctx, s); return err }
}
func (c *Coordinator) inject(point string) error {
	if c.fault != nil {
		return c.fault(point)
	}
	return nil
}
func (c *Coordinator) Begin(ctx context.Context, r BeginRequest) (Session, error) {
	var s Session
	if !validKey(r.Key) || r.TaskID <= 0 || r.WorkspaceID == "" || (r.ArtifactID == "") != (r.ExpectedRevision == "") || len(r.Provenance) > 128 {
		return s, ErrInput
	}
	if err := storage.ValidatePath(r.Path, false); err != nil {
		return s, err
	}
	if r.Privacy != "normal" && r.Privacy != "local-only" {
		return s, ErrInput
	}
	s = Session{WorkspaceID: r.WorkspaceID, TaskID: r.TaskID, ArtifactID: r.ArtifactID, Path: r.Path, BaseRevision: r.ExpectedRevision, BeginKey: r.Key, Privacy: r.Privacy, TrustedOnly: r.TrustedOnly, Provenance: r.Provenance}
	settings, err := c.check(ctx, s)
	if err != nil {
		return s, err
	}
	s.ConfigRevision = settings.Revision
	s.MaxBytes = settings.MaxFileBytes
	// Hash trusted arguments before generating identities so exact begin retries
	// return the original session. Current authority is checked before reuse.
	encoded, err := json.Marshal(r)
	if err != nil {
		return s, err
	}
	s.RequestHash = hash(encoded)
	s.ID, err = randomID()
	if err != nil {
		return s, err
	}
	if s.ArtifactID == "" {
		s.ArtifactID, err = randomID()
		if err != nil {
			return s, err
		}
	}
	s, err = c.DB.BeginWorkspaceWrite(s)
	if err != nil {
		return s, err
	}
	if _, err = c.check(ctx, s); err != nil {
		return Session{}, err
	}
	if s.State == "aborted" {
		return Session{}, ErrUnavailable
	}
	return s, nil
}
func (c *Coordinator) session(ctx context.Context, workspaceID, id string) (Session, *storage.Store, error) {
	if c.DB == nil {
		return Session{}, nil, ErrContext
	}
	s, err := c.DB.WorkspaceWriteSession(workspaceID, id)
	if err != nil {
		return s, nil, ErrUnavailable
	}
	settings, err := c.check(ctx, s)
	if err != nil {
		return s, nil, err
	}
	if s.State == "aborted" {
		return s, nil, ErrUnavailable
	}
	st, err := c.store(s, settings)
	return s, st, err
}
func object(ch runtimedb.WorkspaceWriteChunk) storage.Object {
	return storage.Object{ID: ch.ObjectID, SHA256: ch.Hash, Size: ch.SizeBytes}
}
func (c *Coordinator) Chunk(ctx context.Context, workspaceID, id string, index int, text string) (int, error) {
	if index < 0 || index >= 10240 || len(text) > 1024 || !utf8.ValidString(text) || strings.IndexByte(text, 0) >= 0 {
		return 0, ErrInput
	}
	unlock := c.lock(id)
	defer unlock()
	s, st, err := c.session(ctx, workspaceID, id)
	if err != nil {
		return 0, err
	}
	if s.State != "open" {
		return 0, runtimedb.ErrWorkspaceConflict
	}
	oid, err := randomID()
	if err != nil {
		return 0, err
	}
	ch, err := c.DB.JournalWorkspaceChunk(s, runtimedb.WorkspaceWriteChunk{Index: index, ObjectID: oid, Hash: hash([]byte(text)), SizeBytes: int64(len(text)), ReservationID: oid})
	if err != nil {
		return 0, err
	}
	fence := c.fence(ctx, s)
	if ch.State != "durable" {
		if err = c.inject("chunk_journaled"); err != nil {
			return 0, err
		}
		err = st.StageObject(s.Path, object(ch), strings.NewReader(text), fence)
		if errors.Is(err, storage.ErrConflict) {
			_, err = st.SyncStage(object(ch), fence)
		}
		if err != nil {
			return 0, err
		}
		if err = c.inject("chunk_staged"); err != nil {
			return 0, err
		}
		if err = fence(); err != nil {
			return 0, err
		}
		if err = c.DB.MarkWorkspaceChunkDurable(id, index); err != nil {
			return 0, err
		}
	} else {
		if _, err = st.SyncStage(object(ch), fence); err != nil {
			return 0, err
		}
	}
	return index + 1, nil
}
func mediaType(p string) string {
	switch path.Ext(p) {
	case ".md":
		return "text/markdown"
	case ".json":
		return "application/json"
	case ".csv":
		return "text/csv"
	}
	return "text/plain"
}
func (c *Coordinator) assemble(ctx context.Context, s Session, st *storage.Store) ([]byte, int, error) {
	chunks, err := c.DB.WorkspaceWriteChunks(s.ID)
	if err != nil {
		return nil, 0, err
	}
	var out []byte
	for i, ch := range chunks {
		if ch.Index != i || ch.State != "durable" {
			return nil, 0, runtimedb.ErrWorkspaceConflict
		}
		data, err := st.SyncStage(object(ch), c.fence(ctx, s))
		if err != nil {
			return nil, 0, err
		}
		if int64(len(out))+int64(len(data)) > s.MaxBytes {
			return nil, 0, storage.ErrSize
		}
		out = append(out, data...)
	}
	return out, len(chunks), nil
}

// Commit is an authorized retry/recovery entrypoint. It uses the original
// object and receipt identities across every crash boundary. Unknown, missing
// or corrupt bytes never advance the journal or become a visible revision.
func (c *Coordinator) Commit(ctx context.Context, workspaceID, id, key string) (Receipt, error) {
	var receipt Receipt
	if !validKey(key) {
		return receipt, ErrInput
	}
	unlock := c.lock(id)
	defer unlock()
	s, st, err := c.session(ctx, workspaceID, id)
	if err != nil {
		return receipt, err
	}
	fence := c.fence(ctx, s)
	var data []byte
	var count int
	if s.State == "open" {
		data, count, err = c.assemble(ctx, s, st)
		if err != nil {
			return receipt, err
		}
	}
	if err = fence(); err != nil {
		return receipt, err
	}
	p, err := c.DB.SealWorkspaceWrite(s, hash(data), mediaType(s.Path), key, count, int64(len(data)))
	if err != nil {
		return receipt, err
	}
	if err = c.inject("sealed"); err != nil {
		return receipt, err
	}
	r, err := c.DB.WorkspaceWriteObject(s.WorkspaceID, s.ID)
	if err != nil {
		return receipt, err
	}
	obj := storage.Object{ID: r.ID, SHA256: r.Hash, Size: r.SizeBytes}
	if p.State == "staging" {
		// If sealing survived a restart/retry, reconstruct only the exact sealed
		// complete object from durable indexed chunks and compare its trusted hash.
		if data == nil {
			data, _, err = c.assemble(ctx, s, st)
			if err != nil {
				return receipt, err
			}
		}
		err = st.StageObject(s.Path, obj, strings.NewReader(string(data)), fence)
		if errors.Is(err, storage.ErrConflict) {
			_, err = st.SyncStage(obj, fence)
		}
		if err != nil {
			return receipt, err
		}
		if err = c.inject("assembled"); err != nil {
			return receipt, err
		}
		if err = fence(); err != nil {
			return receipt, err
		}
		if err = c.DB.AdvanceWorkspaceWrite(s.WorkspaceID, s.ID, "prepared"); err != nil {
			return receipt, err
		}
		p.State = "prepared"
	}
	if p.State == "prepared" {
		if err = c.inject("before_publish"); err != nil {
			return receipt, err
		}
		err = st.Publish(obj, fence)
		if errors.Is(err, storage.ErrConflict) {
			_, err = st.SyncPublished(obj, fence)
		}
		if err != nil {
			return receipt, err
		}
		if err = c.inject("after_publish"); err != nil {
			return receipt, err
		}
		if err = fence(); err != nil {
			return receipt, err
		}
		if err = c.DB.AdvanceWorkspaceWrite(s.WorkspaceID, s.ID, "bytes_published"); err != nil {
			return receipt, err
		}
		p.State = "bytes_published"
	}
	if p.State == "bytes_published" {
		if _, err = st.SyncPublished(obj, fence); err != nil {
			return receipt, err
		}
		if err = c.inject("before_commit"); err != nil {
			return receipt, err
		}
		if err = fence(); err != nil {
			return receipt, err
		}
		if err = c.DB.CommitWorkspaceWrite(s.WorkspaceID, s.ID); err != nil {
			return receipt, err
		}
		if err = c.inject("after_commit"); err != nil {
			return receipt, err
		}
	} else if p.State != "committed" {
		return receipt, ErrUnavailable
	}
	if err = fence(); err != nil {
		return receipt, err
	}
	rid, err := c.DB.WorkspaceReceipt(s.WorkspaceID, s.ID)
	if err != nil {
		return receipt, err
	}
	return Receipt{ID: rid, Operation: p.Operation, ArtifactID: p.ArtifactID, Revision: p.RevisionID}, nil
}
