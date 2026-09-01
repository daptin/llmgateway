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

// MeteringPort is implemented by the host. The gateway reports admission and
// terminal usage facts; the host alone owns policy evaluation and persistence.
type MeteringPort interface {
	Admit(context.Context, contract.Admission) (contract.ReservationToken, error)
	Complete(context.Context, contract.Completion) error
	Cancel(context.Context, contract.Cancellation) error
}

type CounterStore interface {
	// Add atomically increments a disposable protection counter. The first
	// successful increment establishes the TTL; later increments must not slide
	// the window.
	Add(context.Context, string, int64, time.Duration) (int64, error)
	Get(context.Context, string) (int64, bool, error)
	// Acquire atomically reserves one unit up to maximum and returns an opaque,
	// idempotently releasable lease. The aggregate expiry must cover the newest
	// lease so live capacity cannot disappear early; TTL bounds leaked capacity
	// after a crash.
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
