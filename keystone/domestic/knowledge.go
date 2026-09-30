package domestic

// The knowledge executors: the household actions that let an agent use
// the Markdown knowledge folder `packages/knowledge-wildstone` declares
// (ADR-W046).
//
// They follow the filedrop executors' rules — a caller never names a
// host path, the summary is the whole answer, and the capability is
// checked per call — with two differences that are the point of this
// capability.
//
// THESE DO HAND OVER BYTES. The drop box refuses to pipe household
// documents through a model; the knowledge folder exists so that an
// agent can read and keep notes. What goes in it is written for that
// audience, and ADR-W046 records the difference. What comes out is
// wrapped as untrusted data, because anybody with the web UI open can
// write a line that reads like an instruction.
//
// NO WRITE IS BLIND. write_note never overwrites, update_note must name
// the revision it read, append_note replaces nothing, and every write
// refuses content a Markdown client would run as code. There is no
// delete: removing a note is something a person does in the web UI.

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/chickenrat17/wildstone-homelab/keystone/knowledge"
	ksredact "github.com/chickenrat17/wildstone-homelab/keystone/redact"
	"github.com/chickenrat17/wildstone-homelab/keystone/runtimedb"
)

// KnowledgeCapability is the capability these executors belong to, and
// the `metadata.capability` of the package that declares them.
// TestKnowledgeToolsAreDeclaredByThePackage pins the two together.
const KnowledgeCapability = "knowledge"

// Knowledge tool names, without the capability prefix.
const (
	NoteToolList   = "list_notes"
	NoteToolSearch = "search_notes"
	NoteToolRead   = "read_note"
	NoteToolWrite  = "write_note"
	NoteToolAppend = "append_note"
	NoteToolUpdate = "update_note"
)

// KnowledgeTools maps each tool to the tier it must be declared at.
//
// Every write is `domestic`, not `confirm`, because none of them can
// lose anything: write_note refuses to overwrite, append_note keeps what
// was there, and update_note keeps the version it replaced under the
// folder's .history. A household rule may therefore keep a log without
// somebody approving each line.
var KnowledgeTools = map[string]string{
	NoteToolList:   TierRead,
	NoteToolSearch: TierRead,
	NoteToolRead:   TierRead,
	NoteToolWrite:  TierDomestic,
	NoteToolAppend: TierDomestic,
	NoteToolUpdate: TierDomestic,
}

// maxReadChunk is how much of a note one read_note answer carries. The
// Butler bounds a whole tool answer at a few kilobytes; a chunk well
// inside that keeps the untrusted envelope from ever being cut off.
const maxReadChunk = 2500

// maxListedNotes bounds one list or search answer.
const maxListedNotes = 20

// defaultListedNotes is what a call naming no limit gets.
const defaultListedNotes = 10

// Knowledge is the household knowledge folder, as the action gateway
// sees it. One instance backs every tool.
type Knowledge struct {
	Authority HouseholdExecutionAuthority
	// Root reports the knowledge folder's absolute host path, now. It is
	// a seam rather than a string for the reason Filedrop.Settings is:
	// the path lives in desired state, and an install after startup
	// must reach these tools. Empty means installed with no folder.
	Root func() (string, error)
	// Enabled reports whether the knowledge capability is installed,
	// asked per call. nil means "do not ask" (tests).
	Enabled func() (bool, error)
}

// Executors returns one registration per tool, in a stable order.
func (k *Knowledge) Executors() []Executor {
	names := []string{NoteToolAppend, NoteToolList, NoteToolRead, NoteToolSearch, NoteToolUpdate, NoteToolWrite}
	out := make([]Executor, 0, len(names))
	for _, name := range names {
		out = append(out, &knowledgeTool{k: k, tool: name})
	}
	return out
}

type knowledgeTool struct {
	k    *Knowledge
	tool string
}

func (t *knowledgeTool) Name() string { return KnowledgeCapability + "." + t.tool }

var knowledgeUsage = map[string]string{
	NoteToolList: "List the notes and folders in the household's knowledge folder. params: folder " +
		"(optional, e.g. projects; default the top level), limit (1-20, default 10).",
	NoteToolSearch: "Find notes whose name or text contains some words. params: query (required, at " +
		"least 2 characters), limit (1-20, default 10).",
	NoteToolRead: "Read one note. params: path (required, e.g. projects/garden.md), offset (optional; " +
		"use the offset a previous answer gave to read further). The answer gives the note's revision, " +
		"which update_note needs.",
	NoteToolWrite: "Create a NEW note; refuses if one exists at that path. params: path (required, e.g. " +
		"decisions/roof.md), content (required Markdown, up to 1 KB).",
	NoteToolAppend: "Add a paragraph to the end of an existing note. params: path, content (both " +
		"required; content up to 1 KB).",
	NoteToolUpdate: "Replace a note's whole text. params: path, content, revision (all required; " +
		"revision is what read_note gave). Refused if the note changed since you read it — read it again.",
}

// Usage implements UsageDescriber.
func (t *knowledgeTool) Usage() string { return knowledgeUsage[t.tool] }

