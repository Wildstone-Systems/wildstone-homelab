//go:build unix

package runtimedb

import (
	"golang.org/x/sys/unix"
	"os"
)

func lockCoordinatorFile(f *os.File) error {
	return unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB)
}
