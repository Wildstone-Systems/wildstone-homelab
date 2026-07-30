//go:build linux

package agent

import (
	"bufio"
	"os"
	"runtime"
	"strconv"
	"strings"
	"syscall"

	wsv1 "github.com/chickenrat17/wildstone-homelab/proto/gen/wardstone/v1"
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
	return &wsv1.OpResult_HostFacts{HostFacts: &wsv1.HostFactsResult{
		Hostname:           hostname,
		Os:                 runtime.GOOS,
		Kernel:             kernel,
		NumCpu:             uint32(runtime.NumCPU()),
		MemTotalBytes:      memTotalBytes(),
		RootDiskTotalBytes: diskTotal,
		RootDiskFreeBytes:  diskFree,
		AgentVersion:       a.cfg.Version,
	}}, nil
}

func memTotalBytes() uint64 {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if kb, ok := strings.CutPrefix(sc.Text(), "MemTotal:"); ok {
			fields := strings.Fields(kb)
			if len(fields) >= 1 {
				n, _ := strconv.ParseUint(fields[0], 10, 64)
				return n * 1024
			}
		}
	}
	return 0
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