func (t *knowledgeTool) Execute(ctx context.Context, c Call) (Result, error) {
	if err := t.k.installed(); err != nil {
		return Result{}, err
	}
	store, err := t.k.store(ctx)
	if err != nil {
		return Result{}, err
	}
	switch t.tool {
	case NoteToolList:
		return t.k.list(store, c)
	case NoteToolSearch:
		return t.k.search(store, c)
	case NoteToolRead:
		return t.k.read(store, c)
	case NoteToolWrite, NoteToolAppend, NoteToolUpdate:
		if err := checkExecution(ctx, t.k.Authority); err != nil {
			return Result{}, err
		}
		return t.k.write(store, t.tool, c)
	default:
		return Result{}, fmt.Errorf("knowledge has no tool %q", t.tool)
	}
}

func (k *Knowledge) installed() error {
	if k == nil || k.Enabled == nil {
		return nil
	}
	on, err := k.Enabled()
	if err != nil {
		return fmt.Errorf("checking whether the household has a knowledge folder: %w", err)
	}
	if !on {
		return fmt.Errorf("this household has no knowledge folder installed")
	}
	return nil
}

func (k *Knowledge) store(ctx context.Context) (*knowledge.Store, error) {
	if k == nil || k.Root == nil {
		return nil, errNoKnowledgeRoot
	}
	root, err := k.Root()
	if err != nil {
		return nil, fmt.Errorf("reading where the household's knowledge folder is: %w", err)
	}
	if strings.TrimSpace(root) == "" {
		return nil, errNoKnowledgeRoot
	}
	return &knowledge.Store{Root: root, BeforeWrite: func() error {
		if err := checkExecution(ctx, k.Authority); err != nil {
			return err
		}
		return runtimedb.CheckPrivacyDelivery(ctx, runtimedb.MemoryDestination{})
	}}, nil
}

var errNoKnowledgeRoot = errors.New("the household knowledge folder has no storage configured on this control plane")

// --- reads ---

func (k *Knowledge) list(store *knowledge.Store, c Call) (Result, error) {
	limit, err := noteLimit(c.Params["limit"])
	if err != nil {
		return Result{}, err
	}
	folder := strings.TrimSpace(c.Params["folder"])
	entries, err := store.List(folder)
	if err != nil {
		return Result{}, knowledgeError(err)
	}
	where := "The knowledge folder"
	if folder != "" {
		where = fmt.Sprintf("%q", strings.Trim(folder, "/"))
	}
	if len(entries) == 0 {
		return Result{Summary: where + " is empty.", AuditSummary: "listed " + where + ": empty", Detail: map[string]string{"count": "0"}}, nil
	}
	shown := entries
	if len(shown) > limit {
		shown = shown[:limit]
	}
	parts := make([]string, 0, len(shown))
	for _, e := range shown {
		if e.Dir {
			parts = append(parts, e.Path+"/ (folder)")
			continue
		}
		parts = append(parts, fmt.Sprintf("%s (%s)", e.Path, humanSize(e.Size)))
	}
	summary := fmt.Sprintf("%s holds %d: %s", where, len(entries), strings.Join(parts, "; "))
	if len(entries) > len(shown) {
		summary += fmt.Sprintf(" (first %d of %d)", len(shown), len(entries))
	}
	return Result{Summary: ksredact.Envelope(summary),
		AuditSummary: fmt.Sprintf("listed %s: %d entries", where, len(entries)),
		Detail:       map[string]string{"count": strconv.Itoa(len(entries))}}, nil
}

func (k *Knowledge) search(store *knowledge.Store, c Call) (Result, error) {
	query := strings.TrimSpace(c.Params["query"])
	limit, err := noteLimit(c.Params["limit"])
	if err != nil {
		return Result{}, err
	}
	hits, truncated, err := store.Search(query)
	if err != nil {
		return Result{}, knowledgeError(err)
	}
	if len(hits) == 0 {
		return Result{Summary: fmt.Sprintf("No note mentions %q.", query),
			AuditSummary: "searched the knowledge folder: no matches", Detail: map[string]string{"count": "0"}}, nil
	}
	shown := hits
	if len(shown) > limit {
		shown = shown[:limit]
	}
	parts := make([]string, 0, len(shown))
	for _, h := range shown {
		parts = append(parts, fmt.Sprintf("%s: %s", h.Path, h.Snippet))
	}
	summary := fmt.Sprintf("%d notes match %q:\n%s", len(hits), query, strings.Join(parts, "\n"))
	if len(hits) > len(shown) {
		summary += fmt.Sprintf("\n(first %d of %d)", len(shown), len(hits))
	}
	if truncated {
		summary += fmt.Sprintf("\n(the search stopped after %d notes)", knowledge.MaxScanFiles)
	}
	return Result{Summary: ksredact.Envelope(summary),
		AuditSummary: fmt.Sprintf("searched the knowledge folder: %d matches", len(hits)),
		Detail:       map[string]string{"count": strconv.Itoa(len(hits))}}, nil
}

