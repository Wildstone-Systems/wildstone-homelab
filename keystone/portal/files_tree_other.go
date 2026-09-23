//go:build !linux

package portal

import "os"

// Keystone serves storage on Linux. Do not silently weaken the no-follow or
// atomic no-replace guarantees on development platforms without these calls.
func openChildRoot(*os.Root, string) (*os.Root, error)                       { return nil, os.ErrInvalid }
func openRegular(*os.Root, string) (*os.File, error)                         { return nil, os.ErrInvalid }
func renameNoReplace(*os.Root, string, *os.Root, string, func() error) error { return os.ErrInvalid }
