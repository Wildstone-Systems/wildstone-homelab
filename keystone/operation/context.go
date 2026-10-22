// Package operation carries trusted runtime operation identity across execution seams.
package operation

import "context"

type Identity struct {
	TaskID          string
	IdempotencyKey  string
	Tool            string
	Step            int64
	PermissionLabel string
	Audience        string
}
type contextKey struct{}

func With(ctx context.Context, id Identity) context.Context {
	return context.WithValue(ctx, contextKey{}, id)
}
func From(ctx context.Context) Identity { id, _ := ctx.Value(contextKey{}).(Identity); return id }
