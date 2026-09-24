package agent

import (
	"strconv"
	"strings"

	wsv1 "github.com/chickenrat17/wildstone-homelab/proto/gen/wardstone/v1"
)

// The H1 hardening fields (ADR-W039 Phase 6, protocol minor 18) narrow a
// container rather than widen it, which is why they carry no allowlist
// the way devices.go's do — the concern there is a package granting
// itself something host-shaped, and dropping a capability or capping
// memory can never do that. What DOES need checking here is syntax:
// Keystone already validates it (keystone/resource's dockerSizeRegexp,
// cpuCountRegexp), but a skewed Keystone or a hand-built op is possible,
// and a malformed value reaching Docker's own API would surface as an
// opaque 400 rather than a message naming the field.

// checkCapDrop mirrors checkCapability's syntax rule (no CAP_ prefix,
// uppercase) without its allowlist: removing a capability never grants
// the container anything, so there is nothing here for a package to
// escalate with.
func checkCapDrop(name string) *wsv1.OpError {
	if strings.HasPrefix(name, "CAP_") {
		return opErr(wsv1.ErrorCode_ERROR_CODE_INVALID,
			"capDrop %q: name it without the CAP_ prefix, as Docker does", name)
	}
	for _, r := range name {
		if !(r == '_' || (r >= 'A' && r <= 'Z')) {
			return opErr(wsv1.ErrorCode_ERROR_CODE_INVALID,
				"capDrop %q must be an uppercase capability name (or ALL)", name)
		}
	}
	if name == "" {
		return opErr(wsv1.ErrorCode_ERROR_CODE_INVALID, "capDrop entry must not be empty")
	}
	return nil
}

// dockerSizeToBytes parses a Docker byte-size string ("512m", "4g", a bare
// integer of bytes) the same way the engine's own --memory/--shm-size
// flags do: b/k/m/g suffixes are binary multiples (1024, not 1000),
// because that is what dockerd itself does, and a Wardstone that used
// decimal multiples would apply a limit measurably different from the one
// the operator read in the plan.
func dockerSizeToBytes(v string) (int64, *wsv1.OpError) {
	if v == "" {
		return 0, nil
	}
	mult := int64(1)
	numPart := v
	switch v[len(v)-1] {
	case 'b', 'B':
		numPart = v[:len(v)-1]
	case 'k', 'K':
		mult = 1 << 10
		numPart = v[:len(v)-1]
	case 'm', 'M':
		mult = 1 << 20
		numPart = v[:len(v)-1]
	case 'g', 'G':
		mult = 1 << 30
		numPart = v[:len(v)-1]
	}
	n, err := strconv.ParseInt(numPart, 10, 64)
	if err != nil || n <= 0 {
		return 0, opErr(wsv1.ErrorCode_ERROR_CODE_INVALID,
			"%q is not a Docker size string: digits with an optional b/k/m/g suffix, e.g. 512m or 4g", v)
	}
	return n * mult, nil
}

// cpusToNanoCPUs parses a decimal core count ("2", "0.5") into Docker's
// NanoCPUs unit (billionths of a core), the same conversion the docker CLI
// itself performs for --cpus.
func cpusToNanoCPUs(v string) (int64, *wsv1.OpError) {
	if v == "" {
		return 0, nil
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil || f <= 0 {
		return 0, opErr(wsv1.ErrorCode_ERROR_CODE_INVALID,
			"%q is not a CPU count: a decimal number greater than zero, e.g. 2 or 0.5", v)
	}
	return int64(f * 1e9), nil
}
