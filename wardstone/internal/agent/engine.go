package agent

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"os"
	"regexp"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"

	"github.com/Wildstone-Systems/wildstone-homelab/keystone/kubeapi"
	wsv1 "github.com/Wildstone-Systems/wildstone-homelab/proto/gen/wardstone/v1"
	"github.com/Wildstone-Systems/wildstone-homelab/wardstone/internal/leader"
)

var engineNodeName = regexp.MustCompile(`^[a-z0-9]([-a-z0-9.]{0,51}[a-z0-9])?$`)

// runLeadership uses only the scoped, rotating Wardstone token. The
// administrator kubeconfig is never opened by this process. A successful
// epoch RPC is followed by dry-run admission on every configured server;
// until all pass, this node serves no leader work.
func (a *Agent) runLeadership(ctx context.Context) error {
	clients := make([]leader.Prober, 0, len(a.cfg.EngineAPIs))
	var electorClient *kubeapi.Client
	for _, addr := range a.cfg.EngineAPIs {
		client, err := kubeapi.New(addr, a.cfg.EngineCAFile, a.cfg.EngineTokenFile)
		if err != nil {
			return fmt.Errorf("engine API %q: %w", addr, err)
		}
		if electorClient == nil {
			electorClient = client
		}
		clients = append(clients, client)
	}
	identity := "system:serviceaccount:wildstone-system:wardstone-" + a.cfg.NodeID
	l := leader.Leader{
		Elector: &leader.Elector{Client: electorClient, Identity: identity},
		Issuer:  keystoneEpochIssuer{agent: a},
		Barrier: leader.Barrier{Servers: clients},
		Serve: func(termCtx context.Context, term leader.Term) error {
			a.activeLeaderEpoch.Store(term.Epoch)
			defer a.activeLeaderEpoch.Store(0)
			a.log.Info("cluster leadership term active", "epoch", term.Epoch)
			<-termCtx.Done()
			a.log.Info("cluster leadership term ended", "epoch", term.Epoch)
			return termCtx.Err()
		},
		Incident: func(err error) { a.log.Error("cluster leadership incident", "error", err) },
	}
	return l.Run(ctx)
}

type keystoneEpochIssuer struct{ agent *Agent }

func (i keystoneEpochIssuer) Issue(ctx context.Context, leaderID string) (uint64, error) {
	// The RPC has no leader field. Checking the local value catches a
	// wiring mistake before the request; Keystone independently derives
	// its identity from this node's verified client certificate.
	if leaderID != "system:serviceaccount:wildstone-system:wardstone-"+i.agent.cfg.NodeID {
		return 0, fmt.Errorf("epoch request for another node was refused")
	}
	cert, err := tls.LoadX509KeyPair(i.agent.certPath(), i.agent.keyPath())
	if err != nil {
		return 0, fmt.Errorf("loading Wardstone identity: %w", err)
	}
	caPEM, err := os.ReadFile(i.agent.caPath())
	if err != nil {
		return 0, fmt.Errorf("loading Keystone CA: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return 0, fmt.Errorf("pinned Keystone CA has no PEM certificate")
	}
	conn, err := grpc.NewClient(i.agent.cfg.KeystoneAddr, grpc.WithTransportCredentials(credentials.NewTLS(&tls.Config{
		Certificates: []tls.Certificate{cert}, RootCAs: pool, MinVersion: tls.VersionTLS13,
	})))
	if err != nil {
		return 0, err
	}
	defer conn.Close()
	resp, err := wsv1.NewKeystoneGatewayClient(conn).IssueLeaderEpoch(ctx, &wsv1.IssueLeaderEpochRequest{})
	if err != nil {
		return 0, err
	}
	if resp.GetEpoch() == 0 {
		return 0, fmt.Errorf("Keystone returned epoch zero")
	}
	return resp.GetEpoch(), nil
}
