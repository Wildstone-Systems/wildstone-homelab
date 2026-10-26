package gateway

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"

	"github.com/Wildstone-Systems/wildstone-homelab/keystone/clusterauthority"
	wsv1 "github.com/Wildstone-Systems/wildstone-homelab/proto/gen/wardstone/v1"
)

type epochIssuerFunc func(context.Context, string) (uint64, error)

func (f epochIssuerFunc) Issue(ctx context.Context, leader string) (uint64, error) {
	return f(ctx, leader)
}

func epochPeer(node string) context.Context {
	cert := &x509.Certificate{Subject: pkix.Name{CommonName: node}}
	return peer.NewContext(context.Background(), &peer.Peer{AuthInfo: credentials.TLSInfo{
		State: tls.ConnectionState{VerifiedChains: [][]*x509.Certificate{{cert}}},
	}})
}

func TestIssueLeaderEpochUsesOnlyVerifiedPeerIdentity(t *testing.T) {
	var asked string
	g, err := New(Config{EpochIssuer: epochIssuerFunc(func(_ context.Context, leader string) (uint64, error) {
		asked = leader
		return 7, nil
	})})
	if err != nil {
		t.Fatal(err)
	}
	resp, err := g.IssueLeaderEpoch(epochPeer("node-a"), &wsv1.IssueLeaderEpochRequest{})
	if err != nil || resp.GetEpoch() != 7 {
		t.Fatalf("epoch response = %v, %v", resp, err)
	}
	if asked != "system:serviceaccount:wildstone-system:wardstone-node-a" {
		t.Fatalf("issuer was asked for %q", asked)
	}
	if _, err := g.IssueLeaderEpoch(context.Background(), &wsv1.IssueLeaderEpochRequest{}); status.Code(err) != codes.Unauthenticated {
		t.Fatalf("no certificate: %v", err)
	}
	if _, err := g.IssueLeaderEpoch(epochPeer("../../keystone"), &wsv1.IssueLeaderEpochRequest{}); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("invalid certificate identity: %v", err)
	}
}

func TestIssueLeaderEpochFailsClosed(t *testing.T) {
	request := &wsv1.IssueLeaderEpochRequest{}
	g, err := New(Config{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := g.IssueLeaderEpoch(epochPeer("node-a"), request); status.Code(err) != codes.Unavailable {
		t.Fatalf("unconfigured issuer: %v", err)
	}
	g.cfg.EpochIssuer = epochIssuerFunc(func(context.Context, string) (uint64, error) {
		return 0, clusterauthority.ErrNotHolder
	})
	if _, err := g.IssueLeaderEpoch(epochPeer("node-a"), request); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("lost Lease: %v", err)
	}
	g.cfg.EpochIssuer = epochIssuerFunc(func(context.Context, string) (uint64, error) {
		return 0, errors.New("database unavailable")
	})
	if _, err := g.IssueLeaderEpoch(epochPeer("node-a"), request); status.Code(err) != codes.Internal {
		t.Fatalf("issuer error: %v", err)
	}
	g.cfg.EpochIssuer = epochIssuerFunc(func(context.Context, string) (uint64, error) { return 0, nil })
	if _, err := g.IssueLeaderEpoch(epochPeer("node-a"), request); status.Code(err) != codes.Internal {
		t.Fatalf("epoch zero: %v", err)
	}
}
