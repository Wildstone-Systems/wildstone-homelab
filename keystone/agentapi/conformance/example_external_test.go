//go:build conformance_external

package conformance_test

// TestExternalRuntimeIsConformant is what a runtime author, or an
// external CI job, actually runs against a live Keystone (or a
// Wildstone-compatible stand-in) from outside this repository's own
// `go test ./...` (doc.go's route 2):
//
//	go test -tags conformance_external ./keystone/agentapi/conformance/... \
//	  -target-url https://keystone.internal:8443
//
// with the runtime bearer in KEYSTONE_RUNTIME_BEARER and a live task
// handle for that Agent in KEYSTONE_CONFORMANCE_HANDLE — both read once
// from the environment and never logged, printed, or passed as a flag,
// so neither can end up in a shell history or a process listing
// (ADR-W040 §7: "no secret in URLs; redaction in logs").
//
// The build tag keeps this file out of every ordinary build, vet and
// test run in this repository: it dials a network address the ordinary
// CI build never should, and with no -target-url given it only skips,
// so an accidental `-tags conformance_external` still does nothing
// destructive.

import (
	"flag"
	"os"
	"testing"

	"github.com/Wildstone-Systems/wildstone-homelab/keystone/agentapi"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/agentapi/conformance"
	"github.com/Wildstone-Systems/wildstone-homelab/keystone/agentapi/httpclient"
)

var targetURL = flag.String("target-url", "", "base URL of a live Keystone (or Wildstone-compatible) deployment to test against")

func TestExternalRuntimeIsConformant(t *testing.T) {
	if *targetURL == "" {
		t.Skip("no -target-url given; this test only runs against a live external deployment")
	}
	bearer := os.Getenv("KEYSTONE_RUNTIME_BEARER")
	if bearer == "" {
		t.Fatal("KEYSTONE_RUNTIME_BEARER must name a runtime principal's bearer")
	}
	handle := agentapi.Handle(os.Getenv("KEYSTONE_CONFORMANCE_HANDLE"))
	if handle == "" {
		t.Skip("set KEYSTONE_CONFORMANCE_HANDLE to a live task handle for this Agent to run the suite externally")
	}
	client := &httpclient.Client{BaseURL: *targetURL, Bearer: bearer}
	conformance.Run(t, conformance.Target{
		Client:       client,
		Deliveries:   client,
		Handle:       func(t *testing.T) agentapi.Handle { return handle },
		ReadOnlyTool: os.Getenv("KEYSTONE_CONFORMANCE_READONLY_TOOL"),
		ProposeTool:  os.Getenv("KEYSTONE_CONFORMANCE_PROPOSE_TOOL"),
		// NeverApplied and SeedDelivery need this deployment's own probe
		// of what its tools actually change and how its deliveries are
		// seeded, which no generic flag or env var can supply — a runtime
		// author who wants those two checks copies this file and fills
		// them in, the same way keystone/api/handlers_agent_test.go's own
		// TestInProcessGatewayIsConformant and TestHTTPClientIsConformant
		// do for Wildstone's own fixture.
	})
}
