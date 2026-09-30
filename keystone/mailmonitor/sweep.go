package mailmonitor

import (
	"context"
	"log/slog"
	"time"

	"github.com/chickenrat17/wildstone-homelab/keystone/runtimedb"
)

// DefaultSweepInterval is how often RunSweeper expires candidates and
// purges excerpts. Short enough that a revocation or an event start
// removes an excerpt within minutes.
const DefaultSweepInterval = 5 * time.Minute

// RunSweeper runs runtimedb.SweepMailMonitor every interval until ctx is
// done: expiry at an event's start or the unresolved window, the
// revocation/offboarding purge, retention, and scrubbing processed mail
// answers from the work queue. Run it in the coordinator process.
func RunSweeper(ctx context.Context, db *runtimedb.DB, interval time.Duration, r runtimedb.MailRetention, log *slog.Logger) {
	if interval <= 0 {
		interval = DefaultSweepInterval
	}
	if log == nil {
		log = slog.Default()
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			out, err := db.SweepMailMonitor(time.Now().UTC(), r)
			if err != nil {
				log.Warn("mail monitor sweep failed", "err", err)
				continue
			}
			if out != (runtimedb.MailSweepResult{}) {
				log.Info("mail monitor sweep", "expired", out.Expired, "revoked", out.Revoked,
					"excerpts", out.ExcerptsPurged, "candidates", out.CandidatesDeleted,
					"messages", out.MessagesDeleted, "scrubbed", out.WorkScrubbed)
			}
		}
	}
}
