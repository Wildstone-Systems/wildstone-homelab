package agentgateway

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/chickenrat17/wildstone-homelab/keystone/agentapi"
)

func TestInProcessDeliveryAndAckRequireLiveRuntimeAuthority(t *testing.T) {
	f := newFixture(t)
	f.bindRuntime(t, "rt-alfred", "alfred")
	b, err := f.db.GetButler("alfred")
	if err != nil {
		t.Fatal(err)
	}
	id, err := f.db.RecordAgentDelivery(b.Seq, agentapi.DeliveryTurn, `{}`)
	if err != nil {
		t.Fatal(err)
	}
	ctx := WithPrincipal(context.Background(), "rt-alfred")
	got, err := f.g.Deliveries(ctx, "", 0)
	if err != nil || len(got) != 1 {
		t.Fatalf("authorized Deliveries = %+v, %v", got, err)
	}
	if err := f.db.SetPrincipalDisabled("rt-alfred", true); err != nil {
		t.Fatal(err)
	}
	if _, err := f.g.Deliveries(ctx, "", time.Millisecond); !errors.Is(err, ErrRefused) {
		t.Fatalf("disabled principal Deliveries = %v, want ErrRefused", err)
	}
	if err := f.g.Ack(ctx, got[0].ID); !errors.Is(err, ErrRefused) {
		t.Fatalf("disabled principal Ack = %v, want ErrRefused", err)
	}
	rows, err := f.db.ListAgentDeliveries(b.Seq, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].ID != id || !rows[0].AckedAt.IsZero() {
		t.Fatalf("refused in-process ack changed delivery: %+v", rows)
	}
}
