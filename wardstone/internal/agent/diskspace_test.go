//go:build linux

package agent

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	wsv1 "github.com/Wildstone-Systems/wildstone-homelab/proto/gen/wardstone/v1"
)

// TestMountOf: the longest mount point that holds the path wins, so a
// path under a mounted share reports the share, and the same path with
// the share missing reports the disk underneath.
func TestMountOf(t *testing.T) {
	info := filepath.Join(t.TempDir(), "mountinfo")
	lines := "" +
		"22 1 8:1 / / rw,relatime shared:1 - ext4 /dev/sda1 rw\n" +
		"30 22 0:40 / /srv/wildstone rw shared:2 - xfs /dev/sdb1 rw\n" +
		"41 30 0:52 / /srv/wildstone/nas/media rw,nosuid,nodev shared:3 - nfs4 nas.example.internal:/media rw,vers=4.1,soft\n" +
		"42 30 0:53 / /srv/wildstone/nas/my\\040photos rw - nfs nas.example.internal:/p rw\n"
	if err := os.WriteFile(info, []byte(lines), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ path, mp, fstype string }{
		{"/srv/wildstone/nas/media/movies", "/srv/wildstone/nas/media", "nfs4"},
		{"/srv/wildstone/nas/media", "/srv/wildstone/nas/media", "nfs4"},
		{"/srv/wildstone/nas/mediaX", "/srv/wildstone", "xfs"}, // prefix of a name is not an ancestor
		{"/srv/wildstone/nas/other", "/srv/wildstone", "xfs"},  // share missing: the disk underneath
		{"/srv/wildstone/nas/my photos/2024", "/srv/wildstone/nas/my photos", "nfs"},
		{"/var/lib/docker/volumes/x/_data", "/", "ext4"},
	} {
		m, err := mountOf(info, tc.path)
		if err != nil {
			t.Fatal(err)
		}
		if m.point != tc.mp || m.fstype != tc.fstype {
			t.Errorf("mountOf(%q) = %q %q, want %q %q", tc.path, m.point, m.fstype, tc.mp, tc.fstype)
		}
	}
	m, _ := mountOf(info, "/srv/wildstone/nas/media/movies")
	if m.source != "nas.example.internal:/media" || strings.Join(m.options, ",") != "rw,nosuid,nodev,rw,vers=4.1,soft" {
		t.Errorf("share source/options = %q %v", m.source, m.options)
	}
}

// TestDiskSpace covers the op end to end: sizes from statfs, the mount
// the path lives on, a not-yet-created path answered for its parent, and
// the managed-prefix and volume-ownership boundaries.
func TestDiskSpace(t *testing.T) {
	info := filepath.Join(t.TempDir(), "mountinfo")
	managed := t.TempDir()
	volDir := t.TempDir()
	if err := os.WriteFile(info, []byte("22 1 8:1 / / rw - ext4 /dev/sda1 rw\n30 22 0:52 / "+managed+" rw - nfs4 nas:/x rw\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	oldInfo, oldStatfs := mountinfoPath, statfsFn
	mountinfoPath = info
	statfsFn = func(path string, st *syscall.Statfs_t) error {
		st.Bsize, st.Blocks, st.Bavail = 4096, 1000, 250
		return nil
	}
	t.Cleanup(func() { mountinfoPath, statfsFn = oldInfo, oldStatfs })

	fake := newFakeDocker(t)
	fake.addOwnedVolumeAt("photos-repo", volDir)
	fake.addForeignVolume("someone-elses")
	h := startGateway(t, []string{"tok"}, "")
	newTestAgent(t, h, "node-a", "tok", func(c *Config) {
		c.ManagedPrefixes = []string{managed}
		c.DockerEndpoint = fake.URL()
	})
	if !h.gw.WaitConnected("node-a", 10*time.Second) {
		t.Fatal("agent never connected")
	}
	ask := func(ds *wsv1.DiskSpace) *wsv1.OpResult {
		return dispatch(t, h, "node-a", &wsv1.Operation{Op: &wsv1.Operation_DiskSpace{DiskSpace: ds}})
	}

	res := ask(&wsv1.DiskSpace{Path: managed})
	d := res.GetDiskSpace()
	if d == nil || d.GetTotalBytes() != 4096*1000 || d.GetFreeBytes() != 4096*250 ||
		d.GetMountPoint() != managed || d.GetFstype() != "nfs4" || !d.GetExists() {
		t.Fatalf("managed path: %+v", res)
	}

	res = ask(&wsv1.DiskSpace{Path: filepath.Join(managed, "repo", "not-yet")})
	if d := res.GetDiskSpace(); d == nil || d.GetExists() || d.GetMountPoint() != managed {
		t.Fatalf("missing path: want its parent's filesystem with exists=false, got %+v", res)
	}

	res = ask(&wsv1.DiskSpace{Volume: "photos-repo"})
	if d := res.GetDiskSpace(); d == nil || d.GetMountPoint() != "/" || !d.GetExists() {
		t.Fatalf("owned volume: %+v", res)
	}

	for name, tc := range map[string]struct {
		op   *wsv1.DiskSpace
		code wsv1.ErrorCode
	}{
		"outside managed": {&wsv1.DiskSpace{Path: t.TempDir()}, wsv1.ErrorCode_ERROR_CODE_FORBIDDEN},
		"foreign volume":  {&wsv1.DiskSpace{Volume: "someone-elses"}, wsv1.ErrorCode_ERROR_CODE_FORBIDDEN},
		"absent volume":   {&wsv1.DiskSpace{Volume: "ghost"}, wsv1.ErrorCode_ERROR_CODE_NOT_FOUND},
		"both set":        {&wsv1.DiskSpace{Path: managed, Volume: "photos-repo"}, wsv1.ErrorCode_ERROR_CODE_INVALID},
		"neither set":     {&wsv1.DiskSpace{}, wsv1.ErrorCode_ERROR_CODE_INVALID},
	} {
		if got := ask(tc.op).GetError().GetCode(); got != tc.code {
			t.Errorf("%s: code %v, want %v", name, got, tc.code)
		}
	}
}

// TestMeasureUsageVolume: a wildstone-owned volume can be sized; a
// foreign one cannot.
func TestMeasureUsageVolume(t *testing.T) {
	volDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(volDir, "a"), []byte("1234567"), 0o644); err != nil {
		t.Fatal(err)
	}
	fake := newFakeDocker(t)
	fake.addOwnedVolumeAt("photos-data", volDir)
	fake.addForeignVolume("someone-elses")
	h := startGateway(t, []string{"tok"}, "")
	newTestAgent(t, h, "node-a", "tok", func(c *Config) { c.DockerEndpoint = fake.URL() })
	if !h.gw.WaitConnected("node-a", 10*time.Second) {
		t.Fatal("agent never connected")
	}
	res := dispatch(t, h, "node-a", &wsv1.Operation{Op: &wsv1.Operation_MeasureUsage{MeasureUsage: &wsv1.MeasureUsage{Volume: "photos-data"}}})
	if mu := res.GetMeasureUsage(); mu == nil || mu.GetTotalBytes() != 7 || mu.GetFileCount() != 1 {
		t.Fatalf("volume usage: %+v", res)
	}
	res = dispatch(t, h, "node-a", &wsv1.Operation{Op: &wsv1.Operation_MeasureUsage{MeasureUsage: &wsv1.MeasureUsage{Volume: "someone-elses"}}})
	if res.GetError().GetCode() != wsv1.ErrorCode_ERROR_CODE_FORBIDDEN {
		t.Fatalf("foreign volume usage: %+v", res)
	}
}
