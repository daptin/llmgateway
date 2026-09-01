package llmgateway_test

import (
	"context"
	"io"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/daptin/llmgateway"
	"github.com/daptin/llmgateway/adapter"
	"github.com/daptin/llmgateway/catalog"
	"github.com/daptin/llmgateway/contract"
	"github.com/daptin/llmgateway/guardrail"
	"github.com/daptin/llmgateway/testkit"
)

type benchmarkAdapter struct{}

func (benchmarkAdapter) Capabilities() adapter.Capabilities {
	return adapter.Capabilities{Operations: map[contract.Operation]bool{contract.OperationChat: true}}
}

func (benchmarkAdapter) Invoke(context.Context, catalog.Deployment, contract.Request) (contract.Response, error) {
	return contract.Response{
		Chat:  &contract.ChatResponse{ID: "response"},
		Usage: contract.Usage{InputTokens: 2, OutputTokens: 3, TotalTokens: 5},
	}, nil
}

func (benchmarkAdapter) Stream(context.Context, catalog.Deployment, contract.Request) (adapter.Stream, error) {
	return nil, io.EOF
}

type benchmarkMetering struct{}

func (benchmarkMetering) Admit(_ context.Context, admission contract.Admission) (contract.ReservationToken, error) {
	return contract.ReservationToken{RequestID: admission.RequestID, Opaque: "held"}, nil
}

func (benchmarkMetering) Complete(context.Context, contract.Completion) error { return nil }
func (benchmarkMetering) Cancel(context.Context, contract.Cancellation) error { return nil }

type benchmarkCounters struct{}

func (benchmarkCounters) Add(context.Context, string, int64, time.Duration) (int64, error) {
	return 0, nil
}

func (benchmarkCounters) Get(context.Context, string) (int64, bool, error) { return 0, false, nil }
func (benchmarkCounters) Acquire(context.Context, string, int64, time.Duration) (string, error) {
	return "lease", nil
}
func (benchmarkCounters) Release(context.Context, string) error { return nil }
func (benchmarkCounters) Delete(context.Context, string) error  { return nil }

type benchmarkSelector struct{}

func (benchmarkSelector) Intn(int) int { return 0 }

func BenchmarkEngineInvoke(b *testing.B) {
	registry := adapter.NewRegistry()
	provider := benchmarkAdapter{}
	if err := registry.Register("test", adapter.FactoryFunc(func(context.Context, catalog.Provider, adapter.Secret) (adapter.Adapter, error) {
		return provider, nil
	})); err != nil {
		b.Fatal(err)
	}
	engine, err := llmgateway.New(llmgateway.Dependencies{
		Catalog: testkit.NewCatalogSource(testDocument()), Adapters: registry,
		Authorizer: testkit.AllowAuthorizer{}, Metering: benchmarkMetering{}, Counters: benchmarkCounters{},
		Cache: llmgateway.DisabledResponseCache{}, Guardrails: guardrail.NewRegistry(),
		Telemetry: llmgateway.DiscardTelemetrySink{}, Selector: benchmarkSelector{}, Clock: llmgateway.SystemClock{},
	}, llmgateway.Options{})
	if err != nil {
		b.Fatal(err)
	}
	if err := engine.Reload(context.Background()); err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() {
		if err := engine.Drain(context.Background()); err != nil {
			b.Error(err)
		}
	})

	principal := contract.Principal{KeyID: "benchmark-key"}
	var sequence atomic.Uint64
	run := func() error {
		request := chatRequest("", false)
		request.EstimatedUsage = contract.Usage{InputTokens: 2, OutputTokens: 8, TotalTokens: 10}
		request.ID = contract.ID(strconv.FormatUint(sequence.Add(1), 10))
		_, err := engine.Invoke(context.Background(), principal, request)
		return err
	}

	b.Run("serial", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			if err := run(); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("parallel", func(b *testing.B) {
		b.ReportAllocs()
		var failures atomic.Uint64
		b.RunParallel(func(pb *testing.PB) {
			for pb.Next() {
				if run() != nil {
					failures.Add(1)
				}
			}
		})
		if count := failures.Load(); count != 0 {
			b.Fatalf("engine invocation failures: %d", count)
		}
	})
}
