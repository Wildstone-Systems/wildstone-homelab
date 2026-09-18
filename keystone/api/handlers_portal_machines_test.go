package api

// The machines card's readings, and the one rule that matters about
// them: a figure nobody could measure must never render as a figure.

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	wsv1 "github.com/chickenrat17/wildstone-homelab/proto/gen/wardstone/v1"
)

// factsApplier answers HostFacts with whatever a test wants, and counts
// how many times it was asked.
type factsApplier struct {
	facts *wsv1.HostFactsResult
	err   error
	calls int
}

func (f *factsApplier) Apply(_ context.Context, _ string, _ *wsv1.Operation) (*wsv1.OpResult, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	return &wsv1.OpResult{Result: &wsv1.OpResult_HostFacts{HostFacts: f.facts}}, nil
}

func machineHarness(t *testing.T, a *factsApplier) *harness {
	t.Helper()
	h := homeHarness(t)
	h.s.Agents = a
	if err := h.s.DB.RecordNodeJoin("wildstone-homelab"); err != nil {
		t.Fatal(err)
	}
	return h
}

// The three figures are computed the way a person means them.
func TestTheMachineReadingsAreComputedHonestly(t *testing.T) {
	const gb = 1 << 30
	a := &factsApplier{facts: &wsv1.HostFactsResult{
		NumCpu:         4,
		CpuBusyPercent: 7,
		// 16 GB total, 9.6 GB available -> 40% used. MemAvailable, not
		// MemFree: free memory on a healthy box is near zero.
		MemTotalBytes:     16 * gb,
		MemAvailableBytes: 96 * gb / 10,
		// 100 GB total, 37 free -> 63% used.
		RootDiskTotalBytes: 100 * gb,
		RootDiskFreeBytes:  37 * gb,
	}}
	h := machineHarness(t, a)
	got := h.summary(t, "sandy").Machines.Stats

	for _, c := range []struct {
		name  string
		got   int
		known bool
		want  int
	}{
		{"cpu", got.CPU, got.CPUKnown, 7},
		{"memory", got.Memory, got.MemKnown, 40},
		{"disk", got.Disk, got.DiskKnown, 63},
	} {
		if !c.known {
			t.Errorf("%s is not known", c.name)
			continue
		}
		if c.got != c.want {
			t.Errorf("%s = %d%%, want %d%%", c.name, c.got, c.want)
		}
	}
}

// A figure the agent could not measure comes back as zero on the wire,
// and zero must mean "not measured" rather than "idle" — §7's rule that
// a bar at zero reads as an idle machine rather than a missing reading.
func TestAnUnmeasuredFigureIsNotAZeroReading(t *testing.T) {
	const gb = 1 << 30
	a := &factsApplier{facts: &wsv1.HostFactsResult{
		// A kernel too old for MemAvailable, and a /proc the agent could
		// not sample: both arrive as zero.
		MemTotalBytes:      16 * gb,
		MemAvailableBytes:  0,
		CpuBusyPercent:     0,
		RootDiskTotalBytes: 100 * gb,
		RootDiskFreeBytes:  37 * gb,
	}}
	h := machineHarness(t, a)
	got := h.summary(t, "sandy").Machines

	if got.Stats.CPUKnown {
		t.Error("an unsampled CPU is reported as a reading of 0%")
	}
	if got.Stats.MemKnown {
		t.Error("an unreadable MemAvailable is reported as a reading of 0%")
	}
	// The one that WAS readable still is: the three fail independently,
	// and one bad figure must not throw away two good ones.
	if !got.Stats.DiskKnown || got.Stats.Disk != 63 {
		t.Errorf("disk = %d%% known=%v, want 63%% known", got.Stats.Disk, got.Stats.DiskKnown)
	}
	// With something readable, the card does not claim it can read
	// nothing.
	if !got.Metrics.Available {
		t.Error("the card says it can read nothing while the disk figure is good")
	}
}

// An unreachable agent takes the bars away, not the page.
func TestAnUnreachableAgentLeavesTheRestOfThePageStanding(t *testing.T) {
	a := &factsApplier{err: context.DeadlineExceeded}
	h := machineHarness(t, a)
	got := h.summary(t, "sandy")

	if got.Machines.Metrics.Available {
		t.Error("the card claims readings it never got")
	}
	if got.Machines.Metrics.Problem == "" {
		t.Error("no sentence in place of the bars")
	}
	// The backup half of the card is unaffected, and so is everything
	// else: one unreadable machine must not take the household's front
	// page down.
	if !got.Machines.Available {
		t.Error("the backup ledger went unavailable because the agent did")
	}
	if !got.Activity.Available || !got.NeedsYou.Available {
		t.Error("an unreachable agent took unrelated cards down with it")
	}
}

// The reading is cached, so a house full of tablets is not a house full
// of agent round trips.
func TestTheReadingIsCachedAcrossPageLoads(t *testing.T) {
	a := &factsApplier{facts: &wsv1.HostFactsResult{CpuBusyPercent: 11}}
	h := machineHarness(t, a)
	for i := 0; i < 4; i++ {
		h.summary(t, "sandy")
	}
	if a.calls != 1 {
		t.Errorf("the agent was asked %d times for four page loads, want 1", a.calls)
	}
}

// With no agent wired at all, the card degrades rather than erroring.
func TestNoAgentConnectionStillRendersTheCard(t *testing.T) {
	h := homeHarness(t)
	h.s.Agents = nil
	got := h.summary(t, "sandy").Machines
	if got.Metrics.Available {
		t.Error("readings claimed with no agent connection")
	}
	if got.Metrics.Problem == "" {
		t.Error("no sentence in place of the bars")
	}
}

// The household surface is given no hostname, here or anywhere on this
// card: §7 gives it no hostnames, addresses or container detail.
func TestTheMachinesCardCarriesNoHostname(t *testing.T) {
	a := &factsApplier{facts: &wsv1.HostFactsResult{
		Hostname: "wildstone-homelab", Kernel: "6.1.0", Os: "linux", CpuBusyPercent: 5,
	}}
	h := machineHarness(t, a)
	res := h.member(http.MethodGet, "/api/v1/home/summary", "sandy", "homepage_users", nil, "")
	defer res.Body.Close()
	body, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	for _, leak := range []string{"wildstone-homelab", "6.1.0"} {
		if strings.Contains(string(body), leak) {
			t.Errorf("the summary carries %q", leak)
		}
	}
}
