// Package research defines the Owner-managed provider registry (ADR-W047).
// Selection is configuration, never tool or disclosure authority.
package research

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"regexp"
	"strings"
	"unicode"

	"github.com/chickenrat17/wildstone-homelab/keystone/redact"
)

var ErrConflict = errors.New("research configuration changed; reload and propose again")

type ProviderKind string

const (
	DDGS    ProviderKind = "ddgs"
	SearXNG ProviderKind = "searxng"
	Tavily  ProviderKind = "tavily"
)

type Limits struct {
	SearchesPerTask       int `json:"searchesPerTask"`
	FetchesPerTask        int `json:"fetchesPerTask"`
	RequestTimeoutSeconds int `json:"requestTimeoutSeconds"`
	DownloadBytes         int `json:"downloadBytes"`
	ExtractedCharacters   int `json:"extractedCharacters"`
	CacheSeconds          int `json:"cacheSeconds"`
}

func DefaultLimits() Limits { return Limits{10, 20, 30, 5 * 1024 * 1024, 50000, 900} }

type Config struct {
	Name          string       `json:"name"`
	Kind          ProviderKind `json:"kind"`
	EndpointRef   string       `json:"endpointRef,omitempty"`
	CredentialRef string       `json:"credentialRef,omitempty"`
	Backends      []string     `json:"backends"`
	FallbackOrder []string     `json:"fallbackOrder"`
	Limits        *Limits      `json:"limits"`
}

// DecodeConfig is the strict Owner-input boundary. Omission selects defaults;
// explicit null, unknown properties and nonpositive configured limits refuse.
func DecodeConfig(raw []byte) (Config, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return Config{}, errors.New("invalid search provider configuration")
	}
	if fields == nil {
		return Config{}, errors.New("configuration must be an object")
	}
	for _, value := range fields {
		if bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return Config{}, errors.New("configuration properties cannot be null")
		}
	}
	var config Config
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&config); err != nil {
		return Config{}, errors.New("invalid search provider configuration")
	}
	return config.Normalize()
}

type Provider struct {
	Config
	ID                   string `json:"id"`
	ConfigRevision       string `json:"configRevision"`
	State                string `json:"state"`
	TestedConfigRevision string `json:"testedConfigRevision,omitempty"`
}
type Selection struct {
	ProviderID     *string `json:"providerId"`
	ConfigRevision string  `json:"configRevision"`
}

// Change is the exact configuration reviewed in a normal Owner Plan. Empty
// ExpectedRevision means create only; it never means unconditional overwrite.
type Change struct {
	Operation        string  `json:"operation"`
	ProviderID       string  `json:"providerId"`
	ExpectedRevision string  `json:"expectedConfigRevision"`
	Config           *Config `json:"config,omitempty"`
}

var refName = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,62}$`)

func validRef(s, prefix string) bool {
	return strings.HasPrefix(s, prefix) && refName.MatchString(strings.TrimPrefix(s, prefix))
}
func NewID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	// Dotted groups preserve opaque 128-bit identity through the existing
	// public-plane secret redactor without exempting secret-shaped values.
	return "sr." + hex.EncodeToString(b[0:4]) + "." + hex.EncodeToString(b[4:8]) + "." + hex.EncodeToString(b[8:12]) + "." + hex.EncodeToString(b[12:16])
}

// Normalize admits only the backend supported by the pinned DDGS worker.
// Reserved kinds are extension seams, not enabled implementations. References
// cannot contain URLs, userinfo, query strings, credentials or host paths.
func (c Config) Normalize() (Config, error) {
	fail := func() (Config, error) { return Config{}, errors.New("invalid search provider configuration") }
	if c.Kind != DDGS {
		return Config{}, errors.New("unsupported search provider kind")
	}
	if strings.TrimSpace(c.Name) != c.Name || len(c.Name) == 0 || len(c.Name) > 128 || strings.IndexFunc(c.Name, unicode.IsControl) >= 0 || redact.String(c.Name) != c.Name {
		return fail()
	}
	// Owner review and complete-replacement edits must see the exact reference.
	// Do not exempt service names from the public-plane secret-value redactor.
	if c.EndpointRef != "" && (!validRef(c.EndpointRef, "service/") || redact.String(c.EndpointRef) != c.EndpointRef) {
		return fail()
	}
	if c.CredentialRef != "" && !validRef(c.CredentialRef, "secret/") {
		return fail()
	}
	if c.Backends == nil {
		c.Backends = []string{"duckduckgo"}
	}
	if len(c.Backends) != 1 || c.Backends[0] != "duckduckgo" {
		return Config{}, errors.New("unsupported DDGS backend; pinned worker supports duckduckgo only")
	}
	if c.FallbackOrder == nil {
		c.FallbackOrder = []string{}
	}
	if len(c.FallbackOrder) != 0 {
		return Config{}, errors.New("no additional fallback backend is supported by the pinned worker")
	}
	if c.Limits == nil {
		defaults := DefaultLimits()
		c.Limits = &defaults
	}
	l := c.Limits
	if l.SearchesPerTask < 1 || l.SearchesPerTask > 100 || l.FetchesPerTask < 1 || l.FetchesPerTask > 200 || l.RequestTimeoutSeconds < 1 || l.RequestTimeoutSeconds > 30 || l.DownloadBytes < 1 || l.DownloadBytes > 5*1024*1024 || l.ExtractedCharacters < 1 || l.ExtractedCharacters > 50000 || l.CacheSeconds < 1 || l.CacheSeconds > 900 {
		return fail()
	}
	return c, nil
}
