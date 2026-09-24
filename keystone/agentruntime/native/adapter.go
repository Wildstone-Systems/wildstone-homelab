// Package native is the NativeAgentAdapter: Wildstone's own agent
// runtime, with its reasoning loop delegated to the OpenAI Agents SDK
// (issue #357).
//
// The SDK runs in a TypeScript sidecar (runtimes/native-agent), one
// process per run, spoken to over stdin/stdout (protocol.go). The split
// is the platform/framework split:
//
//   - The sidecar owns the model → tool → observation loop and nothing
//     else. It holds no tool: every call it wants comes back here.
//   - This adapter owns everything Wildstone must not delegate:
//     authorization (through agentruntime.ToolHost), the audit trail,
//     the hard timeouts, cancellation, the budgets and the explicit
//     BLOCKED / WAITING_FOR_USER states. None of it depends on the
//     sidecar or the model behaving. A sidecar that hangs, loops, or
//     dies is killed and the job still ends in a definite state.
package native

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/chickenrat17/wildstone-homelab/keystone/agentruntime"
)

// RuntimeName is this adapter's name in configuration and audit.
const RuntimeName = "native"

// engineName is what runs the loop, for Capabilities.
const engineName = "openai-agents-sdk"

// APIKeyEnv is the variable the sidecar reads its model API key from.
// The key is never written into the protocol stream, where it could end
// up in a log.
const APIKeyEnv = "WILDSTONE_AGENT_API_KEY"

// controlGuidance is appended to every job's instructions. It is the
// model-facing half of "never reason indefinitely": the other half is
// the timers and budgets in drive.
const controlGuidance = `

When you cannot finish, do not keep trying the same thing.
- If you need information only the person can give, call ask_user with one clear question.
- If you are stuck (a tool keeps failing, you lack permission, or there is no sensible next step), call report_blocked with the reason.
Otherwise, answer the person directly.`

// maxLine bounds one protocol message. The history can be large, but not
// unboundedly: a run whose history outgrows this fails rather than
// making the adapter hold an arbitrary amount of memory.
const maxLine = 8 << 20

// maxText bounds the output, question and reason kept in a Status.
const maxText = 64 << 10

// DefaultInstallDir is where install.sh puts the release's
// native-agent_linux_<arch>.tar.gz: a Node runtime at bin/node and the
// bundled sidecar at main.mjs (scripts/build-native-agent.sh).
const DefaultInstallDir = "/usr/lib/wildstone/native-agent"

// InstalledCommand is the Config.Command for a runtime installed from a
// release package into dir.
func InstalledCommand(dir string) []string {
	return []string{filepath.Join(dir, "bin", "node"), filepath.Join(dir, "main.mjs")}
}

// Model configures which model the sidecar talks to. It stays behind
// this adapter: nothing in the AgentRuntime contract names a provider.
type Model struct {
	// BaseURL is an OpenAI-compatible endpoint. Empty means the SDK's
	// default (api.openai.com).
	BaseURL string
	// Name is the model name.
	Name string
	// APIKey is passed to the sidecar through its environment only.
	APIKey string
	// API is "chat_completions" (default; works with Ollama and other
	// OpenAI-compatible servers) or "responses".
	API string
}

// Config configures the adapter.
type Config struct {
	// Command starts the sidecar, e.g.
	// {"node", "/usr/lib/wildstone/native-agent/dist/main.js"}.
	Command []string
	Model   Model
	// Limits apply to jobs that leave Job.Limits zero. Zero fields take
	// agentruntime.DefaultLimits.
	Limits agentruntime.Limits
	// Env is extra environment for the sidecar ("K=V"). The sidecar does
	// NOT inherit Keystone's environment, which holds secrets it has no
	// business seeing.
	Env []string
	// RetainFinished is how long a terminal job stays queryable.
	// Default one hour.
	RetainFinished time.Duration
	// scriptedModel lets tests drive a real sidecar with a scripted
	// model. Unexported: production cannot set it.
	scriptedModel json.RawMessage
}

