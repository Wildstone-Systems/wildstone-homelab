// Package report builds the Butler's morning report (ADR-W017 A5): a
// DETERMINISTIC digest of unhealthy resources, the last 24 hours of
// events, pending plans, and the backup ledger verdict. No LLM is
// involved anywhere in this package — a configured provider may narrate
// on top later, but the facts never depend on one, so the report keeps
// working when the Butler is off or degraded.
package report

import (
	"fmt"
	"strings"
	"time"

	"github.com/chickenrat17/wildstone-homelab/keystone/runtimedb"
)

// maxReportEvents bounds the 24h event scan; a homelab producing more
// than this in a day is itself a finding, reported as a truncated count.
const maxReportEvents = 2000

// BackupVerdict summarizes the latest ledger row for one backup plan.
type BackupVerdict struct {
	PlanName string                    `json:"planName"`
	Status   runtimedb.BackupRunStatus `json:"status"`
	RunKind  runtimedb.BackupRunKind   `json:"runKind"`
	At       time.Time                 `json:"at"`
	Detail   string                    `json:"detail,omitempty"`
}

// Report is one morning report. Every field is derived from runtime-DB
// reads at GeneratedAt; building it mutates nothing.
type Report struct {
	GeneratedAt time.Time `json:"generatedAt"`

	// Unhealthy is every observed resource currently reporting unhealthy,
	// (kind, name)-ordered.
	Unhealthy []runtimedb.Observed `json:"unhealthy"`
	// PendingPlans is every plan record still in state proposed — the
	// human approval queue.
	PendingPlans []runtimedb.PlanRecord `json:"pendingPlans"`
	// EventCounts24h counts the last 24 hours of events by type;
	// EventTotal24h is their sum.
	EventCounts24h map[string]int `json:"eventCounts24h"`
	EventTotal24h  int            `json:"eventTotal24h"`
	// Backups is the latest ledger verdict per backup plan, plan-name
	// ordered. BackupsOK is false if any plan's latest run failed.
	Backups   []BackupVerdict `json:"backups"`
	BackupsOK bool            `json:"backupsOk"`

	// Summary is the one-paragraph deterministic rendering — what the
	// daily timer appends to the event stream.
	Summary string `json:"summary"`
}

// Morning builds the report as of now.
func Morning(db *runtimedb.DB, now time.Time) (*Report, error) {
	r := &Report{GeneratedAt: now.UTC(), EventCounts24h: map[string]int{}, BackupsOK: true}

	observed, err := db.ListObserved()
	if err != nil {
		return nil, fmt.Errorf("report: %w", err)
	}
	for _, o := range observed {
		if !o.Healthy {
			r.Unhealthy = append(r.Unhealthy, o)
		}
	}

	plans, err := db.ListPlans(500)
	if err != nil {
		return nil, fmt.Errorf("report: %w", err)
	}
	for _, p := range plans {
		if p.State == runtimedb.PlanProposed {
			r.PendingPlans = append(r.PendingPlans, p)
		}
	}

	events, err := db.EventsSince(now.Add(-24*time.Hour), maxReportEvents)
	if err != nil {
		return nil, fmt.Errorf("report: %w", err)
	}
	for _, e := range events {
		r.EventCounts24h[e.Type]++
		r.EventTotal24h++
	}

	// Latest ledger row per backup plan: ListBackupRuns is newest-first,
	// so the first row seen for a plan is its verdict. A run still in
	// state running is skipped — the previous finished run is the verdict.
	runs, err := db.ListBackupRuns(200)
	if err != nil {
		return nil, fmt.Errorf("report: %w", err)
	}
	seen := map[string]bool{}
	for _, run := range runs {
		if seen[run.PlanName] || run.Status == runtimedb.BackupRunRunning {
			continue
		}
		seen[run.PlanName] = true
		at := run.FinishedAt
		if at.IsZero() {
			at = run.StartedAt
		}
		r.Backups = append(r.Backups, BackupVerdict{
			PlanName: run.PlanName, Status: run.Status, RunKind: run.RunKind, At: at, Detail: run.Detail,
		})
		if run.Status != runtimedb.BackupRunSucceeded {
			r.BackupsOK = false
		}
	}
	// ListBackupRuns orders by seq; re-order verdicts by plan name for a
	// stable report.
	for i := 1; i < len(r.Backups); i++ {
		for j := i; j > 0 && r.Backups[j-1].PlanName > r.Backups[j].PlanName; j-- {
			r.Backups[j-1], r.Backups[j] = r.Backups[j], r.Backups[j-1]
		}
	}

	r.Summary = summarize(r)
	return r, nil
}

func summarize(r *Report) string {
	var parts []string
	if len(r.Unhealthy) == 0 {
		parts = append(parts, "all observed resources healthy")
	} else {
		names := make([]string, 0, len(r.Unhealthy))
		for _, o := range r.Unhealthy {
			names = append(names, fmt.Sprintf("%s %s", o.Kind, o.Name))
		}
		parts = append(parts, fmt.Sprintf("%d unhealthy: %s", len(r.Unhealthy), strings.Join(names, ", ")))
	}
	parts = append(parts, fmt.Sprintf("%d plans awaiting approval", len(r.PendingPlans)))
	parts = append(parts, fmt.Sprintf("%d events in 24h", r.EventTotal24h))
	switch {
	case len(r.Backups) == 0:
		parts = append(parts, "backups: no runs recorded")
	case r.BackupsOK:
		parts = append(parts, fmt.Sprintf("backups: all %d plans ok", len(r.Backups)))
	default:
		var failed []string
		for _, b := range r.Backups {
			if b.Status != runtimedb.BackupRunSucceeded {
				failed = append(failed, b.PlanName)
			}
		}
		parts = append(parts, "backups: FAILING for "+strings.Join(failed, ", "))
	}
	return "morning report: " + strings.Join(parts, "; ")
}
