package pkgtool

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// BuildArchive writes a DETERMINISTIC tar.gz of the package directory to
// w and returns the hex sha256 of the compressed bytes (ADR-W010 A2):
// paths sorted, timestamps/uids/gids/usernames zeroed, modes normalized
// (0644 files, 0755 dirs), no external metadata — so the digest is
// reproducible from any checkout of the same content, which is what makes
// the signature meaningful.
func BuildArchive(dir string, w io.Writer) (string, error) {
	var paths []string
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		paths = append(paths, filepath.ToSlash(rel))
		return nil
	})
	if err != nil {
		return "", fmt.Errorf("pkgtool: walking %s: %w", dir, err)
	}
	sort.Strings(paths)

	h := sha256.New()
	gz := gzip.NewWriter(io.MultiWriter(w, h))
	tw := tar.NewWriter(gz)
	for _, rel := range paths {
		data, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(rel)))
		if err != nil {
			return "", fmt.Errorf("pkgtool: reading %s: %w", rel, err)
		}
		if err := tw.WriteHeader(&tar.Header{
			Name: rel,
			Mode: 0o644,
			Size: int64(len(data)),
			// ModTime, Uid, Gid, Uname, Gname deliberately zero.
		}); err != nil {
			return "", fmt.Errorf("pkgtool: tar header %s: %w", rel, err)
		}
		if _, err := tw.Write(data); err != nil {
			return "", fmt.Errorf("pkgtool: tar write %s: %w", rel, err)
		}
	}
	if err := tw.Close(); err != nil {
		return "", fmt.Errorf("pkgtool: closing tar: %w", err)
	}
	if err := gz.Close(); err != nil {
		return "", fmt.Errorf("pkgtool: closing gzip: %w", err)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// maxExtractBytes bounds a single extracted file — packages are YAML and
// docs, not media; a multi-megabyte member is wrong before it is unsafe.
const maxExtractBytes = 4 << 20

// ExtractArchive unpacks an archive produced by BuildArchive into dir.
// Member paths are confined to dir (no absolute paths, no "..", no
// links — the archive format simply has no link members).
func ExtractArchive(r io.Reader, dir string) error {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return fmt.Errorf("pkgtool: gzip: %w", err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return fmt.Errorf("pkgtool: tar: %w", err)
		}
		if hdr.Typeflag != tar.TypeReg {
			return fmt.Errorf("pkgtool: archive member %q has type %d; only regular files are allowed", hdr.Name, hdr.Typeflag)
		}
		name := filepath.ToSlash(hdr.Name)
		if strings.HasPrefix(name, "/") || strings.Contains(name, "..") {
			return fmt.Errorf("pkgtool: archive member %q escapes the target directory", hdr.Name)
		}
		if hdr.Size > maxExtractBytes {
			return fmt.Errorf("pkgtool: archive member %q is %d bytes (cap %d)", hdr.Name, hdr.Size, maxExtractBytes)
		}
		dest := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
			return err
		}
		data, err := io.ReadAll(io.LimitReader(tr, maxExtractBytes+1))
		if err != nil {
			return fmt.Errorf("pkgtool: extracting %s: %w", name, err)
		}
		if len(data) > maxExtractBytes {
			return fmt.Errorf("pkgtool: archive member %q exceeds the %d byte cap", name, maxExtractBytes)
		}
		if err := os.WriteFile(dest, data, 0o644); err != nil {
			return err
		}
	}
}
