//go:build unix

// Package datadir enforces Keystone's management data-directory ownership
// contract.
package datadir

import (
	"fmt"
	"os"
	"syscall"
)

// Secure keeps the host-install ownership contract (the Keystone user owns a
// 0700 directory) while also accepting the group-owned mount root that
// Kubernetes produces for a Pod fsGroup. It never changes an owner or group:
// kubelet/the storage driver must establish the mount before Keystone starts.
func Secure(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("securing data directory: %w", err)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("securing data directory: cannot inspect ownership")
	}
	uid, gid := int(stat.Uid), int(stat.Gid)
	if uid == os.Geteuid() {
		if err := os.Chmod(path, 0o700); err != nil {
			return fmt.Errorf("securing data directory: %w", err)
		}
		return nil
	}
	if !groupOwnedAllowed(info.Mode(), gid, os.Getegid()) {
		return fmt.Errorf(
			"data directory must be owned by uid %d with mode 0700, or by primary gid %d with mode 0770; got uid %d gid %d mode %04o",
			os.Geteuid(), os.Getegid(), uid, gid, info.Mode().Perm(),
		)
	}
	return nil
}

func groupOwnedAllowed(mode os.FileMode, gid, processGID int) bool {
	return gid == processGID && mode.Perm() == 0o770
}
