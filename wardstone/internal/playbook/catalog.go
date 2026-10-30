package playbook

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const (
	CatalogFile   = "runplaybooks.catalog"
	CatalogHeader = "wildstone-runplaybooks-catalog-v1"
)

// Entry is signed transitively: CatalogFile is hashed by the signed manifest.
// Schema is a closed identifier compiled into Wardstone, not a caller value.
type Entry struct {
	Identity string
	Version  string
	Schema   string
	File     string
}

// VerifyEntry verifies the complete set before reading its signed catalog and
// returns only an exact identity/version/schema match.
func VerifyEntry(dir string, pub []byte, identity, version, schema string) (Entry, error) {
	if err := Verify(dir, pub); err != nil {
		return Entry{}, err
	}
	b, err := os.ReadFile(filepath.Join(dir, CatalogFile))
	if err != nil {
		return Entry{}, fmt.Errorf("playbook: read signed catalog: %w", err)
	}
	entries, err := parseCatalog(b)
	if err != nil {
		return Entry{}, err
	}
	for _, e := range entries {
		if e.Identity == identity && e.Version == version && e.Schema == schema {
			info, err := os.Stat(filepath.Join(dir, e.File))
			if err != nil || !info.Mode().IsRegular() {
				return Entry{}, fmt.Errorf("playbook: signed catalog target %q is unavailable", e.File)
			}
			return e, nil
		}
	}
	return Entry{}, fmt.Errorf("playbook: identity/version/schema is not allowlisted")
}

func parseCatalog(b []byte) ([]Entry, error) {
	sc := bufio.NewScanner(bytes.NewReader(b))
	if !sc.Scan() || sc.Text() != CatalogHeader {
		return nil, fmt.Errorf("playbook: catalog does not start with %q", CatalogHeader)
	}
	var out []Entry
	seen := map[string]bool{}
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) != 4 || !filepath.IsLocal(fields[3]) || filepath.Ext(fields[3]) != ".yml" {
			return nil, fmt.Errorf("playbook: malformed catalog line %q", sc.Text())
		}
		key := strings.Join(fields[:3], "\x00")
		if seen[key] {
			return nil, fmt.Errorf("playbook: duplicate catalog entry %q", fields[0])
		}
		seen[key] = true
		out = append(out, Entry{Identity: fields[0], Version: fields[1], Schema: fields[2], File: fields[3]})
	}
	return out, sc.Err()
}
