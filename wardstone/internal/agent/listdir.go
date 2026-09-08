package agent

// Directory listing for the install form's folder picker (protocol minor
// 15).
//
// A read primitive over the host filesystem is worth being careful about,
// so this one is deliberately the narrowest thing that answers the
// question "which folder do you mean":
//
//   - Confined by resolveManaged, the same check every other path-taking
//     op uses. That is not only a safety boundary, it is the honest one:
//     a bind mount outside the managed prefixes is refused at apply time,
//     so a picker that browsed the whole host would offer folders that
//     can never work.
//   - Directories only. Nothing in a package binds a single file.
//   - Names only. No sizes, no modes, no owners — none of it is needed to
//     choose a folder, and each would be a fact about the host leaving
//     the host.
//   - Symlinks are neither followed nor listed. A link is a second name
//     for a directory resolveManaged may or may not admit, and offering
//     one is offering a path that fails later.

import (
	"context"
	"os"
	"sort"

	wsv1 "github.com/chickenrat17/wildstone-homelab/proto/gen/wardstone/v1"
)

func (a *Agent) listDirectory(_ context.Context, op *wsv1.ListDirectory) (*wsv1.OpResult_ListDirectory, *wsv1.OpError) {
	dir, oerr := a.resolveManaged(op.GetPath())
	if oerr != nil {
		return nil, oerr
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			// An empty answer, not an error: a folder that does not exist
			// yet is a legitimate thing to be pointed at, and the picker
			// says so better than a failed request does.
			return &wsv1.OpResult_ListDirectory{ListDirectory: &wsv1.ListDirectoryResult{}}, nil
		}
		return nil, opErr(wsv1.ErrorCode_ERROR_CODE_RUNTIME, "list %s: %v", op.GetPath(), err)
	}
	var dirs []string
	for _, e := range entries {
		// IsDir() is false for a symlink even when it points at one,
		// which is exactly the behaviour wanted here.
		if e.IsDir() {
			dirs = append(dirs, e.Name())
		}
	}
	sort.Strings(dirs)
	return &wsv1.OpResult_ListDirectory{ListDirectory: &wsv1.ListDirectoryResult{Directories: dirs}}, nil
}
