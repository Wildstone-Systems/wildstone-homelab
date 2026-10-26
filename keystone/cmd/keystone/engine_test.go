package main

import (
	"context"
	"errors"
	"testing"
)

type testEpochIssuer struct{ called bool }

func (i *testEpochIssuer) Issue(_ context.Context, _ string) (uint64, error) {
	i.called = true
	return 3, nil
}

func TestCoordinatorEpochIssuerRefusesStandby(t *testing.T) {
	inner := &testEpochIssuer{}
	active := false
	i := coordinatorEpochIssuer{issuer: inner, check: func() error {
		if !active {
			return errors.New("fenced")
		}
		return nil
	}}
	if _, err := i.Issue(context.Background(), "leader"); err == nil || inner.called {
		t.Fatalf("standby Keystone reached epoch issuer: err=%v called=%v", err, inner.called)
	}
	active = true
	if epoch, err := i.Issue(context.Background(), "leader"); err != nil || epoch != 3 || !inner.called {
		t.Fatalf("active Keystone did not issue epoch: epoch=%d err=%v called=%v", epoch, err, inner.called)
	}
	inner.called = false
	active = false // explicit promotion fences this process after startup
	if _, err := i.Issue(context.Background(), "leader"); err == nil || inner.called {
		t.Fatalf("promoted coordinator still reached issuer: err=%v called=%v", err, inner.called)
	}
}
