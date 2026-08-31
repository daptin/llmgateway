package adapter

import (
	"context"
	"errors"
	"io"

	"github.com/daptin/llmgateway/catalog"
	"github.com/daptin/llmgateway/contract"
)

type Capabilities struct {
	Operations map[contract.Operation]bool
	Features   map[string]bool
}

func (c Capabilities) Supports(operation contract.Operation) bool {
	return c.Operations[operation]
}

type Secret struct {
	value []byte
}

func NewSecret(value []byte) Secret {
	return Secret{value: append([]byte(nil), value...)}
}

func (s Secret) Bytes() []byte {
	return append([]byte(nil), s.value...)
}

// String deliberately provides no secret representation.
func (Secret) String() string { return "[REDACTED]" }

type Stream interface {
	Next(context.Context) (contract.StreamEvent, error)
	Close() error
}

// EndOfStream is returned by Stream.Next after its terminal event. Adapters may
// return io.EOF directly; this alias exists to keep host tests expressive.
var EndOfStream = io.EOF

type Adapter interface {
	Capabilities() Capabilities
	Invoke(context.Context, catalog.Deployment, contract.Request) (contract.Response, error)
	Stream(context.Context, catalog.Deployment, contract.Request) (Stream, error)
}

// HealthChecker is implemented only by adapters with a side-effect-free
// provider health operation. Deployments cannot enable probes otherwise.
type HealthChecker interface {
	HealthCheck(context.Context, catalog.Deployment) error
}

// DeploymentValidator lets an adapter reject adapter-specific deployment
// configuration while a complete snapshot is being built. The same adapter
// remains responsible for defensive validation when invoked directly.
type DeploymentValidator interface {
	ValidateDeployment(catalog.Deployment) error
}

type Factory interface {
	Build(context.Context, catalog.Provider, Secret) (Adapter, error)
}

type FactoryFunc func(context.Context, catalog.Provider, Secret) (Adapter, error)

func (f FactoryFunc) Build(ctx context.Context, provider catalog.Provider, secret Secret) (Adapter, error) {
	return f(ctx, provider, secret)
}

var ErrUnsupportedOperation = errors.New("adapter does not support the requested operation")