// Adapter is the NativeAgentAdapter. It implements
// agentruntime.AgentRuntime.
type Adapter struct {
	cfg Config

	mu   sync.Mutex
	jobs map[string]*job

	healthMu sync.Mutex
	health   agentruntime.Health
	version  string
}

var _ agentruntime.AgentRuntime = (*Adapter)(nil)

type job struct {
	spec    agentruntime.Job
	limits  agentruntime.Limits
	guard   *agentruntime.Guard
	status  agentruntime.Status
	history json.RawMessage
	// cancelRun stops the current run; cancelling records that a person
	// asked for it, so the run ends CANCELLED rather than FAILED.
	cancelRun  context.CancelFunc
	cancelling bool
	// settled is closed when the job reaches a Settled state, and
	// replaced when a Resume starts it moving again.
	settled chan struct{}
}

// New returns an adapter. It does not start anything.
func New(cfg Config) (*Adapter, error) {
	if len(cfg.Command) == 0 || cfg.Command[0] == "" {
		return nil, errors.New("native: sidecar command is required")
	}
	if cfg.RetainFinished <= 0 {
		cfg.RetainFinished = time.Hour
	}
	return &Adapter{cfg: cfg, jobs: map[string]*job{}}, nil
}

func (a *Adapter) Name() string { return RuntimeName }

// Start accepts a job and runs it in the background.
func (a *Adapter) Start(ctx context.Context, spec agentruntime.Job) (agentruntime.Status, error) {
	if err := validate(spec); err != nil {
		return agentruntime.Status{}, err
	}
	limits := spec.Limits
	if limits == (agentruntime.Limits{}) {
		limits = a.cfg.Limits
	}
	limits = limits.WithDefaults()
	now := time.Now()

	a.mu.Lock()
	a.pruneLocked(now)
	if _, dup := a.jobs[spec.ID]; dup {
		a.mu.Unlock()
		return agentruntime.Status{}, fmt.Errorf("%w: %s", agentruntime.ErrDuplicateJob, spec.ID)
	}
	j := &job{
		spec:    spec,
		limits:  limits,
		guard:   agentruntime.NewGuard(limits),
		settled: make(chan struct{}),
		status: agentruntime.Status{
			JobID: spec.ID, Runtime: RuntimeName, State: agentruntime.StateQueued,
			CreatedAt: now, UpdatedAt: now,
		},
	}
	a.jobs[spec.ID] = j
	runCtx, stop := a.prepareLocked(j)
	st := j.status
	a.mu.Unlock()

	if err := a.record(ctx, j, agentruntime.AuditEvent{Type: agentruntime.AuditJobState, State: agentruntime.StateQueued}); err != nil {
		a.mu.Lock()
		j.cancelRun()
		delete(a.jobs, spec.ID)
		a.mu.Unlock()
		return agentruntime.Status{}, fmt.Errorf("native: audit unavailable, job not started: %w", err)
	}
	a.launch(runCtx, stop, j, spec.Input, nil)
	return st, nil
}

// Resume answers a job parked in WAITING_FOR_USER or BLOCKED and starts
// it moving again, with its conversation so far.
func (a *Adapter) Resume(ctx context.Context, jobID string, ev agentruntime.Event) (agentruntime.Status, error) {
	if ev.Kind != agentruntime.EventUserReply || strings.TrimSpace(ev.Text) == "" {
		return agentruntime.Status{}, fmt.Errorf("%w: resume needs a non-empty user reply", agentruntime.ErrInvalidJob)
	}
	a.mu.Lock()
	j, ok := a.jobs[jobID]
	if !ok {
		a.mu.Unlock()
		return agentruntime.Status{}, fmt.Errorf("%w: %s", agentruntime.ErrUnknownJob, jobID)
	}
	if !j.status.State.Resumable() {
		st := j.status
		a.mu.Unlock()
		return st, fmt.Errorf("%w: %s is %s", agentruntime.ErrNotResumable, jobID, st.State)
	}
	history := j.history
	j.guard.NewRun()
	j.settled = make(chan struct{})
	j.cancelling = false
	j.status.State = agentruntime.StateQueued
	j.status.Reason, j.status.Question, j.status.Output = "", "", ""
	j.status.Turns = 0
	j.status.FinishedAt = time.Time{}
	j.status.UpdatedAt = time.Now()
	runCtx, stop := a.prepareLocked(j)
	st := j.status
	a.mu.Unlock()

	a.launch(runCtx, stop, j, ev.Text, history)
	return st, nil
}

