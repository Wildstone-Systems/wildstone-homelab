package butlerstore

// The opaque task handle (ADR-W039 §3.1 principle 4, §3.4 steps 1–2,
// §4 item 2; Phase 2a).
//
// A runtime carries one of these on every capability, memory and work
// call, and Wildstone derives everything with authority — the Person,
// the Connection, the audience, the Butler — from it at the moment of
// the call. The runtime never asserts any of those; it presents the
// bearer it was issued when the task was admitted, and the task row
// says the rest.
//
// The handle is `<taskID>.<secret>`. The task id is public: it is what
// audit rows, trace lines and refusals name. The secret is 32 random
// bytes, and only its sha256 digest is stored (migration 0082), so a
// database read cannot mint one and a log line cannot leak one — the
// agentapi.Handle type's String redacts it, and every error below is
// worded without it.
//
// One live handle per task. Issuing replaces the previous one, which
// is how a resumed turn gets a fresh bearer and an abandoned one stops
// working; withdrawing clears it, which is what revocation does to
// every open task of a revoked runtime (FenceTask).

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"github.com/Wildstone-Systems/wildstone-homelab/keystone/agentapi"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/runtimedb"
)

// ErrHandleRefused is the one error every handle refusal wraps. One
// error for every cause — unknown task, wrong secret, withdrawn,
// finished, fenced — because the caller's response is the same (stop)
// and because a refusal that said WHICH check failed would be an
// oracle for guessing the ones that passed. The wrapped message says
// enough for an operator reading a log, never the secret.
var ErrHandleRefused = errors.New("butlerstore: the task handle was refused")

// handleSecretBytes is the entropy behind one handle. 32 bytes is the
// digest's own width; more would be hashed down to it.
const handleSecretBytes = 32

// HandleContext is what a resolved handle entitles: the task and the
// facts of its admission that every gateway decision derives from. It
// is read from the task row at the moment of resolution, never cached,
// so an offboarding, a cancellation or a Butler retirement is visible
// on the next call (design §10.1: re-evaluated at every boundary).
type HandleContext struct {
	// Task is the row itself, in the state it was read.
	Task runtimedb.ButlerTask
	// Butler is the Butler the task was snapshotted to run as (A4). The
	// full record, because the gateway builds the surface from it —
	// which surface, which class, which directive.
	Butler runtimedb.Butler
	// Person is the task's current owner: whose authority the work
	// runs under, and the only Person a call may name.
	Person string
	// OriginConnection and OriginConnectionRef are the Connection the
	// task was admitted over (§10.1). Empty and zero mean the leg does
	// not apply.
	OriginConnection    string
	OriginConnectionRef int64
	// Audience is who the task's output may be disclosed to.
	Audience runtimedb.ButlerTaskAudience
	// Surface and AgentClass are the task's own snapshot of the
	// Butler's, kept separately because the task carries them
	// separately: they are what the task was admitted under.
	Surface    runtimedb.ButlerSurface
	AgentClass string
	// AsOwner and SystemExtra are the request-time facts migration 0083
	// recorded on the task at open (ADR-W039 Phase 2b, #337): what the
	// caller was when the turn that opened it was admitted, mirroring
	// butlerlive.Turn.AsOwner/SystemExtra. Not derivable from anything
	// else on the handle or the Butler.
	AsOwner     bool
	SystemExtra string
}

// IssueHandle mints the task's live handle, replacing any earlier one.
//
// It refuses a task that can no longer do work — completed, failed or
// cancelled — because a bearer for finished work would only ever be
// presented by mistake or by something that should not have it. A
// pending, running or waiting task is issued one; so is a
// paused-offboarded task, since a review that resumes it will need
// one and the ledger's Authorize refuses every call until then.
func (l *Ledger) IssueHandle(taskID string) (agentapi.Handle, error) {
	task, err := l.db.GetButlerTask(taskID)
	if err != nil {
		return "", err
	}
	switch task.State {
	case runtimedb.ButlerTaskCompleted, runtimedb.ButlerTaskFailed, runtimedb.ButlerTaskCancelled:
		return "", fmt.Errorf("butlerstore: %s already finished as %s and cannot be issued a handle", taskID, task.State)
	}
	raw := make([]byte, handleSecretBytes)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("butlerstore: generating a task handle: %w", err)
	}
	secret := base64.RawURLEncoding.EncodeToString(raw)
	if err := l.db.SetButlerTaskHandleHash(taskID, digest(secret)); err != nil {
		return "", err
	}
	return agentapi.Handle(taskID + "." + secret), nil
}

// WithdrawHandle clears the task's live handle. Every later call that
// presents it is refused; a resume issues a new one.
func (l *Ledger) WithdrawHandle(taskID string) error {
	return l.db.SetButlerTaskHandleHash(taskID, "")
}

