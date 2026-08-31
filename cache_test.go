package llmgateway_test

import (
	"context"
	"testing"
	"time"

	"github.com/daptin/llmgateway"
	"github.com/daptin/llmgateway/adapter"
	"github.com/daptin/llmgateway/catalog"
	"github.com/daptin/llmgateway/contract"
	"github.com/daptin/llmgateway/guardrail"
	"github.com/daptin/llmgateway/testkit"
)

func TestExactCacheHitUsesSameAccountingPathAndCannotCrossKeys(t *testing.T) {
	document := testDocument()
	document.Models[0].Capabilities = map[string]bool{"exact_cache": true}
	document.Deployments[0].Pricing.InputMicrosPerMillion = 1_000_000
	provider := testkit.NewFaultAdapter(
		adapter.Capabilities{Operations: map[contract.Operation]bool{contract.OperationChat: true}},
		testkit.AdapterStep{Response: contract.Response{Chat: &contract.ChatResponse{ID: "first"}, Usage: contract.Usage{InputTokens: 2, OutputTokens: 1, TotalTokens: 3}}},
		testkit.AdapterStep{Response: contract.Response{Chat: &contract.ChatResponse{ID: "other-tenant"}, Usage: contract.Usage{InputTokens: 2, OutputTokens: 1, TotalTokens: 3}}},
	)
	registry := adapter.NewRegistry()
	if err := registry.Register("test", adapter.FactoryFunc(func(context.Context, catalog.Provider, adapter.Secret) (adapter.Adapter, error) { return provider, nil })); err != nil {
		t.Fatal(err)
	}
	clock := testkit.NewAutoClock(time.Now())
	accounting := testkit.NewAccountingStore()
	responseCache := testkit.NewResponseCache(clock.Now)
	engine, err := llmgateway.New(llmgateway.Dependencies{
		Catalog: testkit.NewCatalogSource(document), Adapters: registry, Authorizer: testkit.AllowAuthorizer{}, Accounting: accounting,
		Counters: testkit.NewCounterStore(clock.Now), Cache: responseCache, Guardrails: guardrail.NewRegistry(),
		Telemetry: llmgateway.DiscardTelemetrySink{}, Selector: testkit.NewSelector(0), Clock: clock,
	}, llmgateway.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.Reload(context.Background()); err != nil {
		t.Fatal(err)
	}
	zero := 0.0
	request := chatRequest("first-request", false)
	request.Chat.Temperature = &zero
	request.EstimatedUsage = contract.Usage{InputTokens: 2, OutputTokens: 1, TotalTokens: 3, Estimated: true}
	first, err := engine.Invoke(context.Background(), contract.Principal{KeyID: "key-a"}, request)
	if err != nil {
		t.Fatal(err)
	}
	if first.Usage.CostMicros == 0 {
		t.Fatal("provider response should have a calculated cost")
	}
	request.ID = "cached-request"
	cached, err := engine.Invoke(context.Background(), contract.Principal{KeyID: "key-a"}, request)
	if err != nil {
		t.Fatal(err)
	}
	if cached.RequestID != "cached-request" || cached.Chat.ID != "first" || cached.Usage.CostMicros != 0 {
		t.Fatalf("invalid cached response: %#v", cached)
	}
	completion, ok := accounting.Completion("cached-request")
	if !ok || completion.CacheStatus != "hit" || len(completion.Attempts) != 0 || completion.Usage.TotalTokens != 3 {
		t.Fatalf("cache hit bypassed accounting: %#v", completion)
	}
	request.ID = "other-tenant-request"
	other, err := engine.Invoke(context.Background(), contract.Principal{KeyID: "key-b"}, request)
	if err != nil {
		t.Fatal(err)
	}
	if other.Chat.ID != "other-tenant" || len(provider.Attempts()) != 2 {
		t.Fatalf("cache crossed API keys: response=%#v attempts=%v", other, provider.Attempts())
	}
}

func TestCacheFailureDegradesToProviderWithoutFailingRequest(t *testing.T) {
	document := testDocument()
	document.Models[0].Capabilities = map[string]bool{"exact_cache": true}
	provider := testkit.NewFaultAdapter(
		adapter.Capabilities{Operations: map[contract.Operation]bool{contract.OperationChat: true}},
		testkit.AdapterStep{Response: contract.Response{Chat: &contract.ChatResponse{ID: "provider"}, Usage: contract.Usage{TotalTokens: 1}}},
	)
	registry := adapter.NewRegistry()
	if err := registry.Register("test", adapter.FactoryFunc(func(context.Context, catalog.Provider, adapter.Secret) (adapter.Adapter, error) { return provider, nil })); err != nil {
		t.Fatal(err)
	}
	clock := testkit.NewAutoClock(time.Now())
	failedCache := testkit.NewResponseCache(clock.Now)
	failedCache.SetFailure(context.DeadlineExceeded)
	engine, err := llmgateway.New(llmgateway.Dependencies{
		Catalog: testkit.NewCatalogSource(document), Adapters: registry, Authorizer: testkit.AllowAuthorizer{}, Accounting: testkit.NewAccountingStore(),
		Counters: testkit.NewCounterStore(clock.Now), Cache: failedCache, Guardrails: guardrail.NewRegistry(),
		Telemetry: llmgateway.DiscardTelemetrySink{}, Selector: testkit.NewSelector(0), Clock: clock,
	}, llmgateway.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.Reload(context.Background()); err != nil {
		t.Fatal(err)
	}
	zero := 0.0
	request := chatRequest("request", false)
	request.Chat.Temperature = &zero
	if _, err := engine.Invoke(context.Background(), contract.Principal{KeyID: "key"}, request); err != nil {
		t.Fatalf("cache outage affected inference: %v", err)
	}
	if len(provider.Attempts()) != 1 {
		t.Fatalf("provider not called during cache outage: %v", provider.Attempts())
	}
}
