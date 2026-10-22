package pkgtool

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// maxArchiveBytes bounds a fetched archive; packages are YAML and docs.
const maxArchiveBytes = 16 << 20

// fetchClient is the bounded HTTP client for registry fetches.
var fetchClient = &http.Client{Timeout: 30 * time.Second}

// FetchPackage resolves name@constraint against the registry served at
// baseURL (any static file server over an ADR-W010 A3 directory), fetches
// the archive, verifies digest + publisher signature, and extracts it
// into a fresh directory under destRoot. An archive that fails
// verification never touches destRoot (ADR-W010 A2).
func FetchPackage(baseURL, name, constraint string, pub ed25519.PublicKey, destRoot string) (string, error) {
	idxData, err := fetchBytes(baseURL + "/" + indexFile)
	if err != nil {
		return "", fmt.Errorf("pkgtool: fetching index: %w", err)
	}
	var idx Index
	if err := json.Unmarshal(idxData, &idx); err != nil {
		return "", fmt.Errorf("pkgtool: parsing fetched index: %w", err)
	}
	entry, err := idx.Resolve(name, constraint)
	if err != nil {
		return "", err
	}
	// The index is data from the network: confine the archive path to a
	// simple basename before joining it into a URL.
	if strings.ContainsAny(entry.Path, "/\\") || strings.Contains(entry.Path, "..") {
		return "", fmt.Errorf("pkgtool: index entry %s@%s has a non-basename archive path %q", entry.Name, entry.Version, entry.Path)
	}
	archive, err := fetchBytes(baseURL + "/" + url.PathEscape(entry.Path))
	if err != nil {
		return "", fmt.Errorf("pkgtool: fetching archive: %w", err)
	}
	sum := sha256.Sum256(archive)
	if err := VerifyEntry(entry, hex.EncodeToString(sum[:]), pub); err != nil {
		return "", err
	}
	dest, err := os.MkdirTemp(destRoot, entry.Name+"-"+entry.Version+"-")
	if err != nil {
		return "", err
	}
	if err := ExtractArchive(strings.NewReader(string(archive)), dest); err != nil {
		os.RemoveAll(dest)
		return "", err
	}
	return dest, nil
}

func fetchBytes(rawURL string) ([]byte, error) {
	resp, err := fetchClient.Get(rawURL)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: %s", rawURL, resp.Status)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxArchiveBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxArchiveBytes {
		return nil, fmt.Errorf("GET %s: body exceeds %d bytes", rawURL, maxArchiveBytes)
	}
	return data, nil
}
