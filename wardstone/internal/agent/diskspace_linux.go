//go:build linux

package agent

// DiskSpace (protocol minor 19, ADR-W045 A3.1/A3.2): how big and how full
// the filesystem holding a managed path or a wildstone-owned volume is,
// and which mount it is. Read-only — statfs and /proc/self/mountinfo, no
// write, no create.

import (
	"bufio"
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	wsv1 "github.com/Wildstone-Systems/wildstone-homelab/proto/gen/wardstone/v1"
)

// statfsFn is a seam over syscall.Statfs so tests can report a disk of
// any size without owning one.
var statfsFn = syscall.Statfs

func (a *Agent) diskSpace(ctx context.Context, op *wsv1.DiskSpace) (*wsv1.OpResult_DiskSpace, *wsv1.OpError) {
	path, oerr := a.diskSpaceTarget(ctx, op.GetPath(), op.GetVolume())
	if oerr != nil {
		return nil, oerr
	}
	// A repository directory restic has not created yet still has a
	// filesystem it will land on: answer for the nearest existing
	// ancestor, and say so.
	exists := true
	probe := path
	for {
		if _, err := os.Stat(probe); err == nil {
			break
		} else if !os.IsNotExist(err) {
			return nil, opErr(wsv1.ErrorCode_ERROR_CODE_RUNTIME, "stat %q: %v", probe, err)
		}
		exists = false
		parent := filepath.Dir(probe)
		if parent == probe {
			return nil, opErr(wsv1.ErrorCode_ERROR_CODE_NOT_FOUND, "no existing ancestor of %q", path)
		}
		probe = parent
	}
	var st syscall.Statfs_t
	if err := statfsFn(probe, &st); err != nil {
		return nil, opErr(wsv1.ErrorCode_ERROR_CODE_RUNTIME, "statfs %q: %v", probe, err)
	}
	m, err := mountOf(mountinfoPath, probe)
	if err != nil {
		return nil, opErr(wsv1.ErrorCode_ERROR_CODE_RUNTIME, "read mountinfo: %v", err)
	}
	bsize := uint64(st.Bsize) //nolint:gosec // block size is never negative
	return &wsv1.OpResult_DiskSpace{DiskSpace: &wsv1.DiskSpaceResult{
		TotalBytes: st.Blocks * bsize,
		FreeBytes:  st.Bavail * bsize,
		MountPoint: m.point,
		Fstype:     m.fstype,
		Exists:     exists,
		Source:     m.source,
		Options:    m.options,
	}}, nil
}

// mountEntry is one mountinfo line, as much of it as DiskSpace reports.
type mountEntry struct {
	point, fstype, source string
	options               []string
}

// mountOf returns the mount that holds path: the LONGEST mountinfo mount
// point that is path or one of its ancestors. Mount points in mountinfo
// escape space, tab, newline and backslash as octal (\040 and so on),
// which is decoded before comparing.
//
// A line is "ID parent major:minor root point mount-options [optional
// fields...] - fstype source super-options".
func mountOf(mountinfo, path string) (mountEntry, error) {
	f, err := os.Open(mountinfo)
	if err != nil {
		return mountEntry{}, err
	}
	defer f.Close()
	var best mountEntry
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 5 {
			continue
		}
		mp := unescapeMountinfo(fields[4])
		if !(path == mp || mp == "/" || strings.HasPrefix(path, mp+"/")) {
			continue
		}
		if len(mp) < len(best.point) {
			continue
		}
		e := mountEntry{point: mp}
		if len(fields) > 5 {
			e.options = strings.Split(fields[5], ",")
		}
		for i, fl := range fields {
			if fl == "-" {
				if i+1 < len(fields) {
					e.fstype = fields[i+1]
				}
				if i+2 < len(fields) {
					e.source = unescapeMountinfo(fields[i+2])
				}
				if i+3 < len(fields) {
					e.options = append(e.options, strings.Split(fields[i+3], ",")...)
				}
				break
			}
		}
		// Equal length is the same mount point mounted over itself: the
		// later line is the one on top, and the one path now reaches.
		best = e
	}
	return best, sc.Err()
}

func unescapeMountinfo(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+3 < len(s) {
			if n, err := strconv.ParseUint(s[i+1:i+4], 8, 8); err == nil {
				b.WriteByte(byte(n))
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}
