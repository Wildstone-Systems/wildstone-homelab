//go:build !linux

package workspace

import "os"

// Required kernel confinement is unavailable; never weaken it on other hosts.
func openRootNoSymlinks(string) (*os.Root, error)           { return nil, ErrFilesystem }
func openRootDescendant(*os.Root, string) (*os.Root, error) { return nil, ErrFilesystem }
