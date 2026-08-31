package llmgateway_test

import (
	"context"
	"errors"
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

func protectedEngine(t *testing.T, document catalog.Document, provider adapter.Adapter, metering *testkit.MeteringRecorder, counters llmgateway.CounterStore, clock llmgateway.Clock, options llmgateway.Options) *llmgateway.Engine {
	t.Helper()
	registry := adapter.NewRegistry()
	if err := registry.Register("test", adapter.FactoryFunc(func(context.Context, catalog.Provider, adapter.Secret) (adapter.Adapter, error) { return provider, nil })); err != nil {
		t.Fatal(err)
	}
	engine, err := llmgateway.New(llmgateway.Dependencies{
		Catalog: testkit.NewCatalogSource(document), Adapters: registry, Authorizer: testkit.AllowAuthorizer{},
		Metering: metering, Counters: counters, Cache: llmgateway.DisabledResponseCache{}, Guardrails: guardrail.NewRegistry(), Telemetry: llmgateway.DiscardTelemetrySink{}, Selector: testkit.NewSelector(0), Clock: clock,
	}, options)
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.Reload(context.Background()); err != nil {
		t.Fatal(err)
	}
	return engine
}

func TestCircuitStateIsSharedAcrossEnginesAndHalfOpenRecovers(t *testing.T) {
	clock := testkit.NewClock(time.Date(2026, 8, 31, 0, 0, 0, 0, time.UTC))
	counters := testkit.NewCounterStore(clock.Now)
	failing := testkit.NewFaultAdapter(
		adapter.Capabilities{Operations: map[contract.Operation]bool{contract.OperationChat: true}},
		testkit.AdapterStep{InvokeError: &contract.Error{Code: contract.ErrorProvider, Message: "failed", HTTPStatus: 502, Retryable: true}},
	)
	success := testkit.NewFaultAdapter(
		adapter.Capabilities{Operations: map[contract.Operation]bool{contract.OperationChat: true}},
		testkit.AdapterStep{Response: contract.Response{Chat: &contract.ChatResponse{ID: "ok"}, Usage: contract.Usage{TotalTokens: 1}}},
	)
	options := llmgateway.Options{CircuitFailures: 1, CircuitCooldown: time.Second, CircuitWindow: 10 * time.Second}
	first := protectedEngine(t, testDocument(), failing, testkit.NewMeteringRecorder(), counters, clock, options)
	second := protectedEngine(t, testDocument(), success, testkit.NewMeteringRecorder(), counters, clock, options)
	if _, err := first.Invoke(context.Background(), contract.Principal{}, chatRequest("first", false)); err == nil {
		t.Fatal("expected first provider failure")
	}
	_, err := second.Invoke(context.Background(), contract.Principal{}, chatRequest("blocked", false))
	var typed *contract.Error
	if !errors.As(err, &typed) || typed.Code != contract.ErrorUnavailable {
		t.Fatalf("expected shared cooldown, got %v", err)
	}
	if len(success.Attempts()) != 0 {
		t.Fatalf("provider was called while circuit was open: %v", success.Attempts())
	}
	clock.Advance(time.Second)
	if _, err := second.Invoke(context.Background(), contract.Principal{}, chatRequest("probe", false)); err != nil {
		t.Fatalf("half-open probe failed: %v", err)
	}
	if len(success.Attempts()) != 1 {
		t.Fatalf("expected one half-open provider probe, got %v", success.Attempts())
	}
}

type blockingAdapter struct {
	entered chan struct{}
	release chan struct{}
	calls   atomic.Int64
}

func (b *blockingAdapter) Capabilities() adapter.Capabilities {
	return adapter.Capabilities{Operations: map[contract.Operation]bool{contract.OperationChat: true}}
}

func (b *blockingAdapter) Invoke(ctx context.Context, _ catalog.Deployment, _ contract.Request) (contract.Response, error) {
	b.calls.Add(1)
	select {
	case b.entered <- struct{}{}:
	case <-ctx.Done():
		return contract.Response{}, ctx.Err()
	}
	select {
	case <-b.release:
		return contract.Response{Chat: &contract.ChatResponse{ID: "ok"}, Usage: contract.Usage{TotalTokens: 1}}, nil
	case <-ctx.Done():
		return contract.Response{}, ctx.Err()
	}
}

func (*blockingAdapter) Stream(context.Context, catalog.Deployment, contract.Request) (adapter.Stream, error) {
	return nil, adapter.ErrUnsupportedOperation
}

func TestConcurrencyAdmissionPreventsSecondProviderCall(t *testing.T) {
	document := testDocument()
	document.Deployments[0].MaxConcurrency = 1
	clock := testkit.NewAutoClock(time.Now())
	provider := &blockingAdapter{entered: make(chan struct{}, 1), release: make(chan struct{})}
	engine := protectedEngine(t, document, provider, testkit.NewMeteringRecorder(), testkit.NewCounterStore(clock.Now), clock, llmgateway.Options{})
	firstDone := make(chan error, 1)
	go func() {
		_, err := engine.Invoke(context.Background(), contract.Principal{}, chatRequest("first", false))
		firstDone <- err
	}()
	select {
	case <-provider.entered:
	case <-time.After(time.Second):
		t.Fatal("first provider call did not start")
	}
	_, err := engine.Invoke(context.Background(), contract.Principal{}, chatRequest("second", false))
	var typed *contract.Error
	if !errors.As(err, &typed) || typed.Code != contract.ErrorRateLimit {
		t.Fatalf("expected concurrency rejection, got %v", err)
	}
	if provider.calls.Load() != 1 {
		t.Fatalf("second request reached provider: calls=%d", provider.calls.Load())
	}
	close(provider.release)
	if err := <-firstDone; err != nil {
		t.Fatal(err)
	}
}

func TestCounterFailureFailsClosedBeforeProvider(t *testing.T) {
	clock := testkit.NewAutoClock(time.Now())
	counters := testkit.NewCounterStore(clock.Now)
	counters.SetFailure(errors.New("coordination unavailable"))
	provider := testkit.NewFaultAdapter(
		adapter.Capabilities{Operations: map[contract.Operation]bool{contract.OperationChat: true}},
		testkit.AdapterStep{Response: contract.Response{Chat: &contract.ChatResponse{ID: "must-not-run"}, Usage: contract.Usage{TotalTokens: 1}}},
	)
	engine := protectedEngine(t, testDocument(), provider, testkit.NewMeteringRecorder(), counters, clock, llmgateway.Options{})
	_, err := engine.Invoke(context.Background(), contract.Principal{}, chatRequest("request", false))
	var typed *contract.Error
	if !errors.As(err, &typed) || typed.Code != contract.ErrorUnavailable {
		t.Fatalf("expected fail-closed unavailable error, got %v", err)
	}
	if len(provider.Attempts()) != 0 {
		t.Fatalf("provider was called without protection: %v", provider.Attempts())
	}
}

func TestGateRejectionDoesNotCreateOrRenumberUpstreamAttempts(t *testing.T) {
	document := testDocument()
	document.Deployments = append(document.Deployments, catalog.Deployment{
		ID: "d2", Name: "second", ProviderID: "p", ModelID: "m", UpstreamModel: "other",
		Operations: []contract.Operation{contract.OperationChat}, Weight: 1, MaxConcurrency: -1, RPM: -1, TPM: -1, Enabled: true,
	})
	clock := testkit.NewAutoClock(time.Now())
	counters := testkit.NewCounterStore(clock.Now)
	if _, err := counters.Add(context.Background(), "llmgateway:deployment:d:rate_cooldown", 1, time.Minute); err != nil {
		t.Fatal(err)
	}
	provider := testkit.NewFaultAdapter(
		adapter.Capabilities{Operations: map[contract.Operation]bool{contract.OperationChat: true}},
		testkit.AdapterStep{Response: contract.Response{Chat: &contract.ChatResponse{ID: "ok"}, Usage: contract.Usage{TotalTokens: 1}}},
	)
	store := testkit.NewMeteringRecorder()
	engine := protectedEngine(t, document, provider, store, counters, clock, llmgateway.Options{})
	if _, err := engine.Invoke(context.Background(), contract.Principal{}, chatRequest("gated-route", false)); err != nil {
		t.Fatal(err)
	}
	completion, ok := store.Completion("gated-route")
	if !ok || len(completion.Attempts) != 1 || completion.Attempts[0].Number != 1 || completion.Attempts[0].DeploymentID != "d2" {
		t.Fatalf("completion=%+v present=%v", completion, ok)
	}
}
