// Package indexworker drains semantic-memory work under the Keystone
// coordinator lease. It owns no authority: source metadata comes from the
// runtime database and is rechecked atomically at publication.
package indexworker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/Wildstone-Systems/wildstone-homelab/keystone/butler/provider"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/butlerlive"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/runtimedb"
)

const (
	leaseDuration = 2 * time.Minute
	retryDelay    = 30 * time.Second
	// maxAttempts bounds retries so one permanently broken source cannot
	// cycle forever; after it the job is marked failed with its last class.
	maxAttempts = 5
)

// Resolver returns the current server-owned embedding route and its stable
// non-secret identity. Tests replace it with a deterministic fake.
type Resolver func(*runtimedb.DB, string, *runtimedb.PrivacyTurn) (provider.Embedder, string, string, error)

// Worker drains one database using one already-acquired coordinator lease.
type Worker struct {
	DB      *runtimedb.DB
	Lease   runtimedb.CoordinatorLease
	Resolve Resolver
	Now     func() time.Time
	// RetryDelay overrides the default wait before a failed job is retried.
	RetryDelay time.Duration
}

// DefaultResolver binds the exact Agent assignment through S1.
func DefaultResolver(db *runtimedb.DB, agent string, privacy *runtimedb.PrivacyTurn) (provider.Embedder, string, string, error) {
	b, err := db.GetButler(agent)
	if err != nil {
		return nil, "", "", err
	}
	embedder, err := butlerlive.ResolveEmbedder(db, agent, privacy)
	if err != nil {
		return nil, "", "", err
	}
	return embedder, b.Provider + "/" + b.ProviderModel, strconv.FormatInt(b.ModelSeq, 10), nil
}

// RunOnce processes at most one item. ok is false for an idle queue.
func (w *Worker) RunOnce(ctx context.Context) (ok bool, err error) {
	if w.DB == nil || w.Lease.Holder == "" || w.Lease.Epoch <= 0 {
		return false, fmt.Errorf("indexworker: coordinator authority is not wired")
	}
	if err := w.DB.CheckCoordinator(w.Lease.Holder, w.Lease.Epoch); err != nil {
		return false, err
	}
	resolve := w.Resolve
	if resolve == nil {
		resolve = DefaultResolver
	}
	job, ok, err := w.DB.ClaimIndexJobLease(w.Lease.Holder, leaseDuration)
	if err != nil || !ok {
		return ok, err
	}
	delay := w.RetryDelay
	if delay <= 0 {
		delay = retryDelay
	}
	fail := func(class string, cause error) (bool, error) {
		if job.Attempts >= maxAttempts {
			if finishErr := w.DB.FinishIndexJobLease(job.Seq, job.LeaseOwner, class); finishErr != nil {
				return true, finishErr
			}
			return true, cause
		}
		if retryErr := w.DB.RetryIndexJob(job.Seq, job.LeaseOwner, class, delay); retryErr != nil {
			return true, retryErr
		}
		return true, cause
	}
	if job.Kind != runtimedb.IndexJobIndex {
		return fail("unsupported-job", fmt.Errorf("indexworker: unsupported job kind"))
	}
	var payload runtimedb.MemoryIndexJob
	if err := json.Unmarshal([]byte(job.Payload), &payload); err != nil || payload.MemorySeq <= 0 {
		return fail("invalid-payload", fmt.Errorf("indexworker: invalid job payload"))
	}
	source, err := w.DB.MemoryForIndex(payload.MemorySeq)
	if errors.Is(err, runtimedb.ErrNotFound) {
		return true, w.DB.FinishIndexJobLease(job.Seq, job.LeaseOwner, "source-unavailable")
	}
	if err != nil {
		return fail("source-unavailable", err)
	}
	agent := source.Butler
	if agent == "" {
		return fail("source-unassigned", fmt.Errorf("indexworker: source has no Agent assignment"))
	}
	b, err := w.DB.GetButler(agent)
	if err != nil {
		return fail("assignment-unavailable", err)
	}
	privacyCtx, privacy, err := w.DB.BeginPrivacyTurn(ctx, b.ModelKind, runtimedb.MemoryDestination{BuiltIn: true}, "")
	if err != nil {
		return fail("privacy-refused", err)
	}
	if err := runtimedb.TrackMemoryPrivacy(privacyCtx, source.Seq, nil); err != nil {
		return fail("privacy-refused", err)
	}
	embedder, model, version, err := resolve(w.DB, agent, privacy)
	if err != nil {
		return fail("embedder-unavailable", err)
	}
	vector, err := embedder.Embed(privacyCtx, source.Content)
	if err != nil {
		return fail("embedding-failed", err)
	}
	if err := w.DB.CheckCoordinator(w.Lease.Holder, w.Lease.Epoch); err != nil {
		// Leave the attempt leased. A newly promoted coordinator recovers it
		// after expiry; the fenced process performs no further durable write.
		return true, err
	}
	if err := w.DB.PublishMemoryIndex(job, source, vector, model, version, w.Lease); err != nil {
		return fail("publication-refused", err)
	}
	return true, nil
}

// Run drains in bounded ticks until cancellation. Foreground writes never wait
// for this loop.
func (w *Worker) Run(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		_, _ = w.RunOnce(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
