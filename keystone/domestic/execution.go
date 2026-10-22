package domestic

import (
	"context"
	"fmt"
	"net/http"
)

// HouseholdExecutionAuthority is supplied by the runtime store, which checks
// this process's lease and the household dispatch stop on every call.
type HouseholdExecutionAuthority interface{ CheckHouseholdExecution() error }

func checkExecution(ctx context.Context, authority HouseholdExecutionAuthority) error {
	if authority == nil {
		return fmt.Errorf("household execution refused: no coordinator authority is wired")
	}
	if err := authority.CheckHouseholdExecution(); err != nil {
		return err
	}
	return CheckExecutionContext(ctx)
}

// WithExecutionCheck carries trusted caller authorization through preparation
// to the gateway and executor boundaries. Model-authored Call fields cannot set it.
func WithExecutionCheck(ctx context.Context, check func() error) context.Context {
	previous, _ := ctx.Value(executionCheckKey{}).(func() error)
	return context.WithValue(ctx, executionCheckKey{}, func() error {
		if previous != nil {
			if err := previous(); err != nil {
				return err
			}
		}
		return check()
	})
}

type executionCheckKey struct{}

func CheckExecutionContext(ctx context.Context) error {
	if ctx == nil {
		return nil
	}
	if check, ok := ctx.Value(executionCheckKey{}).(func() error); ok {
		return check()
	}
	return nil
}

// Redirects and transport retries are new attempts to disclose or mutate.
func executionHTTPClient(client *http.Client) *http.Client {
	if client == nil {
		client = http.DefaultClient
	}
	local := *client
	base := local.Transport
	if base == nil {
		base = http.DefaultTransport
	}
	local.Transport = executionTransport{base}
	return &local
}

type executionTransport struct{ base http.RoundTripper }

func (t executionTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if err := CheckExecutionContext(r.Context()); err != nil {
		return nil, err
	}
	return t.base.RoundTrip(r)
}
