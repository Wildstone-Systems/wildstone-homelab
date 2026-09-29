// Package report builds the Butler's morning report (ADR-W017 A5): a
// DETERMINISTIC digest of unhealthy resources, the last 24 hours of
// events, pending plans, and the backup ledger verdict. No LLM is
// involved anywhere in this package — a configured provider may narrate
// on top later, but the facts never depend on one, so the report keeps
// working when the Butler is off or degraded.
package report

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/chickenrat17/wildstone-homelab/keystone/resource"
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

// BackupAlert is a verification failure that needs household attention.
// Kind is stable API data; Detail is the plain-language rendering.
type BackupAlert struct {
	PlanName string                  `json:"planName"`
	Kind     string                  `json:"kind"`
	RunKind  runtimedb.BackupRunKind `json:"runKind,omitempty"`
	At       time.Time               `json:"at,omitzero"`
	Detail   string                  `json:"detail"`
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
	// BackupAlerts covers facts a single latest-row verdict cannot: stale
	// off-site copies, failed checks/drills, and abrupt source shrinkage.
	BackupAlerts []BackupAlert `json:"backupAlerts,omitempty"`

	// Summary is the one-paragraph deterministic rendering — what the
	// daily timer appends to the event stream.
	Summary string `json:"summary"`
}

// Morning builds the report as of now.
func Morning(db *runtimedb.DB, now time.Time, desired ...resource.Resource) (*Report, error) {
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

	if err := addBackupAlerts(r, db, desired, now); err != nil {
		return nil, err
	}
	if len(r.BackupAlerts) > 0 {
		r.BackupsOK = false
	}

	r.Summary = summarize(r)
	return r, nil
}

var backupScheduleIntervals = map[string]time.Duration{
	"daily": 24 * time.Hour, "weekly": 7 * 24 * time.Hour, "monthly": 30 * 24 * time.Hour,
}

const sourceShrinkKind = "source-shrink"

func addBackupAlerts(r *Report, db *runtimedb.DB, desired []resource.Resource, now time.Time) error {
	for _, plan := range desired {
		if plan.Kind != resource.KindBackupPlan || plan.BackupPlan == nil {
			continue
		}
		name := plan.Metadata.Name
		if offsite := plan.BackupPlan.Offsite; offsite != nil {
			last, err := db.LastBackupRun(name, runtimedb.BackupRunOffsite, runtimedb.BackupRunSucceeded)
			if errors.Is(err, runtimedb.ErrNotFound) {
				r.BackupAlerts = append(r.BackupAlerts, BackupAlert{PlanName: name, Kind: "stale-offsite", Detail: "the off-site copy has never completed"})
			} else if err != nil {
				return fmt.Errorf("report: reading %s off-site freshness: %w", name, err)
			} else if interval := backupScheduleIntervals[offsite.Schedule]; interval > 0 && now.Sub(last.FinishedAt) > interval {
				r.BackupAlerts = append(r.BackupAlerts, BackupAlert{PlanName: name, Kind: "stale-offsite", At: last.FinishedAt, Detail: fmt.Sprintf("the last off-site copy is older than its %s schedule", offsite.Schedule)})
			}
		}

		for _, check := range []struct {
			kind  runtimedb.BackupRunKind
			alert string
		}{
			{runtimedb.BackupRunCheck, "failed-check"},
			{runtimedb.BackupRunOffsiteCheck, "failed-check"},
			{runtimedb.BackupRunDrill, "failed-drill"},
			{runtimedb.BackupRunOffsiteDrill, "failed-drill"},
		} {
			last, err := db.LastBackupRunAny(name, check.kind)
			if errors.Is(err, runtimedb.ErrNotFound) {
				continue
			}
			if err != nil {
				return fmt.Errorf("report: reading %s %s verdict: %w", name, check.kind, err)
			}
			if last.Status == runtimedb.BackupRunFailed {
				detail := last.Detail
				if detail == "" {
					detail = fmt.Sprintf("the latest %s failed", check.kind)
				}
				r.BackupAlerts = append(r.BackupAlerts, BackupAlert{PlanName: name, Kind: check.alert, RunKind: check.kind, At: last.FinishedAt, Detail: detail})
			}
		}

		samples, err := db.LatestBackupSourceSamples(name, 2)
		if err != nil {
			return fmt.Errorf("report: reading %s source sizes: %w", name, err)
		}
		if len(samples) == 2 && samples[1].Bytes > 0 && samples[0].Bytes < samples[1].Bytes-samples[1].Bytes/4 {
			drop := 100 * float64(samples[1].Bytes-samples[0].Bytes) / float64(samples[1].Bytes)
			r.BackupAlerts = append(r.BackupAlerts, BackupAlert{
				PlanName: name, Kind: sourceShrinkKind, At: samples[0].MeasuredAt,
				Detail: fmt.Sprintf("protected source size dropped %.0f%% (%d to %d bytes)", drop, samples[1].Bytes, samples[0].Bytes),
			})
		}
	}
	sort.SliceStable(r.BackupAlerts, func(i, j int) bool {
		if r.BackupAlerts[i].PlanName != r.BackupAlerts[j].PlanName {
			return r.BackupAlerts[i].PlanName < r.BackupAlerts[j].PlanName
		}
		return r.BackupAlerts[i].Kind < r.BackupAlerts[j].Kind
	})
	return nil
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
	if len(r.BackupAlerts) > 0 {
		alerts := make([]string, 0, len(r.BackupAlerts))
		for _, a := range r.BackupAlerts {
			alerts = append(alerts, a.PlanName+"/"+a.Kind)
		}
		parts = append(parts, "backup alerts: "+strings.Join(alerts, ", "))
	}
	return "morning report: " + strings.Join(parts, "; ")
}