// Cancel stops a job. A settled, non-terminal job is cancelled at once;
// a moving one is cancelled at its next step (the sidecar is killed, so
// "next step" is immediate unless a tool is mid-call). Cancelling a
// terminal job is a no-op that returns its status.
func (a *Adapter) Cancel(ctx context.Context, jobID string) (agentruntime.Status, error) {
	a.mu.Lock()
	j, ok := a.jobs[jobID]
	if !ok {
		a.mu.Unlock()
		return agentruntime.Status{}, fmt.Errorf("%w: %s", agentruntime.ErrUnknownJob, jobID)
	}
	switch {
	case j.status.State.Terminal():
		st := j.status
		a.mu.Unlock()
		return st, nil
	case j.status.State.Settled():
		// Parked: nothing is running to stop.
		a.mu.Unlock()
		a.settle(j, agentruntime.StateCancelled, "cancelled", "", "", nil)
	default:
		// cancelRun is always set before a job leaves the lock in a
		// moving state (prepareLocked), so there is no window in which
		// a cancel can miss the run it meant to stop.
		j.cancelling = true
		j.cancelRun()
		a.mu.Unlock()
	}
	return a.Status(ctx, jobID)
}

func (a *Adapter) Status(_ context.Context, jobID string) (agentruntime.Status, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	j, ok := a.jobs[jobID]
	if !ok {
		return agentruntime.Status{}, fmt.Errorf("%w: %s", agentruntime.ErrUnknownJob, jobID)
	}
	// The guard belongs to the run goroutine; its counters reach the
	// status through touch and settle, under the lock.
	return j.status, nil
}

// Wait blocks until the job is Settled or ctx ends.
func (a *Adapter) Wait(ctx context.Context, jobID string) (agentruntime.Status, error) {
	a.mu.Lock()
	j, ok := a.jobs[jobID]
	if !ok {
		a.mu.Unlock()
		return agentruntime.Status{}, fmt.Errorf("%w: %s", agentruntime.ErrUnknownJob, jobID)
	}
	ch := j.settled
	a.mu.Unlock()
	select {
	case <-ch:
		return a.Status(ctx, jobID)
	case <-ctx.Done():
		st, _ := a.Status(ctx, jobID)
		return st, ctx.Err()
	}
}

func (a *Adapter) Capabilities(ctx context.Context) (agentruntime.Capabilities, error) {
	a.Health(ctx)
	a.healthMu.Lock()
	v := a.version
	a.healthMu.Unlock()
	return agentruntime.Capabilities{
		Runtime: RuntimeName, Engine: engineName, EngineVersion: v,
		ToolCalling: true, Resume: true, Cancel: true,
	}, nil
}

