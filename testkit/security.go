package testkit

import (
	"context"

	"github.com/daptin/llmgateway/catalog"
	"github.com/daptin/llmgateway/contract"
)

type AllowAuthorizer struct{}

func (AllowAuthorizer) Authorize(context.Context, contract.Principal, catalog.Model) error {
	return nil
}

type SecretResolver map[string][]byte

func (s SecretResolver) ResolveSecret(ctx context.Context, reference string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return append([]byte(nil), s[reference]...), nil
}
