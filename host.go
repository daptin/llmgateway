package llmgateway

import (
	"context"
	"time"

	"github.com/daptin/llmgateway/catalog"
	"github.com/daptin/llmgateway/contract"
)

type CatalogSource interface {
	Load(context.Context, uint64) (catalog.Document, error)
}

type SecretResolver interface {
	ResolveSecret(context.Context, string) ([]byte, error)
}

type Authenticator interface {
	Authenticate(context.Context, string) (contract.Principal, error)
}

type AuthenticatorFunc func(context.Context, string) (contract.Principal, error)

func (f AuthenticatorFunc) Authenticate(ctx context.Context, credential string) (contract.Principal, error) {
	return f(ctx, credential)
}

type Authorizer interface {
	Authorize(context.Context, contract.Principal, catalog.Model) error
}

type AccountingStore interface {
	Admit(context.Context, contract.Admission) (contract.ReservationToken, error)
	Finalize(context.Context, contract.Completion) error
	Cancel(context.Context, contract.Cancellation) error
	ReapExpired(context.Context, time.Time, int) (contract.ReapResult, error)
}

type CounterStore interface {
	Add(context.Context, string, int64, time.Duration) (int64, error)
	Get(context.Context, string) (int64, bool, error)
	Acquire(context.Context, string, int64, time.Duration) (string, error)
	Release(context.Context, string) error
	Delete(context.Context, string) error
}

type ResponseCache interface {
	Get(context.Context, string) ([]byte, bool, error)
	Set(context.Context, string, []byte, time.Duration) error
	Delete(context.Context, string) error
}

// DisabledResponseCache is an explicit cache opt-out.
type DisabledResponseCache struct{}

func (DisabledResponseCache) Get(context.Context, string) ([]byte, bool, error) {
	return nil, false, nil
}
func (DisabledResponseCache) Set(context.Context, string, []byte, time.Duration) error { return nil }
func (DisabledResponseCache) Delete(context.Context, string) error                     { return nil }

type TelemetryEvent struct {
	Name       string
	RequestID  contract.ID
	Revision   uint64
	Attributes map[string]string
	Measures   map[string]int64
}

type TelemetrySink interface {
	Record(context.Context, TelemetryEvent)
}

// DiscardTelemetrySink is an explicit opt-out for hosts that do not emit
// gateway telemetry.
type DiscardTelemetrySink struct{}

func (DiscardTelemetrySink) Record(context.Context, TelemetryEvent) {}

type Clock interface {
	Now() time.Time
	NewTimer(time.Duration) Timer
}

type Timer interface {
	C() <-chan time.Time
	Stop() bool
}

type Selector interface {
	Intn(int) int
}