// FenceTask is what revocation does to one open task (ADR-W039 §3.7):
// every in-flight attempt is fenced so a late result is refused, and
// the handle is withdrawn so the next call is too. It returns the new
// lease epoch.
//
// It is deliberately NOT what a stalled model call does. FenceAttempts
// alone moves the epoch and leaves the handle alone, because the same
// runtime is about to retry under the same authority; this withdraws
// the bearer, because the runtime that held it is no longer trusted.
func (l *Ledger) FenceTask(taskID string) (int64, error) {
	epoch, err := l.db.FenceButlerTaskAttempts(taskID)
	if err != nil {
		return 0, err
	}
	if err := l.WithdrawHandle(taskID); err != nil {
		return 0, err
	}
	return epoch, nil
}

// ResolveHandle checks a presented handle and returns what it entitles.
//
// Every failure wraps ErrHandleRefused. The secret is compared in
// constant time against the stored digest; a task with no digest —
// never issued, or withdrawn by FenceTask — refuses every presentation.
// A task that has finished, was cancelled, or is paused for review
// refuses too: Ledger.Authorize would say the same one call later, but
// a handle is the first thing a gateway touches and refusing here
// keeps a finished task's material from being assembled for nothing.
//
// The Butler is resolved by the task's own snapshot, never by anything
// in the request, and a retired Butler refuses: nothing may act for a
// retired Agent (ADR-W031 A6).
func (l *Ledger) ResolveHandle(h agentapi.Handle) (HandleContext, error) {
	if h.IsZero() {
		return HandleContext{}, fmt.Errorf("%w: no handle was presented", ErrHandleRefused)
	}
	taskID, secret, ok := strings.Cut(string(h), ".")
	if !ok || taskID == "" || secret == "" {
		return HandleContext{}, fmt.Errorf("%w: the handle is malformed", ErrHandleRefused)
	}
	stored, err := l.db.ButlerTaskHandleHash(taskID)
	if errors.Is(err, runtimedb.ErrNotFound) {
		return HandleContext{}, fmt.Errorf("%w: %s names no task", ErrHandleRefused, taskID)
	}
	if err != nil {
		return HandleContext{}, err
	}
	// The comparison runs even when nothing is stored, against a digest
	// that cannot match, so a task without a handle costs the same as
	// one with the wrong secret.
	want := stored
	if want == "" {
		want = strings.Repeat("0", sha256.Size*2)
	}
	if subtle.ConstantTimeCompare([]byte(digest(secret)), []byte(want)) != 1 || stored == "" {
		return HandleContext{}, fmt.Errorf("%w: %s did not accept it", ErrHandleRefused, taskID)
	}
	task, err := l.db.GetButlerTask(taskID)
	if err != nil {
		return HandleContext{}, err
	}
	switch task.State {
	case runtimedb.ButlerTaskCompleted, runtimedb.ButlerTaskFailed, runtimedb.ButlerTaskCancelled:
		return HandleContext{}, fmt.Errorf("%w: %s already finished as %s", ErrHandleRefused, taskID, task.State)
	case runtimedb.ButlerTaskPausedOffboarded:
		return HandleContext{}, fmt.Errorf("%w: %s is paused for review after its owner was offboarded", ErrHandleRefused, taskID)
	}
	name, err := l.db.TaskButler(taskID)
	if err != nil {
		return HandleContext{}, err
	}
	if name == "" {
		return HandleContext{}, fmt.Errorf("%w: %s was not snapshotted to a butler", ErrHandleRefused, taskID)
	}
	b, err := l.db.GetButler(name)
	if errors.Is(err, runtimedb.ErrNotFound) {
		return HandleContext{}, fmt.Errorf("%w: %s runs as %s, which no longer exists", ErrHandleRefused, taskID, name)
	}
	if err != nil {
		return HandleContext{}, err
	}
	if !b.RetiredAt.IsZero() {
		return HandleContext{}, fmt.Errorf("%w: %s runs as %s, which is retired", ErrHandleRefused, taskID, name)
	}
	return HandleContext{
		Task:                task,
		Butler:              b,
		Person:              task.Owner,
		OriginConnection:    task.OriginConnection,
		OriginConnectionRef: task.OriginConnectionRef,
		Audience:            task.Audience,
		Surface:             task.Surface,
		AgentClass:          task.AgentClass,
		AsOwner:             task.AsOwner,
		SystemExtra:         task.SystemExtra,
	}, nil
}

// digest is the stored form of a secret: hex sha256, fixed width so the
// constant-time compare has equal-length inputs.
func digest(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])
}