// Health runs the sidecar's self-check (it loads the SDK and reports its
// version, without calling a model). The answer is cached briefly.
func (a *Adapter) Health(ctx context.Context) agentruntime.Health {
	a.healthMu.Lock()
	defer a.healthMu.Unlock()
	if !a.health.CheckedAt.IsZero() && time.Since(a.health.CheckedAt) < 30*time.Second {
		return a.health
	}
	h := agentruntime.Health{CheckedAt: time.Now()}
	cctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	args := append(append([]string{}, a.cfg.Command[1:]...), "--health")
	cmd := exec.CommandContext(cctx, a.cfg.Command[0], args...)
	cmd.Env = a.childEnv()
	out, err := cmd.Output()
	var m message
	switch {
	case err != nil:
		h.Detail = "sidecar self-check failed: " + err.Error()
	case json.Unmarshal(out, &m) != nil || m.Type != msgReady:
		h.Detail = "sidecar self-check returned an unexpected answer"
	case m.Protocol != ProtocolVersion:
		h.Detail = fmt.Sprintf("sidecar speaks protocol %d, keystone speaks %d", m.Protocol, ProtocolVersion)
	default:
		h.OK = true
		h.Detail = m.Engine + " " + m.EngineVersion
		a.version = m.EngineVersion
	}
	a.health = h
	return h
}

func validate(spec agentruntime.Job) error {
	if strings.TrimSpace(spec.ID) == "" {
		return fmt.Errorf("%w: id is required", agentruntime.ErrInvalidJob)
	}
	if spec.Recorder == nil {
		return fmt.Errorf("%w: a recorder is required", agentruntime.ErrInvalidJob)
	}
	if strings.TrimSpace(spec.Input) == "" {
		return fmt.Errorf("%w: input is required", agentruntime.ErrInvalidJob)
	}
	if spec.Tools != nil {
		for _, t := range spec.Tools.Specs() {
			if t.Name == toolAskUser || t.Name == toolReportBlocked {
				return fmt.Errorf("%w: tool name %q is reserved", agentruntime.ErrInvalidJob, t.Name)
			}
		}
	}
	return nil
}

// pruneLocked forgets terminal jobs past retention. Caller holds a.mu.
func (a *Adapter) pruneLocked(now time.Time) {
	for id, j := range a.jobs {
		if j.status.State.Terminal() && now.Sub(j.status.FinishedAt) > a.cfg.RetainFinished {
			delete(a.jobs, id)
		}
	}
}

// childEnv is the sidecar's whole environment: a short allowlist from
// Keystone's (enough to find node and reach a model through a proxy),
// the API key, and Config.Env.
func (a *Adapter) childEnv() []string {
	pass := []string{
		"PATH", "SYSTEMROOT", "TMPDIR", "TEMP", "TMP", "HOME",
		"HTTPS_PROXY", "HTTP_PROXY", "NO_PROXY", "https_proxy", "http_proxy", "no_proxy",
		"NODE_EXTRA_CA_CERTS", "SSL_CERT_FILE",
	}
	env := []string{"NODE_ENV=production"}
	for _, k := range pass {
		if v, ok := os.LookupEnv(k); ok {
			env = append(env, k+"="+v)
		}
	}
	if a.cfg.Model.APIKey != "" {
		env = append(env, APIKeyEnv+"="+a.cfg.Model.APIKey)
	}
	return append(env, a.cfg.Env...)
}

func (a *Adapter) modelSpec() *modelSpec {
	if a.cfg.scriptedModel != nil {
		return &modelSpec{Provider: "scripted", Script: a.cfg.scriptedModel}
	}
	return &modelSpec{Provider: "openai", BaseURL: a.cfg.Model.BaseURL, Name: a.cfg.Model.Name, API: a.cfg.Model.API}
}

// record writes one audit event, filling in the job's identity.
func (a *Adapter) record(ctx context.Context, j *job, e agentruntime.AuditEvent) error {
	e.JobID = j.spec.ID
	e.Runtime = RuntimeName
	e.Actor = j.spec.Actor
	e.At = time.Now()
	e.Detail = bound(e.Detail, 1024)
	return j.spec.Recorder.Record(ctx, e)
}

