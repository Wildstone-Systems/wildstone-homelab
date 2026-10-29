package agent

import "runtime"

// hostArchitecture is deliberately narrower than runtime.GOARCH. HostFacts
// is a public typed contract, not a bag of host strings, and package platform
// admission initially understands exactly the two supported Linux targets.
func hostArchitecture() string {
	switch runtime.GOARCH {
	case "amd64", "arm64":
		return runtime.GOARCH
	default:
		return ""
	}
}
