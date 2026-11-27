package contract

import "context"

type boundedExposureKey struct{}

// WithBoundedTokenExposure requests conservative text reservations from a trusted
// host budget. This is backend context, not a public inference parameter.
func WithBoundedTokenExposure(ctx context.Context) context.Context {
	return context.WithValue(ctx, boundedExposureKey{}, true)
}

func BoundedTokenExposure(ctx context.Context) bool {
	value, _ := ctx.Value(boundedExposureKey{}).(bool)
	return value
}
