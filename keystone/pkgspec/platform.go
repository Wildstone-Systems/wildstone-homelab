package pkgspec

import (
	"fmt"
	"strings"
)

// Platform is one package-supported host platform. The values deliberately
// match Kubernetes's canonical OS/architecture vocabulary, so a future k3s
// backend can use Architecture() directly as kubernetes.io/arch.
type Platform string

const (
	PlatformLinuxAMD64 Platform = "linux/amd64"
	PlatformLinuxARM64 Platform = "linux/arm64"
)

// supportedPlatforms is the closed initial package-platform vocabulary.
// Keep it private so callers cannot widen admission at runtime.
var supportedPlatforms = map[Platform]bool{
	PlatformLinuxAMD64: true,
	PlatformLinuxARM64: true,
}

// OS and Architecture return the two canonical selector components.
func (p Platform) OS() string {
	os, _, _ := strings.Cut(string(p), "/")
	return os
}

func (p Platform) Architecture() string {
	_, arch, _ := strings.Cut(string(p), "/")
	return arch
}

// SupportsPlatform reports whether an explicitly declared package platform
// matches canonical HostFacts. A package that omits platforms is compatible
// by the staged-adoption rule and never calls this method.
func (p *Package) SupportsPlatform(os, arch string) bool {
	for _, platform := range p.Platforms {
		if platform.OS() == os && platform.Architecture() == arch {
			return true
		}
	}
	return false
}

func validatePlatforms(p *Package) error {
	if p.Platforms != nil && len(p.Platforms) == 0 {
		return &ValidationError{Field: "platforms", Msg: "must not be empty: omit the field to preserve compatibility"}
	}
	seen := map[Platform]bool{}
	for i, platform := range p.Platforms {
		field := fmt.Sprintf("platforms[%d]", i)
		if !supportedPlatforms[platform] {
			return &ValidationError{Field: field, Msg: fmt.Sprintf(
				"%q is not supported (must be one of linux/amd64, linux/arm64)", platform)}
		}
		if seen[platform] {
			return &ValidationError{Field: field, Msg: fmt.Sprintf("%q declared twice", platform)}
		}
		seen[platform] = true
	}
	return nil
}
