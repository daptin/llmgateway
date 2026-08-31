package llmgateway_test

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/daptin/llmgateway"
	"github.com/daptin/llmgateway/adapter"
	"github.com/daptin/llmgateway/catalog"
	"github.com/daptin/llmgateway/contract"
	"github.com/daptin/llmgateway/testkit"
)

func streamEngine(t *testing.T, document catalog.Document, fault *testkit.FaultAdapter, store *testkit.AccountingStore) *llmgateway.Engine {
	t.Helper()
	registry := adapter.NewRegistry()
	if err := registry.Register("test", adapter.FactoryFunc(func(context.Context, catalog.Provider, adapter.Secret) (adapter.Adapter, error) { return fault, nil })); err != nil {
		t.Fatal(err)
	}
	clock := testkit.NewAutoClock(time.Now())
	engine, err := llmgateway.New(llmgateway.Dependencies{
		Catalog: testkit.NewCatalogSource(document), Adapters: registry, Authorizer: testkit.AllowAuthorizer{},
		Accounting: store, Counters: testkit.NewCounterStore(clock.Now), Selector: testkit.NewSelector(0), Clock: clock,
	}, llmgateway.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.Reload(context.Background()); err != nil {
		t.Fatal(err)
	}
	return engine
}

func TestStreamRetriesBeforeFirstEvent(t *testing.T) {
	document := testDocument()
	document.Deployments = append(document.Deployments, catalog.Deployment{ID: "d2", Name: "second", ProviderID: "p", ModelID: "m", UpstreamModel: "other", Operations: []contract.Operation{contract.OperationChat}, Weight: 1, MaxConcurrency: -1, RPM: -1, TPM: -1, Enabled: true})
	fault := testkit.NewFaultAdapter(
		adapter.Capabilities{Operations: map[contract.Operation]bool{contract.OperationChat: true}},
		testkit.AdapterStep{TerminalError: &contract.Error{Code: contract.ErrorRateLimit, Message: "limited", HTTPStatus: 429, Retryable: true}},
		testkit.AdapterStep{Events: []contract.StreamEvent{{Type: "content_delta", Chat: &contract.ChatDelta{Content: "ok"}}, {Type: "finish", Usage: &contract.Usage{TotalTokens: 1}, Terminal: true}}},
	)
	store := testkit.NewAccountingStore()
	stream, err := streamEngine(t, document, fault, store).Stream(context.Background(), contract.Principal{}, chatRequest("request", true))
	if err != nil {
		t.Fatal(err)
	}
	event, err := stream.Next(context.Background())
	if err != nil || !event.OutputCommitted {
		t.Fatalf("first event=%+v err=%v", event, err)
	}
	if _, err := stream.Next(context.Background()); err != nil {
		t.Fatal(err)
	}
	if store.State("request") != "finalized" {
		t.Fatalf("state=%s", store.State("request"))
	}
	if attempts := fault.Attempts(); len(attempts) != 2 || attempts[0] == attempts[1] {
		t.Fatalf("expected pre-commit failover, got %v", attempts)
	}
}

func TestStreamNeverRetriesAfterFirstEvent(t *testing.T) {
	fault := testkit.NewFaultAdapter(
		adapter.Capabilities{Operations: map[contract.Operation]bool{contract.OperationChat: true}},
		testkit.AdapterStep{Events: []contract.StreamEvent{{Type: "content_delta", Chat: &contract.ChatDelta{Content: "partial"}}}, TerminalError: &contract.Error{Code: contract.ErrorProvider, Message: "reset", HTTPStatus: 502, Retryable: true}},
	)
	store := testkit.NewAccountingStore()
	stream, err := streamEngine(t, testDocument(), fault, store).Stream(context.Background(), contract.Principal{}, chatRequest("request", true))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := stream.Next(context.Background()); err != nil {
		t.Fatal(err)
	}
	_, err = stream.Next(context.Background())
	var gatewayError *contract.Error
	if !errors.As(err, &gatewayError) || gatewayError.Code != contract.ErrorProvider {
		t.Fatalf("expected provider error, got %v", err)
	}
	if attempts := fault.Attempts(); len(attempts) != 1 {
		t.Fatalf("post-commit retry occurred: %v", attempts)
	}
	if store.State("request") != "finalized" {
		t.Fatalf("state=%s", store.State("request"))
	}
}

func TestStreamCloseCancelsAccounting(t *testing.T) {
	fault := testkit.NewFaultAdapter(
		adapter.Capabilities{Operations: map[contract.Operation]bool{contract.OperationChat: true}},
		testkit.AdapterStep{Events: []contract.StreamEvent{{Type: "content_delta", Chat: &contract.ChatDelta{Content: "partial"}}}},
	)
	store := testkit.NewAccountingStore()
	stream, err := streamEngine(t, testDocument(), fault, store).Stream(context.Background(), contract.Principal{}, chatRequest("request", true))
	if err != nil {
		t.Fatal(err)
	}
	if err := stream.Close(context.Background()); err != nil && !errors.Is(err, io.EOF) {
		t.Fatal(err)
	}
	if store.State("request") != "cancelled" {
		t.Fatalf("state=%s", store.State("request"))
	}
}