func (k *Knowledge) read(store *knowledge.Store, c Call) (Result, error) {
	p := strings.TrimSpace(c.Params["path"])
	if p == "" {
		return Result{}, fmt.Errorf("param %q is required", "path")
	}
	offset := 0
	if raw := strings.TrimSpace(c.Params["offset"]); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 0 {
			return Result{}, fmt.Errorf("offset must be a whole number of at least 0, got %q", raw)
		}
		offset = n
	}
	content, note, err := store.Read(p)
	if err != nil {
		return Result{}, knowledgeError(err)
	}
	if offset > len(content) {
		return Result{}, fmt.Errorf("offset %d is past the end of %s (%d bytes)", offset, note.Path, len(content))
	}
	for offset > 0 && offset < len(content) && !utf8.RuneStart(content[offset]) {
		offset--
	}
	end := offset + maxReadChunk
	if end >= len(content) {
		end = len(content)
	} else {
		for end > offset && !utf8.RuneStart(content[end]) {
			end--
		}
	}
	head := fmt.Sprintf("%s (revision %s, %d bytes", note.Path, note.Revision, note.Size)
	if offset > 0 || end < len(content) {
		head += fmt.Sprintf(", showing bytes %d-%d", offset, end)
	}
	head += "):\n"
	// The path is inside the envelope with the text: a person names a
	// note in the web UI, so a filename can be phrased as an instruction
	// just as a line of the note can.
	summary := ksredact.Envelope(head + string(content[offset:end]))
	if end < len(content) {
		summary += fmt.Sprintf("\nMore follows: read again with offset=%d.", end)
	}
	return Result{Summary: summary, AuditSummary: "read " + head[:len(head)-2], Detail: map[string]string{
		"path": note.Path, "revision": note.Revision, "bytes": strconv.FormatInt(note.Size, 10),
	}}, nil
}

// --- writes ---

func (k *Knowledge) write(store *knowledge.Store, tool string, c Call) (Result, error) {
	p := strings.TrimSpace(c.Params["path"])
	if p == "" {
		return Result{}, fmt.Errorf("param %q is required", "path")
	}
	content := c.Params["content"]
	if strings.TrimSpace(content) == "" {
		return Result{}, fmt.Errorf("param %q is required", "content")
	}
	// A credential does not belong in a folder everybody in the house
	// and every agent can read. The same refusal `remember` gives.
	if ksredact.NamesACredential(content) {
		return Result{}, fmt.Errorf("that looks like it names a password or other credential; " +
			"those belong in the household's secrets, not in a shared note")
	}
	var (
		note knowledge.Note
		err  error
		verb string
	)
	switch tool {
	case NoteToolWrite:
		note, err = store.Create(p, content)
		verb = "Created"
	case NoteToolAppend:
		note, err = store.Append(p, content)
		verb = "Added to"
	case NoteToolUpdate:
		note, err = store.Update(p, content, c.Params["revision"])
		verb = "Updated"
	}
	if err != nil {
		return Result{}, knowledgeError(err)
	}
	summary := fmt.Sprintf("%s %s (now revision %s, %s).", verb, note.Path, note.Revision, humanSize(note.Size))
	return Result{EffectConfirmed: true, Summary: summary, AuditSummary: summary,
		Detail: map[string]string{"path": note.Path, "revision": note.Revision},
	}, nil
}

func noteLimit(raw string) (int, error) {
	v := strings.TrimSpace(raw)
	if v == "" {
		return defaultListedNotes, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 1 || n > maxListedNotes {
		return 0, fmt.Errorf("limit must be a whole number from 1 to %d, got %q", maxListedNotes, raw)
	}
	return n, nil
}

// knowledgeError turns the store's errors into sentences a model can act
// on, without ever naming a host path.
func knowledgeError(err error) error {
	var conflict *knowledge.ConflictError
	switch {
	case errors.As(err, &conflict):
		return fmt.Errorf("the note changed since you read it (it is now revision %s); read it again "+
			"and base your change on what is there now", conflict.Current)
	case errors.Is(err, knowledge.ErrNoRoot):
		return errNoKnowledgeRoot
	case errors.Is(err, knowledge.ErrPath):
		return fmt.Errorf("that is not a usable note path: use a relative path like projects/garden.md, " +
			"with no leading slash, no .. and no hidden folders")
	case errors.Is(err, knowledge.ErrNotFound):
		return fmt.Errorf("there is no such note or folder; list_notes or search_notes shows what exists")
	case errors.Is(err, knowledge.ErrExists):
		return fmt.Errorf("a note already exists there; read it and use update_note or append_note instead")
	case errors.Is(err, knowledge.ErrReserved):
		return fmt.Errorf("that path is reserved for the note app's own configuration and cannot be written by an agent")
	case errors.Is(err, knowledge.ErrConflict), errors.Is(err, knowledge.ErrContent),
		errors.Is(err, knowledge.ErrTooLarge), errors.Is(err, knowledge.ErrQuery), errors.Is(err, knowledge.ErrNotNote):
		return err
	}
	return err
}
