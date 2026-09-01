package llmgateway_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	llmgateway "github.com/daptin/llmgateway"
	"github.com/daptin/llmgateway/adapter"
	"github.com/daptin/llmgateway/catalog"
	"github.com/daptin/llmgateway/contract"
	"github.com/daptin/llmgateway/guardrail"
	"github.com/daptin/llmgateway/testkit"
)

type healthAdapter struct {
	*testkit.FaultAdapter
	healthErr   error
	healthPanic bool
	healthStart chan<- struct{}
	healthWait  <-chan struct{}
	healthCalls atomic.Int64
	closes      atomic.Int64
}

func (a *healthAdapter) HealthCheck(ctx context.Context, _ catalog.Deployment) error {
	a.healthCalls.Add(1)
	if a.healthStart != nil {
		a.healthStart <- struct{}{}
	}
	if a.healthWait != nil {
		select {
		case <-a.healthWait:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if a.healthPanic {
		panic("health payload must not escape")
	}
	return a.healthErr
}

func (a *healthAdapter) CloseIdleConnections() { a.closes.Add(1) }

func healthEngine(t *testing.T, provider adapter.Adapter, counters llmgateway.CounterStore, options llmgateway.Options, checks ...catalog.HealthCheck) *llmgateway.Engine {
	t.Helper()
	document := testDocument()
	check := catalog.HealthCheck{Enabled: true}
	if len(checks) == 1 {
		check = checks[0]
	}
	document.Deployments[0].HealthCheck = check
	registry := adapter.NewRegistry()
	if err := registry.Register("test", adapter.FactoryFunc(func(context.Context, catalog.Provider, adapter.Secret) (adapter.Adapter, error) {
		return provider, nil
	})); err != nil {
		t.Fatal(err)
	}
	engine, err := llmgateway.New(llmgateway.Dependencies{Catalog: testkit.NewCatalogSource(document), Adapters: registry,
		Authorizer: testkit.AllowAuthorizer{}, Metering: testkit.NewMeteringRecorder(), Counters: counters,
		Cache: llmgateway.DisabledResponseCache{}, Guardrails: guardrail.NewRegistry(), Telemetry: llmgateway.DiscardTelemetrySink{},
		Selector: testkit.NewSelector(0), Clock: llmgateway.SystemClock{}}, options)
	if err != nil {
		t.Fatal(err)
	}
	return engine
}

func TestReloadRejectsHealthCheckWithoutAdapterSupport(t *testing.T) {
	provider := testkit.NewFaultAdapter(adapter.Capabilities{Operations: map[contract.Operation]bool{contract.OperationChat: true}})
	engine := healthEngine(t, provider, testkit.NewCounterStore(time.Now), llmgateway.Options{})
	if err := engine.Reload(context.Background()); err == nil {
		t.Fatal("accepted health checks on an adapter without probe support")
	}
}

func TestSuccessfulProbeDoesNotEraseRateLimitCooldown(t *testing.T) {
	fault := testkit.NewFaultAdapter(adapter.Capabilities{Operations: map[contract.Operation]bool{contract.OperationChat: true}},
		testkit.AdapterStep{Response: contract.Response{Usage: contract.Usage{TotalTokens: 1}}})
	provider := &healthAdapter{FaultAdapter: fault}
	counters := testkit.NewCounterStore(time.Now)
	engine := healthEngine(t, provider, counters, llmgateway.Options{})
	if err := engine.Reload(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := counters.Add(context.Background(), "llmgateway:deployment:d:rate_cooldown", 1, time.Minute); err != nil {
		t.Fatal(err)
	}
	report, err := engine.Probe(context.Background())
	if err != nil || report.Healthy != 1 {
		t.Fatalf("report=%+v err=%v", report, err)
	}
	_, err = engine.Invoke(context.Background(), contract.Principal{}, chatRequest("rate-cooldown", false))
	var gatewayError *contract.Error
	if !errors.As(err, &gatewayError) || gatewayError.Code != contract.ErrorRateLimit {
		t.Fatalf("rate cooldown was erased: %v", err)
	}
	if len(fault.Attempts()) != 0 {
		t.Fatalf("provider called during rate cooldown: %v", fault.Attempts())
	}
}

func TestFailedProbeOpensInfrastructureCircuit(t *testing.T) {
	fault := testkit.NewFaultAdapter(adapter.Capabilities{Operations: map[contract.Operation]bool{contract.OperationChat: true}})
	provider := &healthAdapter{FaultAdapter: fault, healthErr: errors.New("unreachable")}
	engine := healthEngine(t, provider, testkit.NewCounterStore(time.Now), llmgateway.Options{}, catalog.HealthCheck{Enabled: true, FailureThreshold: 1})
	if err := engine.Reload(context.Background()); err != nil {
		t.Fatal(err)
	}
	report, err := engine.Probe(context.Background())
	if err != nil || report.Unhealthy != 1 {
		t.Fatalf("report=%+v err=%v", report, err)
	}
	_, err = engine.Invoke(context.Background(), contract.Principal{}, chatRequest("open-circuit", false))
	var gatewayError *contract.Error
	if !errors.As(err, &gatewayError) || gatewayError.Code != contract.ErrorUnavailable {
		t.Fatalf("failed probe did not open circuit: %v", err)
	}
}

func TestPanickingProbeIsContainedAndOpensInfrastructureCircuit(t *testing.T) {
	fault := testkit.NewFaultAdapter(adapter.Capabilities{Operations: map[contract.Operation]bool{contract.OperationChat: true}})
	provider := &healthAdapter{FaultAdapter: fault, healthPanic: true}
	engine := healthEngine(t, provider, testkit.NewCounterStore(time.Now), llmgateway.Options{}, catalog.HealthCheck{Enabled: true, FailureThreshold: 1})
	if err := engine.Reload(context.Background()); err != nil {
		t.Fatal(err)
	}
	report, err := engine.Probe(context.Background())
	if err != nil || report.Unhealthy != 1 || provider.healthCalls.Load() != 1 {
		t.Fatalf("report=%+v calls=%d err=%v", report, provider.healthCalls.Load(), err)
	}
	_, err = engine.Invoke(context.Background(), contract.Principal{}, chatRequest("panic-open-circuit", false))
	var gatewayError *contract.Error
	if !errors.As(err, &gatewayError) || gatewayError.Code != contract.ErrorUnavailable {
		t.Fatalf("panicking probe did not open circuit: %v", err)
	}
}

func TestDrainWaitsForHealthProbeBeforeClosingSnapshot(t *testing.T) {
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	provider := &healthAdapter{
		FaultAdapter: testkit.NewFaultAdapter(adapter.Capabilities{Operations: map[contract.Operation]bool{contract.OperationChat: true}}),
		healthStart:  started, healthWait: release,
	}
	engine := healthEngine(t, provider, testkit.NewCounterStore(time.Now), llmgateway.Options{})
	if err := engine.Reload(context.Background()); err != nil {
		t.Fatal(err)
	}
	probeResult := make(chan error, 1)
	go func() {
		_, err := engine.Probe(context.Background())
		probeResult <- err
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("health probe did not start")
	}
	drainContext, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	if err := engine.Drain(drainContext); !errors.Is(err, context.DeadlineExceeded) {
		cancel()
		t.Fatalf("drain during probe = %v", err)
	}
	cancel()
	if provider.closes.Load() != 0 {
		t.Fatal("active health adapter was closed")
	}
	close(release)
	select {
	case err := <-probeResult:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("health probe did not finish")
	}
	if err := engine.Drain(context.Background()); err != nil {
		t.Fatal(err)
	}
	if provider.closes.Load() != 1 {
		t.Fatalf("health adapter close count = %d, want 1", provider.closes.Load())
	}
}

func TestProbeHonorsPerDeploymentInterval(t *testing.T) {
	document := testDocument()
	document.Deployments[0].HealthCheck = catalog.HealthCheck{Enabled: true, Interval: time.Minute}
	provider := &healthAdapter{FaultAdapter: testkit.NewFaultAdapter(adapter.Capabilities{Operations: map[contract.Operation]bool{contract.OperationChat: true}})}
	registry := adapter.NewRegistry()
	if err := registry.Register("test", adapter.FactoryFunc(func(context.Context, catalog.Provider, adapter.Secret) (adapter.Adapter, error) { return provider, nil })); err != nil {
		t.Fatal(err)
	}
	clock := testkit.NewClock(time.Date(2026, 8, 31, 0, 0, 0, 0, time.UTC))
	engine, err := llmgateway.New(llmgateway.Dependencies{
		Catalog: testkit.NewCatalogSource(document), Adapters: registry, Authorizer: testkit.AllowAuthorizer{},
		Metering: testkit.NewMeteringRecorder(), Counters: testkit.NewCounterStore(clock.Now), Cache: llmgateway.DisabledResponseCache{},
		Guardrails: guardrail.NewRegistry(), Telemetry: llmgateway.DiscardTelemetrySink{}, Selector: testkit.NewSelector(0), Clock: clock,
	}, llmgateway.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.Reload(context.Background()); err != nil {
		t.Fatal(err)
	}
	first, err := engine.Probe(context.Background())
	if err != nil || first.Checked != 1 {
		t.Fatalf("first probe=%+v err=%v", first, err)
	}
	second, err := engine.Probe(context.Background())
	if err != nil || second.Checked != 0 {
		t.Fatalf("early probe=%+v err=%v", second, err)
	}
	clock.Advance(time.Minute)
	third, err := engine.Probe(context.Background())
	if err != nil || third.Checked != 1 || provider.healthCalls.Load() != 2 {
		t.Fatalf("due probe=%+v calls=%d err=%v", third, provider.healthCalls.Load(), err)
	}
}
