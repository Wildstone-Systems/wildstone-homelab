package agent

import (
	"context"
	"strings"
	"testing"
)

// localhost is the one name guaranteed to resolve wherever the tests run.
func TestResolveHostAliasesProducesDockerExtraHosts(t *testing.T) {
	got := resolveHostAliases(context.Background(), []string{"localhost"}, nil)
	if len(got) != 1 {
		t.Fatalf("want one entry for localhost, got %v", got)
	}
	if !strings.HasPrefix(got[0], "localhost:") {
		t.Errorf(`want "localhost:<ip>", got %q`, got[0])
	}
}

// A name that does not resolve is SKIPPED, not fatal. Refusing to start
// the container would turn a login that fails into a service that does
// not run — strictly worse, and for a name most of the container's work
// does not touch.
func TestResolveHostAliasesSkipsWhatItCannotResolve(t *testing.T) {
	got := resolveHostAliases(context.Background(),
		[]string{"this-name-does-not-exist.invalid", "localhost"}, nil)
	if len(got) != 1 || !strings.HasPrefix(got[0], "localhost:") {
		t.Fatalf("an unresolvable name must be skipped, not fatal: %v", got)
	}
}

func TestResolveHostAliasesIsDeterministicAndDeduplicates(t *testing.T) {
	a := resolveHostAliases(context.Background(), []string{"localhost", "localhost"}, nil)
	if len(a) != 1 {
		t.Fatalf("a repeated name should appear once, got %v", a)
	}
	// Same input, same output: an /etc/hosts that varies per recreate is
	// a difference nobody can see and everybody has to debug.
	b := resolveHostAliases(context.Background(), []string{"localhost"}, nil)
	if a[0] != b[0] {
		t.Errorf("resolution must be stable: %q vs %q", a[0], b[0])
	}
}

func TestResolveHostAliasesOnEmptyIsNil(t *testing.T) {
	if got := resolveHostAliases(context.Background(), nil, nil); got != nil {
		t.Errorf("want nil, got %v", got)
	}
}

// The host-gateway alias is written as Docker's own "host-gateway" value
// rather than resolved (protocol minor 24): the node has no DNS answer
// for it, and the daemon is the only party that knows its bridge
// gateway. Every other name is still resolved, so this is one closed
// value, not a way to hand Docker arbitrary ExtraHosts.
func TestResolveHostAliasesMapsTheHostGatewayAlias(t *testing.T) {
	got := resolveHostAliases(context.Background(),
		[]string{"localhost", hostGatewayAlias, hostGatewayAlias, "host-gateway.invalid"}, nil)
	if len(got) != 2 || !strings.HasPrefix(got[0], "localhost:") || got[1] != "host.docker.internal:host-gateway" {
		t.Fatalf("want [localhost:<ip> host.docker.internal:host-gateway], got %v", got)
	}
}
