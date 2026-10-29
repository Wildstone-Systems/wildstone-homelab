package agent

import "testing"

func TestHostArchitectureIsBounded(t *testing.T) {
	got := hostArchitecture()
	if got != "" && got != "amd64" && got != "arm64" {
		t.Fatalf("hostArchitecture = %q, want empty, amd64, or arm64", got)
	}
}
