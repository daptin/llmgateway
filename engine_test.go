package llmgateway_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/daptin/llmgateway"
	"github.com/daptin/llmgateway/adapter"
	"github.com/daptin/llmgateway/catalog"
	"github.com/daptin/llmgateway/contract"
	"github.com/daptin/llmgateway/guardrail"
	"github.com/daptin/llmgateway/testkit"
)

func testDocument() catalog.Document {
	return catalog.Document{
		Revision:    1,
		Providers:   []catalog.Provider{{ID: "p", Name: "provider", Type: "test", Enabled: true}},
		Models:      []catalog.Model{{ID: "m", Name: "model", Operations: []contract.Operation{contract.OperationChat}, UnsupportedParameterPolicy: "reject", Enabled: true}},
		Deployments: []catalog.Deployment{{ID: "d", Name: "deployment", ProviderID: "p", ModelID: "m", UpstreamModel: "upstream", Operations: []contract.Operation{contract.OperationChat}, Weight: 1, MaxConcurrency: -1, RPM: -1, TPM: -1, Enabled: true}},
	}
}

func chatRequest(id string, stream bool) contract.Request {
	return contract.Request{
		ID: contract.ID(id), Operation: contract.OperationChat, PublicModel: "model", Stream: stream,
		Chat: &contract.ChatRequest{Messages: []contract.Message{{Role: "user", Content: []contract.ContentPart{{Type: "text", Text: "hello"}}}}, N: 1, MaxCompletionTokens: 10},
	}
}

func newEngine(t *testing.T, faultAdapter *testkit.FaultAdapter, accounting *testkit.AccountingStore) *llmgateway.Engine {
	t.Helper()
	registry := adapter.NewRegistry()
	if err := registry.Register("test", adapter.FactoryFunc(func(context.Context, catalog.Provider, adapter.Secret) (adapter.Adapter, error) {
		return faultAdapter, nil
	})); err != nil {
		t.Fatal(err)
	}
	clock := testkit.NewAutoClock(time.Date(2026, 8, 31, 0, 0, 0, 0, time.UTC))
	engine, err := llmgateway.New(llmgateway.Dependencies{
		Catalog: testkit.NewCatalogSource(testDocument()), Adapters: registry,
		Authorizer: testkit.AllowAuthorizer{}, Accounting: accounting, Counters: testkit.NewCounterStore(clock.Now), Cache: llmgateway.DisabledResponseCache{},
		Guardrails: guardrail.NewRegistry(), Telemetry: llmgateway.DiscardTelemetrySink{}, Selector: testkit.NewSelector(0), Clock: clock,
	}, llmgateway.Options{})
	if err != nil {
		t.Fatal(err)
	}
	return engine
}

func TestEngineReloadInvokeAndDrain(t *testing.T) {
	faultAdapter := testkit.NewFaultAdapter(
		adapter.Capabilities{Operations: map[contract.Operation]bool{contract.OperationChat: true}},
		testkit.AdapterStep{Response: contract.Response{Chat: &contract.ChatResponse{ID: "response"}, Usage: contract.Usage{InputTokens: 2, OutputTokens: 3, TotalTokens: 5}}},
	)
	store := testkit.NewAccountingStore()
	engine := newEngine(t, faultAdapter, store)
	if _, err := engine.Snapshot(); err != llmgateway.ErrNotReady {
		t.Fatalf("expected ErrNotReady, got %v", err)
	}
	if err := engine.Reload(context.Background()); err != nil {
		t.Fatal(err)
	}
	request := chatRequest("request", false)
	request.EstimatedUsage = contract.Usage{InputTokens: 2, OutputTokens: 8, TotalTokens: 10}
	response, err := engine.Invoke(context.Background(), contract.Principal{KeyID: "key"}, request)
	if err != nil {
		t.Fatal(err)
	}
	if response.Model != "model" || response.Usage.TotalTokens != 5 || store.State("request") != "finalized" {
		t.Fatalf("response=%+v accounting=%s", response, store.State("request"))
	}
	if err := engine.Drain(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := engine.Snapshot(); err != llmgateway.ErrDraining {
		t.Fatalf("expected ErrDraining, got %v", err)
	}
}

func TestEngineRetriesOnlyRetryableFailure(t *testing.T) {
	faultAdapter := testkit.NewFaultAdapter(
		adapter.Capabilities{Operations: map[contract.Operation]bool{contract.OperationChat: true}},
		testkit.AdapterStep{InvokeError: &contract.Error{Code: contract.ErrorRateLimit, Message: "limited", HTTPStatus: 429, Retryable: true}},
		testkit.AdapterStep{Response: contract.Response{Chat: &contract.ChatResponse{ID: "response"}, Usage: contract.Usage{TotalTokens: 1}}},
	)
	document := testDocument()
	document.Deployments = append(document.Deployments, catalog.Deployment{ID: "d2", Name: "second", ProviderID: "p", ModelID: "m", UpstreamModel: "other", Operations: []contract.Operation{contract.OperationChat}, Weight: 1, MaxConcurrency: -1, RPM: -1, TPM: -1, Enabled: true})
	source := testkit.NewCatalogSource(document)
	registry := adapter.NewRegistry()
	if err := registry.Register("test", adapter.FactoryFunc(func(context.Context, catalog.Provider, adapter.Secret) (adapter.Adapter, error) {
		return faultAdapter, nil
	})); err != nil {
		t.Fatal(err)
	}
	store := testkit.NewAccountingStore()
	clock := testkit.NewAutoClock(time.Now())
	engine, err := llmgateway.New(llmgateway.Dependencies{Catalog: source, Adapters: registry, Authorizer: testkit.AllowAuthorizer{}, Accounting: store, Counters: testkit.NewCounterStore(clock.Now), Cache: llmgateway.DisabledResponseCache{}, Guardrails: guardrail.NewRegistry(), Telemetry: llmgateway.DiscardTelemetrySink{}, Selector: testkit.NewSelector(0), Clock: clock}, llmgateway.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.Reload(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := engine.Invoke(context.Background(), contract.Principal{}, chatRequest("request", false)); err != nil {
		t.Fatal(err)
	}
	if attempts := faultAdapter.Attempts(); len(attempts) != 2 || attempts[0] == attempts[1] {
		t.Fatalf("expected two different deployments, got %v", attempts)
	}
}

func TestEngineDoesNotRetryNonRetryableFailure(t *testing.T) {
	faultAdapter := testkit.NewFaultAdapter(
		adapter.Capabilities{Operations: map[contract.Operation]bool{contract.OperationChat: true}},
		testkit.AdapterStep{InvokeError: errors.New("bad request")},
	)
	store := testkit.NewAccountingStore()
	engine := newEngine(t, faultAdapter, store)
	if err := engine.Reload(context.Background()); err != nil {
		t.Fatal(err)
	}
	_, err := engine.Invoke(context.Background(), contract.Principal{}, chatRequest("request", false))
	if err == nil {
		t.Fatal("expected provider failure")
	}
	if attempts := faultAdapter.Attempts(); len(attempts) != 1 {
		t.Fatalf("expected one attempt, got %v", attempts)
	}
	if store.State("request") != "finalized" {
		t.Fatalf("accounting state=%s", store.State("request"))
	}
}
