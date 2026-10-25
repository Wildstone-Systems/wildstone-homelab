// Package playbook verifies the signed host playbook set before Wardstone
// (or the installer) runs any of it (W050 plan §6.1, ADR-W050 §2.7).
//
// A release ships a directory of playbooks, a manifest naming every file
// with its sha256, and an ed25519 signature over the manifest. Verify
// refuses the whole set if the signature is wrong, a listed file is
// missing or changed, or the directory holds a file the manifest does not
// list: a playbook nobody signed never runs, and neither does a role file
// slipped in beside a signed one.
package playbook

import (
	"bufio"
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Header is the manifest's first line; it versions the format and
// domain-separates the signature from every other thing the same key
// signs.
const Header = "wildstone-playbooks-manifest-v1"

// ManifestFile and SignatureFile sit at the root of a playbook directory.
// They are the only files Verify does not expect the manifest to list.
const (
	ManifestFile  = "playbooks.manifest"
	SignatureFile = "playbooks.manifest.sig"
)

// ErrUnsigned, ErrTampered and ErrUnlisted name why a set was refused.
var (
	ErrUnsigned = errors.New("playbook: manifest signature does not verify")
	ErrTampered = errors.New("playbook: a listed file is missing or changed")
	ErrUnlisted = errors.New("playbook: a file is not in the signed manifest")
)

// Build returns the manifest for every regular file under dir, sorted by
// path, excluding the manifest and its signature.
func Build(dir string) ([]byte, error) {
	files, err := walk(dir)
	if err != nil {
		return nil, err
	}
	var b bytes.Buffer
	b.WriteString(Header + "\n")
	for _, rel := range files {
		sum, err := fileSum(filepath.Join(dir, rel))
		if err != nil {
			return nil, err
		}
		fmt.Fprintf(&b, "%s  %s\n", sum, rel)
	}
	return b.Bytes(), nil
}

// Sign returns the signature file's contents for a manifest: the ed25519
// signature, hex-encoded, so the file is text and survives a newline.
func Sign(priv ed25519.PrivateKey, manifest []byte) []byte {
	return []byte(hex.EncodeToString(ed25519.Sign(priv, manifest)) + "\n")
}

// Verify checks dir's manifest and signature against pub, then every file
// against the manifest. It returns nil only when all of it holds.
func Verify(dir string, pub ed25519.PublicKey) error {
	if len(pub) != ed25519.PublicKeySize {
		return fmt.Errorf("playbook: public key has %d bytes, want %d", len(pub), ed25519.PublicKeySize)
	}
	manifest, err := os.ReadFile(filepath.Join(dir, ManifestFile))
	if err != nil {
		return fmt.Errorf("playbook: reading manifest: %w", err)
	}
	sig, err := os.ReadFile(filepath.Join(dir, SignatureFile))
	if err != nil {
		return fmt.Errorf("playbook: reading signature: %w", err)
	}
	raw, err := hex.DecodeString(string(bytes.TrimSpace(sig)))
	if err != nil || !ed25519.Verify(pub, manifest, raw) {
		return ErrUnsigned
	}
	listed, err := parse(manifest)
	if err != nil {
		return err
	}
	present, err := walk(dir)
	if err != nil {
		return err
	}
	for _, rel := range present {
		if _, ok := listed[rel]; !ok {
			return fmt.Errorf("%w: %s", ErrUnlisted, rel)
		}
	}
	for rel, want := range listed {
		got, err := fileSum(filepath.Join(dir, rel))
		if err != nil || got != want {
			return fmt.Errorf("%w: %s", ErrTampered, rel)
		}
	}
	return nil
}

func parse(manifest []byte) (map[string]string, error) {
	sc := bufio.NewScanner(bytes.NewReader(manifest))
	if !sc.Scan() || sc.Text() != Header {
		return nil, fmt.Errorf("playbook: manifest does not start with %q", Header)
	}
	out := map[string]string{}
	for sc.Scan() {
		sum, rel, ok := strings.Cut(sc.Text(), "  ")
		if !ok || len(sum) != 64 || rel == "" || !filepath.IsLocal(rel) {
			return nil, fmt.Errorf("playbook: malformed manifest line %q", sc.Text())
		}
		if _, dup := out[rel]; dup {
			return nil, fmt.Errorf("playbook: %s is listed twice", rel)
		}
		out[rel] = sum
	}
	return out, sc.Err()
}

// walk lists regular files under dir as slash paths, sorted. A symlink or
// any other non-regular file refuses the set: the manifest can only vouch
// for bytes it hashed.
func walk(dir string) ([]string, error) {
	var out []string
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if rel == ManifestFile || rel == SignatureFile {
			return nil
		}
		if !d.Type().IsRegular() {
			return fmt.Errorf("%w: %s is not a regular file", ErrUnlisted, rel)
		}
		out = append(out, rel)
		return nil
	})
	sort.Strings(out)
	return out, err
}

func fileSum(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}
