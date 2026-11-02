//go:build !unix

package datadir

import "os"

// Kubernetes management images are Linux-only. Preserve the existing
// owner-only behavior for other development platforms.
func Secure(path string) error {
	return os.Chmod(path, 0o700)
}