// settle moves a job to a settled state and records it. A run settles
// once: the first outcome wins. The one later move allowed is a parked
// job (WAITING_FOR_USER, BLOCKED) being cancelled. history, when
// non-nil, replaces the stored conversation.
func (a *Adapter) settle(j *job, state agentruntime.State, reason, question, output string, history json.RawMessage) {
	a.mu.Lock()
	cur := j.status.State
	if cur.Terminal() || (cur.Settled() && state != agentruntime.StateCancelled) {
		a.mu.Unlock()
		return
	}
	now := time.Now()
	j.status.State = state
	j.status.Reason = bound(reason, maxText)
	j.status.Question = bound(question, maxText)
	j.status.Output = bound(output, maxText)
	j.status.UpdatedAt = now
	j.status.ToolCalls = j.guard.ToolCalls()
	if state.Terminal() {
		j.status.FinishedAt = now
	}
	if history != nil {
		j.history = history
	}
	select {
	case <-j.settled:
		// Already closed: a parked job being cancelled.
	default:
		close(j.settled)
	}
	a.mu.Unlock()

	// The outcome is recorded with a fresh context: the run's own may be
	// exactly what just ended.
	if err := a.record(context.Background(), j, agentruntime.AuditEvent{
		Type: agentruntime.AuditJobState, State: state, Detail: reason,
	}); err != nil {
		a.mu.Lock()
		j.status.Reason = bound(strings.TrimSpace(j.status.Reason+" (audit unavailable: "+err.Error()+")"), maxText)
		a.mu.Unlock()
	}
}

func (a *Adapter) setState(j *job, s agentruntime.State) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if j.status.State.Settled() {
		return
	}
	j.status.State = s
	j.status.UpdatedAt = time.Now()
}

func (a *Adapter) touch(j *job, heartbeat bool, turns int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	now := time.Now()
	j.status.UpdatedAt = now
	if heartbeat {
		j.status.LastHeartbeat = now
	}
	if turns > 0 {
		j.status.Turns = turns
	}
}

// prepareLocked creates the context for a job's next run. Caller holds
// a.mu.
//
// The run is bounded by the job's own timeout and deliberately NOT by
// the caller's context: Start returns immediately, and a request context
// ending must not silently kill background work. Cancel is how a run is
// stopped.
func (a *Adapter) prepareLocked(j *job) (context.Context, context.CancelFunc) {
	runCtx, cancel := context.WithTimeout(context.Background(), j.limits.JobTimeout)
	j.cancelRun = cancel
	return runCtx, cancel
}

// launch starts one run: a Start or a Resume.
// stop is this run's own cancel, held here rather than read back from
// the job: by the time the run ends a Resume may already have given the
// job a new one.
func (a *Adapter) launch(runCtx context.Context, stop context.CancelFunc, j *job, input string, history json.RawMessage) {
	go func() {
		defer stop()
		a.drive(runCtx, j, input, history)
	}()
}

// run is one sidecar process and its protocol stream.
type run struct {
	cmd     *exec.Cmd
	enc     *json.Encoder
	msgs    chan message
	readErr error
	stderr  *tail
}

func (a *Adapter) spawn(ctx context.Context) (*run, error) {
	cmd := exec.CommandContext(ctx, a.cfg.Command[0], a.cfg.Command[1:]...)
	cmd.Env = a.childEnv()
	cmd.WaitDelay = 2 * time.Second
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	r := &run{cmd: cmd, enc: json.NewEncoder(stdin), msgs: make(chan message, 64), stderr: &tail{max: 4096}}
	cmd.Stderr = r.stderr
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	go func() {
		defer close(r.msgs)
		sc := bufio.NewScanner(stdout)
		sc.Buffer(make([]byte, 64<<10), maxLine)
		for sc.Scan() {
			var m message
			if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
				r.readErr = fmt.Errorf("unreadable message from sidecar: %w", err)
				return
			}
			r.msgs <- m
		}
		r.readErr = sc.Err()
	}()
	return r, nil
}

