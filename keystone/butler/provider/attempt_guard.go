package provider

import (
	"context"
	"errors"
	"net/http"
)

type attemptCheckKey struct{}

// WithAttemptCheck binds a live runtime policy check to every HTTP attempt,
// including retries and redirects inside one provider Chat call.
func WithAttemptCheck(ctx context.Context, check func() error) context.Context {
	prior, _ := ctx.Value(attemptCheckKey{}).(func() error)
	return context.WithValue(ctx, attemptCheckKey{}, func() error {
		if prior != nil {
			if err := prior(); err != nil {
				return err
			}
		}
		return check()
	})
}

type checkedTransport struct{ base http.RoundTripper }

type attemptCheckError struct{ error }

func (e attemptCheckError) Unwrap() error { return e.error }

func unwrapAttemptCheckError(err error) (error, bool) {
	var checked attemptCheckError
	if errors.As(err, &checked) {
		return checked.error, true
	}
	return nil, false
}

func (t checkedTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if check, _ := req.Context().Value(attemptCheckKey{}).(func() error); check != nil {
		if err := check(); err != nil {
			return nil, attemptCheckError{err}
		}
	}
	return t.base.RoundTrip(req)
}
func guardedHTTPClient(client *http.Client) *http.Client {
	copy := *client
	base := copy.Transport
	if base == nil {
		base = http.DefaultTransport
	}
	copy.Transport = checkedTransport{base: base}
	return &copy
}
