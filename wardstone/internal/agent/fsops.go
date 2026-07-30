package agent

// Pure-filesystem operations: SnapshotDir and MeasureUsage. No Docker
// involved. Kept in their own file so ops.go does not bloat (ADR-W003 W6b).

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	wsv1 "github.com/chickenrat17/wildstone-homelab/proto/gen/wardstone/v1"
)

// maxSnapshotBytes caps the cumulative size of regular files a SnapshotDir
// will archive (ADR-W003 §A4.4 bounded responses, applied here to bounded
// *disk writes* since a snapshot could otherwise grow without limit).
const maxSnapshotBytes = 4 << 30 // 4 GiB

// maxUsageWalkEntries caps how many directory entries MeasureUsage will
// visit before giving up — protects the agent from an unbounded scan of a
// pathologically large or maliciously grown tree (ADR-W003 §A4.4).
const maxUsageWalkEntries = 1_000_000

// snapshotDir archives source_dir into a gzip'd tar written atomically to
// dest_path. Both paths must resolve under a managed prefix.
//
// Symlinks encountered inside source_dir are archived as tar link entries
// (their target string only) and are NEVER followed/dereferenced: doing so
// would let a symlink planted inside the managed tree pull arbitrary
// content from anywhere the agent process can read into the snapshot,
// defeating the managed-prefix invariant through the back door. Sockets,
// devices, and named pipes are skipped silently — they are not meaningful
// to archive and reading them could block or error unpredictably.
func (a *Agent) snapshotDir(op *wsv1.SnapshotDir) (*wsv1.OpResult_SnapshotDir, *wsv1.OpError) {
	src, oerr := a.resolveManaged(op.GetSourceDir())
	if oerr != nil {
		return nil, oerr
	}
	dest, oerr := a.resolveManaged(op.GetDestPath())
	if oerr != nil {
		return nil, oerr
	}
	if !strings.HasSuffix(dest, ".tar.gz") {
		return nil, opErr(wsv1.ErrorCode_ERROR_CODE_INVALID, "dest_path %q must end in .tar.gz", op.GetDestPath())
	}

	info, err := os.Lstat(src)
	if os.IsNotExist(err) {
		return nil, opErr(wsv1.ErrorCode_ERROR_CODE_NOT_FOUND, "source_dir %q", op.GetSourceDir())
	}
	if err != nil {
		return nil, opErr(wsv1.ErrorCode_ERROR_CODE_RUNTIME, "stat source_dir: %v", err)
	}
	if !info.IsDir() {
		return nil, opErr(wsv1.ErrorCode_ERROR_CODE_INVALID, "source_dir %q is not a directory", op.GetSourceDir())
	}

	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return nil, opErr(wsv1.ErrorCode_ERROR_CODE_RUNTIME, "mkdir dest parent: %v", err)
	}
	tmp := dest + ".wildstone-tmp"

	fileCount, size, sum, oerr := writeSnapshot(tmp, src)
	if oerr != nil {
		os.Remove(tmp) // a partial tmp file must never become dest
		return nil, oerr
	}
	if err := os.Rename(tmp, dest); err != nil {
		os.Remove(tmp)
		return nil, opErr(wsv1.ErrorCode_ERROR_CODE_RUNTIME, "rename: %v", err)
	}

	return &wsv1.OpResult_SnapshotDir{SnapshotDir: &wsv1.SnapshotDirResult{
		Sha256Hex: hex.EncodeToString(sum),
		SizeBytes: uint64(size),
		FileCount: uint32(fileCount),
	}}, nil
}