// drive runs the job until it settles. It is the watchdog: every exit
// from this function leaves the job in a settled state.
func (a *Adapter) drive(ctx context.Context, j *job, input string, history json.RawMessage) {
	lim := j.limits
	a.setState(j, agentruntime.StateRunning)
	if err := a.record(ctx, j, agentruntime.AuditEvent{Type: agentruntime.AuditJobState, State: agentruntime.StateRunning}); err != nil {
		a.settle(j, agentruntime.StateFailed, "audit unavailable: "+err.Error(), "", "", nil)
		return
	}

	r, err := a.spawn(ctx)
	if err != nil {
		a.settle(j, agentruntime.StateFailed, "could not start the agent runtime: "+err.Error(), "", "", nil)
		return
	}
	defer func() {
		// Killing is safe whatever state the process is in, and makes
		// sure a sidecar that ignored its outcome does not outlive it.
		if r.cmd.Process != nil {
			_ = r.cmd.Process.Kill()
		}
		_ = r.cmd.Wait()
	}()

	var tools []agentruntime.ToolSpec
	known := map[string]bool{}
	if j.spec.Tools != nil {
		tools = j.spec.Tools.Specs()
		for _, t := range tools {
			known[t.Name] = true
		}
	}
	start := message{
		Type: msgStart, Protocol: ProtocolVersion, JobID: j.spec.ID,
		Instructions: j.spec.Instructions + controlGuidance,
		Input:        input, History: history, Tools: tools, Model: a.modelSpec(),
		MaxTurns: lim.MaxTurns, HeartbeatMS: lim.HeartbeatInterval.Milliseconds(),
	}
	if err := r.enc.Encode(start); err != nil {
		a.settle(j, agentruntime.StateFailed, "could not start the agent runtime: "+err.Error(), "", "", nil)
		return
	}

	stall := time.NewTimer(lim.StallTimeout)
	turn := time.NewTimer(lim.TurnTimeout)
	defer stall.Stop()
	defer turn.Stop()
	reset := func(t *time.Timer, d time.Duration) {
		if !t.Stop() {
			select {
			case <-t.C:
			default:
			}
		}
		t.Reset(d)
	}

	for {
		select {
		case <-ctx.Done():
			a.mu.Lock()
			cancelled := j.cancelling
			a.mu.Unlock()
			if cancelled {
				a.settle(j, agentruntime.StateCancelled, "cancelled", "", "", nil)
			} else {
				a.settle(j, agentruntime.StateFailed, fmt.Sprintf("job timed out after %s", lim.JobTimeout), "", "", nil)
			}
			return
		case <-stall.C:
			a.settle(j, agentruntime.StateFailed, fmt.Sprintf("agent runtime stopped responding (no heartbeat for %s)", lim.StallTimeout), "", "", nil)
			return
		case <-turn.C:
			a.settle(j, agentruntime.StateFailed, fmt.Sprintf("model turn made no progress for %s", lim.TurnTimeout), "", "", nil)
			return
		case m, ok := <-r.msgs:
			if !ok {
				why := "agent runtime exited without an outcome"
				if r.readErr != nil {
					why += ": " + r.readErr.Error()
				}
				if t := strings.TrimSpace(r.stderr.String()); t != "" {
					why += ": " + t
				}
				a.settle(j, agentruntime.StateFailed, why, "", "", nil)
				return
			}
			reset(stall, lim.StallTimeout)
			switch m.Type {
			case msgReady:
				if m.Protocol != ProtocolVersion {
					a.settle(j, agentruntime.StateFailed, fmt.Sprintf("sidecar speaks protocol %d, keystone speaks %d", m.Protocol, ProtocolVersion), "", "", nil)
					return
				}
			case msgHeartbeat:
				a.touch(j, true, 0)
			case msgTurn:
				reset(turn, lim.TurnTimeout)
				if why := j.guard.Turn(); why != "" {
					a.settle(j, agentruntime.StateBlocked, why, "", "", nil)
					return
				}
				a.touch(j, true, j.guard.Turns())
			case msgInvalid:
				if a.invalid(ctx, j, m.Name, m.CallID, m.Reason) {
					return
				}
			case msgToolCall:
				reset(turn, lim.TurnTimeout)
				if a.toolCall(ctx, j, r, known, m) {
					return
				}
				// The tool's time is not the model's.
				reset(turn, lim.TurnTimeout)
				reset(stall, lim.StallTimeout)
			case msgFinal:
				if strings.TrimSpace(m.Output) == "" {
					// The ambiguous case: the model stopped without saying
					// anything. That is not success, and it is not worth
					// another round of guessing either.
					a.settle(j, agentruntime.StateBlocked, "the agent produced no answer and no next action", "", "", m.History)
					return
				}
				a.settle(j, agentruntime.StateCompleted, "", "", m.Output, m.History)
				return
			case msgWaiting:
				q := strings.TrimSpace(m.Question)
				if q == "" {
					a.settle(j, agentruntime.StateBlocked, "the agent asked for input without a question", "", "", m.History)
					return
				}
				a.settle(j, agentruntime.StateWaitingForUser, "", q, "", m.History)
				return
			case msgBlocked:
				why := strings.TrimSpace(m.Reason)
				if why == "" {
					why = "the agent reported it is blocked"
				}
				a.settle(j, agentruntime.StateBlocked, why, "", "", m.History)
				return
			case msgError:
				if m.Kind == errKindMaxTurns {
					a.settle(j, agentruntime.StateBlocked, "turn budget exhausted", "", "", m.History)
					return
				}
				a.settle(j, agentruntime.StateFailed, fmt.Sprintf("agent runtime error (%s): %s", m.Kind, m.Message), "", "", nil)
				return
			default:
				// An unknown message is a version skew; ignoring it is
				// safe because only the outcomes above can end a run.
			}
		}
	}
}

