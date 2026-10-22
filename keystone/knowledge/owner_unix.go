//go:build unix

package knowledge

import (
	"io/fs"
	"os"
	"syscall"
)

// noFollow keeps an open from following a symlink at the leaf. The
// Lstat walk has already refused links; this closes the window between
// that walk and the open.
const noFollow = syscall.O_NOFOLLOW

// ownerOfFile reports the owner a new file should be given so it matches
// its neighbours.
//
// Keystone runs as root, and a web UI over the same folder usually does
// not: SilverBullet, for one, runs as whoever owns the folder. A note
// Keystone wrote as root would be one that UI can read but never save.
// When the process is not root it cannot give files away, and does not
// try.
func ownerOfFile(fi fs.FileInfo) (int, int, bool) {
	if os.Geteuid() != 0 {
		return 0, 0, false
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, 0, false
	}
	return int(st.Uid), int(st.Gid), true
}
