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