// invalid records a malformed action. It returns true when the job has
// settled.
func (a *Adapter) invalid(ctx context.Context, j *job, name, callID, why string) bool {
	if err := a.record(ctx, j, agentruntime.AuditEvent{Type: agentruntime.AuditInvalidAction, Tool: name, CallID: callID, Detail: why}); err != nil {
		a.settle(j, agentruntime.StateFailed, "audit unavailable: "+err.Error(), "", "", nil)
		return true
	}
	if failed := j.guard.Invalid(); failed != "" {
		a.settle(j, agentruntime.StateFailed, failed, "", "", nil)
		return true
	}
	return false
}

// toolCall authorizes, audits and runs one tool call, and answers the
// sidecar. It returns true when the job has settled.
func (a *Adapter) toolCall(ctx context.Context, j *job, r *run, known map[string]bool, m message) bool {
	reply := func(content string, isErr bool) bool {
		if err := r.enc.Encode(message{Type: msgToolRes, CallID: m.CallID, Output: content, IsError: isErr}); err != nil {
			a.settle(j, agentruntime.StateFailed, "lost the agent runtime: "+err.Error(), "", "", nil)
			return true
		}
		return false
	}

	var input map[string]any
	if !known[m.Name] {
		if a.invalid(ctx, j, m.Name, m.CallID, "unknown tool") {
			return true
		}
		return reply(fmt.Sprintf("error: %q is not an available tool", m.Name), true)
	}
	if err := json.Unmarshal([]byte(orEmptyObject(m.Arguments)), &input); err != nil || input == nil {
		if a.invalid(ctx, j, m.Name, m.CallID, "arguments are not a JSON object") {
			return true
		}
		return reply("error: arguments must be a JSON object", true)
	}

	if why := j.guard.BeforeTool(m.Name, input); why != "" {
		_ = a.record(ctx, j, agentruntime.AuditEvent{Type: agentruntime.AuditToolDenied, Tool: m.Name, CallID: m.CallID, Detail: why})
		a.settle(j, agentruntime.StateBlocked, why, "", "", nil)
		return true
	}
	a.mu.Lock()
	j.status.ToolCalls = j.guard.ToolCalls()
	a.mu.Unlock()
	// Fail closed: a call that cannot be audited is not made.
	if err := a.record(ctx, j, agentruntime.AuditEvent{Type: agentruntime.AuditToolRequested, Tool: m.Name, CallID: m.CallID}); err != nil {
		a.settle(j, agentruntime.StateFailed, "audit unavailable: "+err.Error(), "", "", nil)
		return true
	}

	a.setState(j, agentruntime.StateWaitingForTool)
	res, auditType, detail := a.execute(ctx, j, m.Name, input)
	a.setState(j, agentruntime.StateRunning)
	if ctx.Err() != nil {
		// Cancelled or timed out mid-call; drive's ctx branch settles.
		_ = a.record(context.Background(), j, agentruntime.AuditEvent{Type: auditType, Tool: m.Name, CallID: m.CallID, Detail: detail})
		return false
	}
	if err := a.record(ctx, j, agentruntime.AuditEvent{Type: auditType, Tool: m.Name, CallID: m.CallID, Detail: detail}); err != nil {
		a.settle(j, agentruntime.StateFailed, "audit unavailable: "+err.Error(), "", "", nil)
		return true
	}
	failed := res.Denied || res.Failed
	if why := j.guard.AfterTool(failed); why != "" {
		a.settle(j, agentruntime.StateBlocked, why+" ("+m.Name+")", "", "", nil)
		return true
	}
	return reply(res.Content, failed)
}

