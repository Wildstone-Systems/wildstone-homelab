package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func mustMorning(t *testing.T, doc string) morningView {
	t.Helper()
	var v morningView
	if err := json.Unmarshal([]byte(doc), &v); err != nil {
		t.Fatal(err)
	}
	return v
}

func TestRenderMorningAllClear(t *testing.T) {
	var b strings.Builder
	renderMorning(&b, mustMorning(t, `{"summary":"Quiet night.","backupsOk":true}`))
	out := b.String()
	for _, want := range []string{"Quiet night.", "all healthy", "no runs recorded", "none"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	if strings.Contains(out, "Backup alerts") {
		t.Errorf("alerts section should be omitted when empty:\n%s", out)
	}
}

func TestRenderMorningShowsProblems(t *testing.T) {
	var b strings.Builder
	renderMorning(&b, mustMorning(t, `{
		"summary":"Trouble.",
		"unhealthy":[{"kind":"container","name":"jellyfin","node":"n1"}],
		"pendingPlans":[{"id":"plan-7","risk":"high","summary":"upgrade db"}],
		"backups":[{"planName":"nightly","status":"failed","runKind":"backup","at":"2026-10-08T01:00:00Z","detail":"disk full"}],
		"backupsOk":false,
		"backupAlerts":[{"planName":"nightly","kind":"stale-offsite","detail":"no copy in 9 days"}]}`))
	out := b.String()
	for _, want := range []string{"container/jellyfin on n1", "plan-7 [high] upgrade db",
		"nightly: failed backup", "<-- failed", "disk full", "Backup alerts", "stale-offsite"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}

func TestRenderMorningEventCountsSorted(t *testing.T) {
	var b strings.Builder
	renderMorning(&b, mustMorning(t, `{"eventTotal24h":6,"eventCounts24h":{"zeta":1,"alpha":2,"mid":3}}`))
	out := b.String()
	a, m, z := strings.Index(out, "alpha"), strings.Index(out, "mid"), strings.Index(out, "zeta")
	if !(a < m && m < z) || a < 0 {
		t.Errorf("event types not sorted:\n%s", out)
	}
}

func TestMorningViewDecodesServerFieldNames(t *testing.T) {
	v := mustMorning(t, `{"generatedAt":"2026-10-09T06:00:00Z","unhealthy":[{"kind":"k","name":"n","awaitingApproval":true}],
		"pendingPlans":[{"id":"plan-1","createdAt":"2026-10-09T05:00:00Z"}],"backupsOk":true,"eventTotal24h":3}`)
	if v.GeneratedAt.IsZero() || !v.Unhealthy[0].AwaitingApproval || v.PendingPlans[0].ID != "plan-1" ||
		!v.BackupsOK || v.EventTotal24h != 3 {
		t.Errorf("decode = %+v", v)
	}
}
