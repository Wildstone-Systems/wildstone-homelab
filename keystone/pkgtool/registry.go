package pkgtool

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/Wildstone-Systems/wildstone-homelab/keystone/pkgspec"
)

// IndexEntry is one published package version in a registry index
// (ADR-W010 A3).
type IndexEntry struct {
	Name    string `json:"name"`
	Version string `json:"version"`
	Sha256  string `json:"sha256"`
	Path    string `json:"path"` // archive path relative to the registry root
	Sig     string `json:"sig"`  // base64 ed25519 signature (ADR-W010 A2)
}

// Index is the whole registry index: a plain JSON file beside the
// archives, servable by any static file server — the registry is data,
// like packages themselves.
type Index struct {
	Packages []IndexEntry `json:"packages"`
}

const indexFile = "index.json"

// ReadIndex loads <root>/index.json; a missing file is an empty registry.
func ReadIndex(root string) (*Index, error) {
	data, err := os.ReadFile(filepath.Join(root, indexFile))
	if os.IsNotExist(err) {
		return &Index{}, nil
	}
	if err != nil {
		return nil, err
	}
	var idx Index
	if err := json.Unmarshal(data, &idx); err != nil {
		return nil, fmt.Errorf("pkgtool: parsing %s: %w", indexFile, err)
	}
	return &idx, nil
}

// writeIndex writes the index atomically (temp + rename).
func writeIndex(root string, idx *Index) error {
	data, err := json.MarshalIndent(idx, "", "  ")
	if err != nil {
		return err
	}
	tmp := filepath.Join(root, indexFile+".tmp")
	if err := os.WriteFile(tmp, append(data, '\n'), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(root, indexFile))
}

// Publish builds the deterministic archive for pkgDir, signs it, writes
// it into the registry root, and updates the index — replacing any
// existing entry for the same (name, version): republishing a version is
// an explicit overwrite, visible in the registry's own git history if
// the root is tracked.
func Publish(root, pkgDir string, priv ed25519.PrivateKey) (IndexEntry, error) {
	pkg, err := pkgspec.Load(filepath.Join(pkgDir, "package.yaml"))
	if err != nil {
		return IndexEntry{}, fmt.Errorf("pkgtool: publish: %w", err)
	}
	if _, err := parseSemver(pkg.Metadata.Version); err != nil {
		return IndexEntry{}, fmt.Errorf("pkgtool: publish %s: %w", pkg.Metadata.Name, err)
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return IndexEntry{}, err
	}

	archiveName := fmt.Sprintf("%s-%s.tgz", pkg.Metadata.Name, pkg.Metadata.Version)
	f, err := os.Create(filepath.Join(root, archiveName))
	if err != nil {
		return IndexEntry{}, err
	}
	digest, err := BuildArchive(pkgDir, f)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return IndexEntry{}, fmt.Errorf("pkgtool: building archive: %w", err)
	}

	entry := IndexEntry{
		Name:    pkg.Metadata.Name,
		Version: pkg.Metadata.Version,
		Sha256:  digest,
		Path:    archiveName,
		Sig:     base64.StdEncoding.EncodeToString(SignArchive(priv, digest)),
	}
	idx, err := ReadIndex(root)
	if err != nil {
		return IndexEntry{}, err
	}
	kept := idx.Packages[:0]
	for _, e := range idx.Packages {
		if !(e.Name == entry.Name && e.Version == entry.Version) {
			kept = append(kept, e)
		}
	}
	idx.Packages = append(kept, entry)
	if err := writeIndex(root, idx); err != nil {
		return IndexEntry{}, err
	}
	return entry, nil
}

// Resolve picks the highest version of name satisfying constraint
// (ADR-W010 A4): "" = highest available; an exact "X.Y.Z"; or "^N" = the
// highest with major N. No fuller range algebra until something needs it.
func (idx *Index) Resolve(name, constraint string) (IndexEntry, error) {
	var best *IndexEntry
	var bestV semver
	for i, e := range idx.Packages {
		if e.Name != name {
			continue
		}
		v, err := parseSemver(e.Version)
		if err != nil {
			return IndexEntry{}, fmt.Errorf("pkgtool: index entry %s@%s: %w", e.Name, e.Version, err)
		}
		switch {
		case constraint == "":
		case strings.HasPrefix(constraint, "^"):
			major, err := strconv.Atoi(strings.TrimPrefix(constraint, "^"))
			if err != nil {
				return IndexEntry{}, fmt.Errorf("pkgtool: bad constraint %q", constraint)
			}
			if v.major != major {
				continue
			}
		default:
			if e.Version != constraint {
				continue
			}
		}
		if best == nil || bestV.less(v) {
			best, bestV = &idx.Packages[i], v
		}
	}
	if best == nil {
		return IndexEntry{}, fmt.Errorf("pkgtool: no version of %q satisfies %q", name, constraint)
	}
	return *best, nil
}

// VerifyEntry checks an archive's bytes against its index entry: digest
// first, then the publisher signature over that digest. An archive that
// fails either never reaches extraction or Expand (ADR-W010 A2).
func VerifyEntry(entry IndexEntry, archiveSha256Hex string, pub ed25519.PublicKey) error {
	if archiveSha256Hex != entry.Sha256 {
		return fmt.Errorf("pkgtool: %s@%s: archive digest %.12s… does not match index %.12s…",
			entry.Name, entry.Version, archiveSha256Hex, entry.Sha256)
	}
	sig, err := base64.StdEncoding.DecodeString(entry.Sig)
	if err != nil {
		return fmt.Errorf("pkgtool: %s@%s: signature is not base64: %w", entry.Name, entry.Version, err)
	}
	if !VerifyArchive(pub, entry.Sha256, sig) {
		return fmt.Errorf("pkgtool: %s@%s: publisher signature does not verify", entry.Name, entry.Version)
	}
	return nil
}

// semver is the minimal three-integer version pkgtool needs (ADR-W010
// A4). Pre-release/build suffixes are rejected outright: launch packages
// are owner-published, and a closed grammar beats a permissive one.
type semver struct{ major, minor, patch int }

func parseSemver(s string) (semver, error) {
	parts := strings.Split(s, ".")
	if len(parts) != 3 {
		return semver{}, fmt.Errorf("version %q is not MAJOR.MINOR.PATCH", s)
	}
	var v semver
	for i, dst := range []*int{&v.major, &v.minor, &v.patch} {
		n, err := strconv.Atoi(parts[i])
		if err != nil || n < 0 || (len(parts[i]) > 1 && parts[i][0] == '0') {
			return semver{}, fmt.Errorf("version %q is not MAJOR.MINOR.PATCH", s)
		}
		*dst = n
	}
	return v, nil
}

func (a semver) less(b semver) bool {
	if a.major != b.major {
		return a.major < b.major
	}
	if a.minor != b.minor {
		return a.minor < b.minor
	}
	return a.patch < b.patch
}