// execute runs the call under the tool timeout. A tool that ignores its
// context is abandoned rather than waited on: the job moves on, and the
// audit trail says the outcome is unknown.
func (a *Adapter) execute(ctx context.Context, j *job, name string, input map[string]any) (agentruntime.ToolResult, string, string) {
	tctx, cancel := context.WithTimeout(ctx, j.limits.ToolTimeout)
	defer cancel()
	type result struct {
		res agentruntime.ToolResult
		err error
	}
	done := make(chan result, 1)
	go func() {
		res, err := j.spec.Tools.Call(tctx, name, input)
		done <- result{res, err}
	}()
	select {
	case out := <-done:
		switch {
		case out.err != nil:
			return agentruntime.ToolResult{Content: "error: " + name + " could not be run", Failed: true},
				agentruntime.AuditToolFailed, out.err.Error()
		case out.res.Denied:
			return out.res, agentruntime.AuditToolDenied, out.res.Content
		case out.res.Failed:
			return out.res, agentruntime.AuditToolFailed, ""
		default:
			return out.res, agentruntime.AuditToolResult, ""
		}
	case <-tctx.Done():
		return agentruntime.ToolResult{
				Content: fmt.Sprintf("error: %s timed out after %s; its outcome is unknown", name, j.limits.ToolTimeout),
				Failed:  true,
			}, agentruntime.AuditToolTimeout,
			"outcome unknown after " + j.limits.ToolTimeout.String()
	}
}

func orEmptyObject(s string) string {
	if strings.TrimSpace(s) == "" {
		return "{}"
	}
	return s
}

func bound(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := n
	for cut > 0 && (s[cut]&0xC0) == 0x80 {
		cut--
	}
	return s[:cut] + "…"
}

// tail keeps the last max bytes written to it: the sidecar's stderr, for
// the reason a run failed.
type tail struct {
	mu  sync.Mutex
	max int
	buf []byte
}

var _ io.Writer = (*tail)(nil)

func (t *tail) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, p...)
	if len(t.buf) > t.max {
		t.buf = append([]byte(nil), t.buf[len(t.buf)-t.max:]...)
	}
	return len(p), nil
}

func (t *tail) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return string(t.buf)
}
