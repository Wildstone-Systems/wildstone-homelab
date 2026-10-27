//go:build linux

package agent

import (
	"bufio"
	"context"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	wsv1 "github.com/Wildstone-Systems/wildstone-homelab/proto/gen/wardstone/v1"
)

func (a *Agent) hostFacts() (*wsv1.OpResult_HostFacts, *wsv1.OpError) {
	hostname, _ := os.Hostname()
	var uts syscall.Utsname
	kernel := ""
	if err := syscall.Uname(&uts); err == nil {
		kernel = utsString(uts.Release)
	}
	var st syscall.Statfs_t
	var diskTotal, diskFree uint64
	if err := syscall.Statfs("/", &st); err == nil {
		diskTotal = st.Blocks * uint64(st.Bsize)
		diskFree = st.Bavail * uint64(st.Bsize)
	}
	total, available := memBytes()
	busy, measured := cpuBusyPercent()
	engine, engineVersion := hostEngineVersion()
	epoch := a.activeLeaderEpoch.Load()
	return &wsv1.OpResult_HostFacts{HostFacts: &wsv1.HostFactsResult{
		Hostname:           hostname,
		Os:                 runtime.GOOS,
		Kernel:             kernel,
		NumCpu:             uint32(runtime.NumCPU()),
		MemTotalBytes:      total,
		MemAvailableBytes:  available,
		CpuBusyPercent:     busy,
		CpuMeasured:        measured,
		RootDiskTotalBytes: diskTotal,
		RootDiskFreeBytes:  diskFree,
		AgentVersion:       a.cfg.Version,
		Engine:             engine,
		EngineVersion:      engineVersion,
		EngineLeader:       epoch != 0,
		LeaderEpoch:        epoch,
	}}, nil
}

// hostEngineVersion reads the installed binary's version without opening an
// administrator kubeconfig or contacting the API server. An installed binary
// may be unhealthy; HostFacts does not claim readiness from this field.
func hostEngineVersion() (string, string) {
	for _, path := range []string{"/usr/local/bin/k3s", "/usr/bin/k3s"} {
		info, err := os.Stat(path)
		if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		out, err := exec.CommandContext(ctx, path, "--version").Output()
		cancel()
		if err != nil {
			return "k3s", ""
		}
		fields := strings.Fields(string(out))
		if len(fields) >= 3 && fields[0] == "k3s" && fields[1] == "version" {
			return "k3s", fields[2]
		}
		return "k3s", ""
	}
	return "", ""
}

// cpuSampleWindow is how long cpuBusyPercent watches /proc/stat.
//
// Short enough that a facts call stays snappy, long enough that the
// jiffy counters move: at 100Hz a 200ms window is ~20 ticks per core,
// which is plenty of resolution for a bar on a dashboard and far too
// coarse to be a profiler. It is deliberately not configurable — a
// caller that could ask for a ten-second window could hold an agent
// worker for ten seconds.
const cpuSampleWindow = 200 * time.Millisecond

// cpuBusyPercent samples /proc/stat twice and returns the percentage of
// jiffies spent doing anything but idling.
//
// SAMPLED, NOT DERIVED FROM LOAD AVERAGE. Load is a queue length —
// runnable plus uninterruptible tasks — and dividing it by the core
// count produces a number that looks like a utilisation and is not one:
// a box with four cores and three processes blocked on a slow disk
// reports 75% while the CPUs sit idle. On a family's dashboard that is
// a bar pointing at a problem that does not exist.
//
// Returns 0 when it cannot read, and the caller must treat 0 as "not
// measured" rather than "idle" — which is why the household surface
// draws no bar at all rather than an empty one.
//
// measured is false when there was nothing to read, which is different
// from a machine that is genuinely idle: that one is measured at 0.
func cpuBusyPercent() (busyPercent uint32, measured bool) {
	first, ok := cpuJiffies()
	if !ok {
		return 0, false
	}
	time.Sleep(cpuSampleWindow)
	second, ok := cpuJiffies()
	if !ok {
		return 0, false
	}
	totalDelta := second.total - first.total
	idleDelta := second.idle - first.idle
	if totalDelta == 0 || idleDelta > totalDelta {
		// A counter that did not move, or went backwards across a
		// suspend. No reading is better than a wrong one.
		return 0, false
	}
	busy := (totalDelta - idleDelta) * 100 / totalDelta
	if busy > 100 {
		busy = 100
	}
	return uint32(busy), true
}

type cpuTicks struct{ total, idle uint64 }

// cpuJiffies reads the aggregate "cpu" line of /proc/stat.
//
// idle counts both idle and iowait: a core waiting on a disk is not
// doing work, and counting iowait as busy is what makes a machine
// running a backup look pinned.
func cpuJiffies() (cpuTicks, bool) {
	f, err := os.Open("/proc/stat")
	if err != nil {
		return cpuTicks{}, false
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 5 || fields[0] != "cpu" {
			continue
		}
		var t cpuTicks
		for i, raw := range fields[1:] {
			n, err := strconv.ParseUint(raw, 10, 64)
			if err != nil {
				return cpuTicks{}, false
			}
			t.total += n
			// Fields after the label are: user nice system idle iowait
			// irq softirq steal guest guest_nice. Index 3 is idle and 4
			// is iowait.
			if i == 3 || i == 4 {
				t.idle += n
			}
		}
		return t, true
	}
	return cpuTicks{}, false
}

// memBytes reads total and AVAILABLE memory from /proc/meminfo.
//
// MemAvailable rather than MemFree, and the difference is the whole
// point: free memory on a healthy Linux box is near zero because the
// page cache uses whatever is spare, so "used = total - free" reports
// 97% on a machine with plenty of headroom. MemAvailable is the
// kernel's own estimate of what a new process could actually get, which
// is the number a person means when they ask how much memory is left.
//
// A kernel too old to publish MemAvailable reports 0, which the caller
// renders as "not measured" rather than as a full machine.
func memBytes() (total, available uint64) {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0, 0
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		for _, want := range []struct {
			prefix string
			into   *uint64
		}{
			{"MemTotal:", &total},
			{"MemAvailable:", &available},
		} {
			kb, ok := strings.CutPrefix(line, want.prefix)
			if !ok {
				continue
			}
			if fields := strings.Fields(kb); len(fields) >= 1 {
				n, _ := strconv.ParseUint(fields[0], 10, 64)
				*want.into = n * 1024
			}
		}
	}
	return total, available
}

func utsString(f [65]int8) string {
	b := make([]byte, 0, len(f))
	for _, c := range f {
		if c == 0 {
			break
		}
		b = append(b, byte(c))
	}
	return string(b)
}
