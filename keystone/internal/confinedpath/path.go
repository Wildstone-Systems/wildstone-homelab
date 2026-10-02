// Package confinedpath validates untrusted relative paths using ADR-W037's bounds.
package confinedpath

import (
	"errors"
	"strings"
	"unicode"
	"unicode/utf8"
)

const MaxPathDepth = 32
const MaxPathBytes = 4096
const MaxNameBytes = 255

var ErrPath = errors.New("invalid relative path")

// RelativePath is validated without cleaning: accepting a/../b as b hides a
// caller mistake and makes the policy differ between endpoints. Empty is the
// namespace root, accepted only by directory operations.
type RelativePath string

func ParseRelativePath(raw string) (RelativePath, error) {
	if !utf8.ValidString(raw) || len(raw) > MaxPathBytes || strings.Contains(raw, "\\") {
		return "", ErrPath
	}
	if raw == "" {
		return "", nil
	}
	parts := strings.Split(raw, "/")
	if len(parts) > MaxPathDepth {
		return "", ErrPath
	}
	for _, part := range parts {
		if part == "" || part == "." || part == ".." || strings.HasPrefix(part, ".") || len(part) > MaxNameBytes || strings.IndexFunc(part, unicode.IsControl) >= 0 {
			return "", ErrPath
		}
	}
	return RelativePath(raw), nil
}
