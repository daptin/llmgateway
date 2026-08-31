package llmgateway_test

import (
	"context"
	"encoding/json"
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

func guardedDocument(phase string) catalog.Document {
	document := testDocument()
	document.Models[0].GuardrailIDs = []contract.ID{"g"}
	document.Guardrails = []catalog.Guardrail{{
		ID: "g", Name: "deny-secret", Kind: "phrase", Phase: phase, Priority: 1,
		Config: json.RawMessage(`{"mode":"deny","patterns":["secret"]}`), FailMode: "closed", Enabled: true,
	}}
	return document
}

func guardedEngine(t *testing.T, phase string, provider adapter.Adapter, accounting *testkit.AccountingStore) *llmgateway.Engine {
	t.Helper()
	adapters := adapter.NewRegistry()
	if err := adapters.Register("test", adapter.FactoryFunc(func(context.Context, catalog.Provider, adapter.Secret) (adapter.Adapter, error) { return provider, nil })); err != nil {
		t.Fatal(err)
	}
	guardrails := guardrail.NewRegistry()
	if err := guardrails.Register("phrase", guardrail.PhraseFactory{}); err != nil {
		t.Fatal(err)
	}
	clock := testkit.NewAutoClock(time.Now())
	engine, err := llmgateway.New(llmgateway.Dependencies{
		Catalog: testkit.NewCatalogSource(guardedDocument(phase)), Adapters: adapters, Authorizer: testkit.AllowAuthorizer{},
		Accounting: accounting, Counters: testkit.NewCounterStore(clock.Now), Cache: llmgateway.DisabledResponseCache{}, Guardrails: guardrails,
		Telemetry: llmgateway.DiscardTelemetrySink{}, Selector: testkit.NewSelector(0), Clock: clock,
	}, llmgateway.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.Reload(context.Background()); err != nil {
		t.Fatal(err)
	}
	return engine
}

func TestInputGuardrailRunsBeforeAccountingAndProvider(t *testing.T) {
	provider := testkit.NewFaultAdapter(adapter.Capabilities{Operations: map[contract.Operation]bool{contract.OperationChat: true}}, testkit.AdapterStep{Response: contract.Response{Chat: &contract.ChatResponse{ID: "unused"}}})
	accounting := testkit.NewAccountingStore()
	engine := guardedEngine(t, "input", provider, accounting)
	request := chatRequest("request", false)
	request.Chat.Messages[0].Content[0].Text = "a secret"
	_, err := engine.Invoke(context.Background(), contract.Principal{}, request)
	var typed *contract.Error
	if !errors.As(err, &typed) || typed.Code != contract.ErrorPermission {
		t.Fatalf("expected guardrail rejection, got %v", err)
	}
	if accounting.State("request") != "" || len(provider.Attempts()) != 0 {
		t.Fatalf("input rejection had side effects: accounting=%s attempts=%v", accounting.State("request"), provider.Attempts())
	}
}

func TestOutputGuardrailFinalizesProviderUsageAsRejected(t *testing.T) {
	provider := testkit.NewFaultAdapter(
		adapter.Capabilities{Operations: map[contract.Operation]bool{contract.OperationChat: true}},
		testkit.AdapterStep{Response: contract.Response{Chat: &contract.ChatResponse{ID: "response", Choices: []contract.ChatChoice{{Message: contract.Message{Role: "assistant", Content: []contract.ContentPart{{Type: "text", Text: "a secret"}}}}}}, Usage: contract.Usage{InputTokens: 2, OutputTokens: 2, TotalTokens: 4}}},
	)
	accounting := testkit.NewAccountingStore()
	engine := guardedEngine(t, "output", provider, accounting)
	_, err := engine.Invoke(context.Background(), contract.Principal{}, chatRequest("request", false))
	var typed *contract.Error
	if !errors.As(err, &typed) || typed.Code != contract.ErrorPermission {
		t.Fatalf("expected output rejection, got %v", err)
	}
	completion, ok := accounting.Completion("request")
	if !ok || completion.Status != "rejected" || completion.Usage.TotalTokens != 4 || len(completion.Attempts) != 1 || completion.Attempts[0].Outcome != "succeeded" {
		t.Fatalf("incorrect rejected accounting: %#v", completion)
	}
}

func TestWholeOutputGuardrailRejectsStreamingBeforeAccounting(t *testing.T) {
	provider := testkit.NewFaultAdapter(adapter.Capabilities{Operations: map[contract.Operation]bool{contract.OperationChat: true}})
	accounting := testkit.NewAccountingStore()
	engine := guardedEngine(t, "output", provider, accounting)
	_, err := engine.Stream(context.Background(), contract.Principal{}, chatRequest("request", true))
	var typed *contract.Error
	if !errors.As(err, &typed) || typed.Code != contract.ErrorInvalidRequest {
		t.Fatalf("expected streaming safety rejection, got %v", err)
	}
	if accounting.State("request") != "" || len(provider.Attempts()) != 0 {
		t.Fatalf("streaming rejection had side effects: accounting=%s attempts=%v", accounting.State("request"), provider.Attempts())
	}
}
