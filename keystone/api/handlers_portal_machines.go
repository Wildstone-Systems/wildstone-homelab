package api

// The machines card's readings.
//
// WHY THIS IS A LIVE PROBE AND NOT A STORED METRIC. Keystone keeps no
// time series — that is the metrics capability's job, and pointing a
// household dashboard at Prometheus would make the family's front page
// depend on an optional operator capability. What the card needs is
// three numbers as they are right now, which is exactly one round trip
// to the agent that is already connected.
//
// IT IS CACHED, because the alternative is an agent round trip on every
// page load from every tablet in the house, and because the numbers do
// not mean anything at a finer resolution than this: the agent samples
// CPU over 200ms, so two reads a second apart are two samples of noise.
//
// ZERO IS "NOT MEASURED", NEVER "IDLE". A kernel too old for
// MemAvailable, a container without /proc, a darwin build — all report
// zero, and every one of them must render as an absent bar rather than
// an empty one. §7 is explicit: never a bar at zero, "which reads as an
// idle machine rather than a missing reading".

import (
	"context"
	"sync"
	"time"

	wsv1 "github.com/chickenrat17/wildstone-homelab/proto/gen/wardstone/v1"
)

// machineStatsTTL is how long one reading serves every caller.
//
// Fifteen seconds: long enough that a wall tablet refreshing and three
// phones loading the page do not each cost an agent round trip, short
// enough that a household watching a backup run sees the disk move.
const machineStatsTTL = 15 * time.Second

// machineStats is one machine's live readings, as percentages.
//
// Each has its own Known flag rather than a sentinel, because the three
// fail independently: a kernel can publish CPU and not MemAvailable,
// and a statfs can fail while /proc is fine. A single "available" for
// all three would throw away two good readings to report one bad one.
type machineStats struct {
	CPU       int  `json:"cpu"`
	CPUKnown  bool `json:"cpuKnown"`
	Memory    int  `json:"memory"`
	MemKnown  bool `json:"memoryKnown"`
	Disk      int  `json:"disk"`
	DiskKnown bool `json:"diskKnown"`
	// Node is not sent to the household surface — §7 gives that card no
	// hostnames — but it is kept here so the cache can tell two
	// machines apart.
	node string
}

// machineStatsCache holds the last reading per node.
type machineStatsCache struct {
	mu   sync.Mutex
	at   time.Time
	node string
	got  machineStats
	ok   bool
}

// machineReadings returns the live percentages for one node.
//
// A failure is not an error to the caller: the card's whole job is to
// be honest about what it cannot see, so an unreachable agent returns
// ok=false and the page says so. Returning an error would make one
// unreadable machine take the entire front page down, which is the
// opposite of what this card is for.
func (s *Server) machineReadings(ctx context.Context, node string) (machineStats, bool) {
	if s.Agents == nil || node == "" {
		return machineStats{}, false
	}
	s.hostFacts.mu.Lock()
	if s.hostFacts.ok && s.hostFacts.node == node && time.Since(s.hostFacts.at) < machineStatsTTL {
		got := s.hostFacts.got
		s.hostFacts.mu.Unlock()
		return got, true
	}
	s.hostFacts.mu.Unlock()

	// Bounded, and deliberately shorter than a page load's patience: the
	// agent's own CPU sample takes 200ms, so a healthy answer is fast
	// and a slow one is a machine in trouble. The card would rather say
	// "can't read" than hold the household's front page open.
	probeCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()

	res, err := s.Agents.Apply(probeCtx, node, &wsv1.Operation{
		Op: &wsv1.Operation_HostFacts{HostFacts: &wsv1.HostFacts{}},
	})
	if err != nil {
		s.Log.Info("machine readings unavailable", "node", node, "err", err)
		return machineStats{}, false
	}
	facts := res.GetHostFacts()
	if facts == nil {
		return machineStats{}, false
	}

	got := machineStats{node: node}
	if p := facts.GetCpuBusyPercent(); p > 0 {
		got.CPU, got.CPUKnown = int(p), true
	}
	if total, avail := facts.GetMemTotalBytes(), facts.GetMemAvailableBytes(); total > 0 && avail > 0 && avail <= total {
		got.Memory, got.MemKnown = int((total-avail)*100/total), true
	}
	if total, free := facts.GetRootDiskTotalBytes(), facts.GetRootDiskFreeBytes(); total > 0 && free <= total {
		got.Disk, got.DiskKnown = int((total-free)*100/total), true
	}

	s.hostFacts.mu.Lock()
	s.hostFacts.at, s.hostFacts.node, s.hostFacts.got, s.hostFacts.ok = time.Now(), node, got, true
	s.hostFacts.mu.Unlock()
	return got, true
}
