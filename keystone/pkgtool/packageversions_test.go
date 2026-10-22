package pkgtool

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// A package's version must move when its content does (F91).
//
// This is not bookkeeping. An installed capability records the package
// and version it was expanded from (ADR-W021 A2), and that record is the
// ONLY thing that can say whether what is running matches what the
// catalog now holds. When content changes under an unchanged version the
// two are indistinguishable, and nothing — not the Console, not the
// reconciler, not an operator — can tell a capability that needs
// re-installing from one that is current.
//
// That is not hypothetical. The dashboard package changed across three
// commits while its version stayed 1.0.0. An operator upgraded, saw a
// dashboard identical to the one before, and had no way to distinguish
// "already current" from "needs re-installing" from "the upgrade
// failed" — and reasonably concluded the last.
//
// The lock file records what each version's content hashes to. Changing
// a package without bumping its version fails here, with the two things
// the author has to do spelled out.
const versionLockPath = "../../packages/versions.lock"

func TestPackageContentMatchesItsVersion(t *testing.T) {
	have := hashShippedPackages(t)
	want := readVersionLock(t)

	if os.Getenv("UPDATE_PACKAGE_LOCK") == "1" {
		writeVersionLock(t, have)
		t.Log("packages/versions.lock rewritten")
		return
	}

	for name, h := range have {
		prior, known := want[name]
		switch {
		case !known:
			t.Errorf("package %q is not in packages/versions.lock — add it by running:\n"+
				"    UPDATE_PACKAGE_LOCK=1 go test ./pkgtool/ -run PackageContentMatchesItsVersion", name)
		case prior.version == h.version && prior.sum != h.sum:
			t.Errorf("package %q changed but its version is still %s.\n"+
				"    Bump metadata.version, then run:\n"+
				"    UPDATE_PACKAGE_LOCK=1 go test ./pkgtool/ -run PackageContentMatchesItsVersion\n"+
				"    An installed capability records the version it was expanded from; leaving it\n"+
				"    unchanged makes a stale install indistinguishable from a current one.", name, h.version)
		case prior.version != h.version && prior.sum == h.sum:
			t.Errorf("package %q bumped its version to %s without changing anything", name, h.version)
		}
	}
	for name := range want {
		if _, still := have[name]; !still {
			t.Errorf("package %q is in the lock file but no longer in the catalog; re-run with UPDATE_PACKAGE_LOCK=1", name)
		}
	}
}

type pkgHash struct{ version, sum string }

func hashShippedPackages(t *testing.T) map[string]pkgHash {
	t.Helper()
	root, err := filepath.Abs("../../packages")
	if err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]pkgHash{}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(root, e.Name(), "package.yaml"))
		if err != nil {
			continue
		}
		sum := sha256.Sum256(raw)
		out[e.Name()] = pkgHash{version: versionOf(string(raw)), sum: hex.EncodeToString(sum[:])[:16]}
	}
	if len(out) == 0 {
		t.Fatal("no packages found — the walk is broken, not the catalog")
	}
	return out
}

func versionOf(raw string) string {
	for _, line := range strings.Split(raw, "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(line), "version:"); ok {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

func readVersionLock(t *testing.T) map[string]pkgHash {
	t.Helper()
	raw, err := os.ReadFile(versionLockPath)
	if err != nil {
		return map[string]pkgHash{}
	}
	out := map[string]pkgHash{}
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		f := strings.Fields(line)
		if len(f) == 3 {
			out[f[0]] = pkgHash{version: f[1], sum: f[2]}
		}
	}
	return out
}

func writeVersionLock(t *testing.T, have map[string]pkgHash) {
	t.Helper()
	names := make([]string, 0, len(have))
	for n := range have {
		names = append(names, n)
	}
	sort.Strings(names)
	var b strings.Builder
	b.WriteString("# What each shipped package's version currently contains (F91).\n")
	b.WriteString("#\n")
	b.WriteString("# An installed capability records the package version it was expanded\n")
	b.WriteString("# from, so a version that does not move when content does makes a stale\n")
	b.WriteString("# install indistinguishable from a current one.\n")
	b.WriteString("#\n")
	b.WriteString("# Regenerate:\n")
	b.WriteString("#   UPDATE_PACKAGE_LOCK=1 go test ./pkgtool/ -run PackageContentMatchesItsVersion\n")
	b.WriteString("#\n")
	b.WriteString("# name  version  sha256(package.yaml)[:16]\n")
	for _, n := range names {
		fmt.Fprintf(&b, "%s %s %s\n", n, have[n].version, have[n].sum)
	}
	if err := os.WriteFile(versionLockPath, []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
}