// writeSnapshot walks src (lexical order, via WalkDir) and writes a gzip'd
// tar of it to tmpPath, streaming a sha256 of the final compressed bytes
// as they are written. Returns the regular-file count, final file size,
// and sha256 digest. On any error the caller is responsible for removing
// tmpPath.
func writeSnapshot(tmpPath, src string) (fileCount int, size int64, sum []byte, oerr *wsv1.OpError) {
	f, err := os.OpenFile(tmpPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return 0, 0, nil, opErr(wsv1.ErrorCode_ERROR_CODE_RUNTIME, "create tmp: %v", err)
	}
	defer f.Close()

	h := sha256.New()
	gz := gzip.NewWriter(io.MultiWriter(f, h))
	tw := tar.NewWriter(gz)

	var cumulative int64
	walkErr := filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil // don't archive the root as an entry
		}
		info, err := d.Info()
		if err != nil {
			return err
		}

		switch {
		case d.Type()&os.ModeSymlink != 0:
			target, err := os.Readlink(path)
			if err != nil {
				return err
			}
			hdr, err := tar.FileInfoHeader(info, target)
			if err != nil {
				return err
			}
			hdr.Name = filepath.ToSlash(rel)
			hdr.Typeflag = tar.TypeSymlink
			return tw.WriteHeader(hdr)
		case d.IsDir():
			hdr, err := tar.FileInfoHeader(info, "")
			if err != nil {
				return err
			}
			hdr.Name = filepath.ToSlash(rel) + "/"
			return tw.WriteHeader(hdr)
		case info.Mode().IsRegular():
			cumulative += info.Size()
			if cumulative > maxSnapshotBytes {
				return errSnapshotTooLarge
			}
			hdr, err := tar.FileInfoHeader(info, "")
			if err != nil {
				return err
			}
			hdr.Name = filepath.ToSlash(rel)
			if err := tw.WriteHeader(hdr); err != nil {
				return err
			}
			fh, err := os.Open(path)
			if err != nil {
				return err
			}
			defer fh.Close()
			if _, err := io.Copy(tw, fh); err != nil {
				return err
			}
			fileCount++
			return nil
		default:
			// socket, device, named pipe, etc: skip silently.
			return nil
		}
	})
	if walkErr == errSnapshotTooLarge {
		return 0, 0, nil, opErr(wsv1.ErrorCode_ERROR_CODE_INVALID,
			"source_dir exceeds the %d byte snapshot cap", maxSnapshotBytes)
	}
	if walkErr != nil {
		return 0, 0, nil, opErr(wsv1.ErrorCode_ERROR_CODE_RUNTIME, "walk: %v", walkErr)
	}
	if err := tw.Close(); err != nil {
		return 0, 0, nil, opErr(wsv1.ErrorCode_ERROR_CODE_RUNTIME, "tar close: %v", err)
	}
	if err := gz.Close(); err != nil {
		return 0, 0, nil, opErr(wsv1.ErrorCode_ERROR_CODE_RUNTIME, "gzip close: %v", err)
	}
	if err := f.Sync(); err != nil {
		return 0, 0, nil, opErr(wsv1.ErrorCode_ERROR_CODE_RUNTIME, "sync: %v", err)
	}
	st, err := f.Stat()
	if err != nil {
		return 0, 0, nil, opErr(wsv1.ErrorCode_ERROR_CODE_RUNTIME, "stat tmp: %v", err)
	}
	return fileCount, st.Size(), h.Sum(nil), nil
}

// errSnapshotTooLarge is a sentinel returned by the WalkDir callback to
// abort the walk early once the cumulative-bytes cap is exceeded.
var errSnapshotTooLarge = fsError("snapshot exceeds size cap")

type fsError string

func (e fsError) Error() string { return string(e) }

// measureUsage recursively sums regular-file sizes under path (managed
// prefix required). Symlinks are not followed. The walk aborts with a
// RUNTIME error if it visits more than maxUsageWalkEntries entries.
func (a *Agent) measureUsage(op *wsv1.MeasureUsage) (*wsv1.OpResult_MeasureUsage, *wsv1.OpError) {
	path, oerr := a.resolveManaged(op.GetPath())
	if oerr != nil {
		return nil, oerr
	}
	if _, err := os.Lstat(path); os.IsNotExist(err) {
		return nil, opErr(wsv1.ErrorCode_ERROR_CODE_NOT_FOUND, "path %q", op.GetPath())
	} else if err != nil {
		return nil, opErr(wsv1.ErrorCode_ERROR_CODE_RUNTIME, "stat: %v", err)
	}

	var totalBytes uint64
	var fileCount uint64
	var entries int
	walkErr := filepath.WalkDir(path, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		entries++
		if entries > maxUsageWalkEntries {
			return errUsageWalkTooLarge
		}
		if d.Type()&os.ModeSymlink != 0 {
			return nil // not followed
		}
		if d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if info.Mode().IsRegular() {
			totalBytes += uint64(info.Size())
			fileCount++
		}
		return nil
	})
	if walkErr == errUsageWalkTooLarge {
		return nil, opErr(wsv1.ErrorCode_ERROR_CODE_RUNTIME, "usage walk exceeded 1M entries")
	}
	if walkErr != nil {
		return nil, opErr(wsv1.ErrorCode_ERROR_CODE_RUNTIME, "walk: %v", walkErr)
	}
	return &wsv1.OpResult_MeasureUsage{MeasureUsage: &wsv1.MeasureUsageResult{
		TotalBytes: totalBytes,
		FileCount:  fileCount,
	}}, nil
}

var errUsageWalkTooLarge = fsError("usage walk exceeded 1M entries")
