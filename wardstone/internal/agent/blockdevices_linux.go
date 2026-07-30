//go:build linux

package agent

// ListBlockDevices parses /sys/block directly — no lsblk, no shell-out
// (ADR-W003 §A4.5, W6c). loop* and ram* devices are skipped (they are not
// real storage a homelab operator would mount).

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"

	wsv1 "github.com/chickenrat17/wildstone-homelab/proto/gen/wardstone/v1"
)

// sysBlockPath is the /sys/block root. Package-level var so tests can point
// it at a fixture directory built in t.TempDir().
var sysBlockPath = "/sys/block"

func (a *Agent) listBlockDevices() (*wsv1.OpResult_ListBlockDevices, *wsv1.OpError) {
	entries, err := os.ReadDir(sysBlockPath)
	if err != nil {
		if os.IsNotExist(err) {
			return &wsv1.OpResult_ListBlockDevices{ListBlockDevices: &wsv1.ListBlockDevicesResult{}}, nil
		}
		return nil, opErr(wsv1.ErrorCode_ERROR_CODE_RUNTIME, "read %s: %v", sysBlockPath, err)
	}
	var devices []*wsv1.BlockDevice
	for _, e := range entries {
		name := e.Name()
		if strings.HasPrefix(name, "loop") || strings.HasPrefix(name, "ram") {
			continue
		}
		sizeSectors := readUintFile(filepath.Join(sysBlockPath, name, "size"))
		removable := readUintFile(filepath.Join(sysBlockPath, name, "removable")) == 1
		devices = append(devices, &wsv1.BlockDevice{
			Name:      name,
			SizeBytes: sizeSectors * 512, // /sys/block size is always in 512-byte sectors
			Removable: removable,
		})
	}
	return &wsv1.OpResult_ListBlockDevices{ListBlockDevices: &wsv1.ListBlockDevicesResult{Devices: devices}}, nil
}

func readUintFile(path string) uint64 {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	n, _ := strconv.ParseUint(strings.TrimSpace(string(b)), 10, 64)
	return n
}
