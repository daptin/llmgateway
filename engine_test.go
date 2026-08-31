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

func newEngine(t *testing.T, faultAdapter *testkit.FaultAdapter, accounting *testkit.AccountingStore) *llmgateway.Engine {
	t.Helper()
	registry := adapter.NewRegistry()
	if err := registry.Register("test", adapter.FactoryFunc(func(context.Context, catalog.Provider, adapter.Secret) (adapter.Adapter, error) {
		return faultAdapter, nil
	})); err != nil {
		t.Fatal(err)
	}
	engine, err := llmgateway.New(llmgateway.Dependencies{
		Catalog: testkit.NewCatalogSource(testDocument()), Adapters: registry,
		Authorizer: testkit.AllowAuthorizer{}, Accounting: accounting,
		Selector: testkit.NewSelector(0), Clock: testkit.NewClock(time.Date(2026, 8, 31, 0, 0, 0, 0, time.UTC)),
	}, llmgateway.Options{})
	if err != nil {
		t.Fatal(err)
	}
	return engine
}

func TestEngineReloadInvokeAndDrain(t *testing.T) {
	faultAdapter := testkit.NewFaultAdapter(
		adapter.Capabilities{Operations: map[contract.Operation]bool{contract.OperationChat: true}},
		testkit.AdapterStep{Response: contract.Response{Payload: []byte(`{"ok":true}`), Usage: contract.Usage{InputTokens: 2, OutputTokens: 3, TotalTokens: 5}}},
	)
	store := testkit.NewAccountingStore()
	engine := newEngine(t, faultAdapter, store)
	if _, err := engine.Snapshot(); err != llmgateway.ErrNotReady {
		t.Fatalf("expected ErrNotReady, got %v", err)
	}
	if err := engine.Reload(context.Background()); err != nil {
		t.Fatal(err)
	}
	response, err := engine.Invoke(context.Background(), contract.Principal{KeyID: "key"}, contract.Request{
		ID: "request", Operation: contract.OperationChat, PublicModel: "model",
		EstimatedUsage: contract.Usage{InputTokens: 2, OutputTokens: 8, TotalTokens: 10},
	})
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
		testkit.AdapterStep{Response: contract.Response{Usage: contract.Usage{TotalTokens: 1}}},
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
	engine, err := llmgateway.New(llmgateway.Dependencies{Catalog: source, Adapters: registry, Authorizer: testkit.AllowAuthorizer{}, Accounting: store, Selector: testkit.NewSelector(0), Clock: testkit.NewClock(time.Now())}, llmgateway.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.Reload(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := engine.Invoke(context.Background(), contract.Principal{}, contract.Request{ID: "request", Operation: contract.OperationChat, PublicModel: "model"}); err != nil {
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
	_, err := engine.Invoke(context.Background(), contract.Principal{}, contract.Request{ID: "request", Operation: contract.OperationChat, PublicModel: "model"})
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
