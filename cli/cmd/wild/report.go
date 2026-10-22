package main

// `wild report morning` (ADR-W017 A5): the Butler's deterministic
// morning report as a thin API client. The server builds the report; the
// CLI only renders it.

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"time"
)

func runReport(args []string) int {
	if len(args) < 1 {
		reportUsage()
		return 2
	}
	switch args[0] {
	case "morning":
		return runReportMorning(args[1:])
	default:
		fmt.Fprintf(os.Stderr, "wild report: unknown subcommand %q\n", args[0])
		reportUsage()
		return 2
	}
}

func reportUsage() {
	fmt.Fprintln(os.Stderr, `usage:
  wild report morning [-json] [-api URL]`)
}

// morningView mirrors the fields of the API's morning report that the
// CLI prints. It is declared here rather than imported from
// keystone/report, which would pull the runtime DB and SQLite into the
// CLI binary.
type morningView struct {
	GeneratedAt time.Time `json:"generatedAt"`
	Summary     string    `json:"summary"`
	Unhealthy   []struct {
		Kind             string `json:"kind"`
		Name             string `json:"name"`
		Node             string `json:"node"`
		AwaitingApproval bool   `json:"awaitingApproval"`
	} `json:"unhealthy"`
	PendingPlans []struct {
		ID        string    `json:"id"`
		Summary   string    `json:"summary"`
		Risk      string    `json:"risk"`
		CreatedAt time.Time `json:"createdAt"`
	} `json:"pendingPlans"`
	EventTotal24h  int            `json:"eventTotal24h"`
	EventCounts24h map[string]int `json:"eventCounts24h"`
	Backups        []struct {
		PlanName string    `json:"planName"`
		Status   string    `json:"status"`
		RunKind  string    `json:"runKind"`
		At       time.Time `json:"at"`
		Detail   string    `json:"detail"`
	} `json:"backups"`
	BackupsOK    bool `json:"backupsOk"`
	BackupAlerts []struct {
		PlanName string `json:"planName"`
		Kind     string `json:"kind"`
		Detail   string `json:"detail"`
	} `json:"backupAlerts"`
}

func runReportMorning(args []string) int {
	fs := flag.NewFlagSet("wild report morning", flag.ExitOnError)
	apiBase := fs.String("api", "http://127.0.0.1:9441", "Keystone public API base URL")
	raw := fs.Bool("json", false, "print the raw JSON reply")
	fs.Parse(args) //nolint:errcheck // ExitOnError

	token, ok := requireToken("report morning")
	if !ok {
		return 2
	}
	out, err := apiCall(*apiBase, token, http.MethodGet, "/api/v1/reports/morning", nil)
	if err != nil {
		fmt.Fprintf(os.Stderr, "wild report morning: %v\n", err)
		return 1
	}
	if *raw {
		fmt.Println(out)
		return 0
	}
	var v morningView
	if err := json.Unmarshal([]byte(out), &v); err != nil {
		fmt.Fprintf(os.Stderr, "wild report morning: unreadable reply: %v\n", err)
		return 1
	}
	renderMorning(os.Stdout, v)
	return 0
}

// renderMorning prints the report for a person. Empty sections say so in
// one line rather than vanishing, so silence is never mistaken for an
// omission.
func renderMorning(w io.Writer, v morningView) {
	fmt.Fprintln(w, v.Summary)

	fmt.Fprintln(w, "\nUnhealthy:")
	if len(v.Unhealthy) == 0 {
		fmt.Fprintln(w, "  all healthy")
	}
	for _, u := range v.Unhealthy {
		line := fmt.Sprintf("  %s/%s", u.Kind, u.Name)
		if u.Node != "" {
			line += " on " + u.Node
		}
		if u.AwaitingApproval {
			line += " (awaiting approval)"
		}
		fmt.Fprintln(w, line)
	}

	fmt.Fprintln(w, "\nPlans awaiting approval:")
	if len(v.PendingPlans) == 0 {
		fmt.Fprintln(w, "  none")
	}
	for _, p := range v.PendingPlans {
		fmt.Fprintf(w, "  %s [%s] %s\n", p.ID, p.Risk, p.Summary)
	}

	fmt.Fprintf(w, "\nEvents in the last 24h: %d\n", v.EventTotal24h)
	types := make([]string, 0, len(v.EventCounts24h))
	for t := range v.EventCounts24h {
		types = append(types, t)
	}
	sort.Strings(types)
	for _, t := range types {
		fmt.Fprintf(w, "  %s: %d\n", t, v.EventCounts24h[t])
	}

	fmt.Fprintln(w, "\nBackups:")
	if len(v.Backups) == 0 {
		fmt.Fprintln(w, "  no runs recorded")
	}
	for _, b := range v.Backups {
		mark := ""
		if b.Status != "succeeded" {
			mark = "  <-- " + b.Status
		}
		line := fmt.Sprintf("  %s: %s %s at %s%s", b.PlanName, b.Status, b.RunKind,
			b.At.UTC().Format(time.RFC3339), mark)
		if b.Detail != "" {
			line += " (" + b.Detail + ")"
		}
		fmt.Fprintln(w, line)
	}

	if len(v.BackupAlerts) > 0 {
		fmt.Fprintln(w, "\nBackup alerts:")
		for _, a := range v.BackupAlerts {
			fmt.Fprintf(w, "  %s [%s] %s\n", a.PlanName, a.Kind, a.Detail)
		}
	}
}
