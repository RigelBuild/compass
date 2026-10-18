// Package bodydeadline carries a request-scoped body deadline release across
// the HTTP middleware and authentication interceptor.
package bodydeadline

import "context"

type releaseKey struct{}

// WithRelease returns ctx carrying fn as its body deadline release callback.
func WithRelease(ctx context.Context, fn func(context.Context)) context.Context {
	return context.WithValue(ctx, releaseKey{}, fn)
}

// Release clears a body deadline when ctx carries a release callback.
func Release(ctx context.Context) {
	if fn, ok := ctx.Value(releaseKey{}).(func(context.Context)); ok {
		fn(ctx)
	}
}
