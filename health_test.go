package llmgateway_test

import (
	"context"
	"errors"
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
	healthErr error
}

func (a *healthAdapter) HealthCheck(context.Context, catalog.Deployment) error { return a.healthErr }

func healthEngine(t *testing.T, provider adapter.Adapter, counters llmgateway.CounterStore, options llmgateway.Options) *llmgateway.Engine {
	t.Helper()
	document := testDocument()
	document.Deployments[0].HealthCheck = true
	registry := adapter.NewRegistry()
	if err := registry.Register("test", adapter.FactoryFunc(func(context.Context, catalog.Provider, adapter.Secret) (adapter.Adapter, error) {
		return provider, nil
	})); err != nil {
		t.Fatal(err)
	}
	engine, err := llmgateway.New(llmgateway.Dependencies{Catalog: testkit.NewCatalogSource(document), Adapters: registry,
		Authorizer: testkit.AllowAuthorizer{}, Accounting: testkit.NewAccountingStore(), Counters: counters,
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
	engine := healthEngine(t, provider, testkit.NewCounterStore(time.Now), llmgateway.Options{CircuitFailures: 1})
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
