package llmgateway_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
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

func guardedDocument(phase string) catalog.Document {
	document := testDocument()
	document.Models[0].GuardrailIDs = []contract.ID{"g"}
	document.Guardrails = []catalog.Guardrail{{
		ID: "g", Name: "deny-secret", Kind: "phrase", Phase: phase, Priority: 1,
		Config: json.RawMessage(`{"mode":"deny","patterns":["secret"]}`), FailMode: "closed", Enabled: true,
	}}
	return document
}

func guardedEngine(t *testing.T, phase string, provider adapter.Adapter, metering *testkit.MeteringRecorder) *llmgateway.Engine {
	engine := newGuardedEngine(t, phase, provider, metering, guardrail.PhraseFactory{})
	if err := engine.Reload(context.Background()); err != nil {
		t.Fatal(err)
	}
	return engine
}

func newGuardedEngine(t *testing.T, phase string, provider adapter.Adapter, metering *testkit.MeteringRecorder, factory guardrail.Factory) *llmgateway.Engine {
	return newGuardedEngineForDocument(t, guardedDocument(phase), provider, metering, factory)
}

func newGuardedEngineForDocument(t *testing.T, document catalog.Document, provider adapter.Adapter, metering *testkit.MeteringRecorder, factory guardrail.Factory) *llmgateway.Engine {
	t.Helper()
	adapters := adapter.NewRegistry()
	if err := adapters.Register("test", adapter.FactoryFunc(func(context.Context, catalog.Provider, adapter.Secret) (adapter.Adapter, error) { return provider, nil })); err != nil {
		t.Fatal(err)
	}
	guardrails := guardrail.NewRegistry()
	if err := guardrails.Register("phrase", factory); err != nil {
		t.Fatal(err)
	}
	clock := testkit.NewAutoClock(time.Now())
	engine, err := llmgateway.New(llmgateway.Dependencies{
		Catalog: testkit.NewCatalogSource(document), Adapters: adapters, Authorizer: testkit.AllowAuthorizer{},
		Metering: metering, Counters: testkit.NewCounterStore(clock.Now), Cache: llmgateway.DisabledResponseCache{}, Guardrails: guardrails,
		Telemetry: llmgateway.DiscardTelemetrySink{}, Selector: testkit.NewSelector(0), Clock: clock,
	}, llmgateway.Options{})
	if err != nil {
		t.Fatal(err)
	}
	return engine
}

type panickingGuardrail struct {
	propertyPanic bool
	checkPanic    bool
	closes        *atomic.Int64
}

func (checker panickingGuardrail) CheckInput(context.Context, contract.Request) (guardrail.Decision, error) {
	if checker.checkPanic {
		panic("guardrail payload must not escape")
	}
	return guardrail.Decision{Allowed: true}, nil
}

func (panickingGuardrail) CheckOutput(context.Context, contract.Request, contract.Response) (guardrail.Decision, error) {
	return guardrail.Decision{Allowed: true}, nil
}

func (panickingGuardrail) CheckStream(context.Context, contract.Request, contract.StreamEvent) (guardrail.Decision, error) {
	return guardrail.Decision{Allowed: true}, nil
}

func (checker panickingGuardrail) SupportsStreaming() bool {
	if checker.propertyPanic {
		panic("guardrail property payload must not escape")
	}
	return true
}

func (panickingGuardrail) CacheStable() bool { return true }

func (checker panickingGuardrail) CloseIdleConnections() {
	if checker.closes != nil {
		checker.closes.Add(1)
	}
}

func TestInputGuardrailRunsBeforeAccountingAndProvider(t *testing.T) {
	provider := testkit.NewFaultAdapter(adapter.Capabilities{Operations: map[contract.Operation]bool{contract.OperationChat: true}}, testkit.AdapterStep{Response: contract.Response{Chat: &contract.ChatResponse{ID: "unused"}}})
	metering := testkit.NewMeteringRecorder()
	engine := guardedEngine(t, "input", provider, metering)
	request := chatRequest("request", false)
	request.Chat.Messages[0].Content[0].Text = "a secret"
	_, err := engine.Invoke(context.Background(), contract.Principal{}, request)
	var typed *contract.Error
	if !errors.As(err, &typed) || typed.Code != contract.ErrorPermission {
		t.Fatalf("expected guardrail rejection, got %v", err)
	}
	if metering.State("request") != "" || len(provider.Attempts()) != 0 {
		t.Fatalf("input rejection had side effects: metering=%s attempts=%v", metering.State("request"), provider.Attempts())
	}
}

func TestOutputGuardrailFinalizesProviderUsageAsRejected(t *testing.T) {
	provider := testkit.NewFaultAdapter(
		adapter.Capabilities{Operations: map[contract.Operation]bool{contract.OperationChat: true}},
		testkit.AdapterStep{Response: contract.Response{Chat: &contract.ChatResponse{ID: "response", Choices: []contract.ChatChoice{{Message: contract.Message{Role: "assistant", Content: []contract.ContentPart{{Type: "text", Text: "a secret"}}}}}}, Usage: contract.Usage{InputTokens: 2, OutputTokens: 2, TotalTokens: 4}}},
	)
	metering := testkit.NewMeteringRecorder()
	engine := guardedEngine(t, "output", provider, metering)
	_, err := engine.Invoke(context.Background(), contract.Principal{}, chatRequest("request", false))
	var typed *contract.Error
	if !errors.As(err, &typed) || typed.Code != contract.ErrorPermission {
		t.Fatalf("expected output rejection, got %v", err)
	}
	completion, ok := metering.Completion("request")
	if !ok || completion.Status != "rejected" || completion.Usage.TotalTokens != 4 || len(completion.Attempts) != 1 || completion.Attempts[0].Outcome != "succeeded" {
		t.Fatalf("incorrect rejected metering: %#v", completion)
	}
}

func TestWholeOutputGuardrailRejectsStreamingBeforeAccounting(t *testing.T) {
	provider := testkit.NewFaultAdapter(adapter.Capabilities{Operations: map[contract.Operation]bool{contract.OperationChat: true}})
	metering := testkit.NewMeteringRecorder()
	engine := guardedEngine(t, "output", provider, metering)
	_, err := engine.Stream(context.Background(), contract.Principal{}, chatRequest("request", true))
	var typed *contract.Error
	if !errors.As(err, &typed) || typed.Code != contract.ErrorInvalidRequest {
		t.Fatalf("expected streaming safety rejection, got %v", err)
	}
	if metering.State("request") != "" || len(provider.Attempts()) != 0 {
		t.Fatalf("streaming rejection had side effects: metering=%s attempts=%v", metering.State("request"), provider.Attempts())
	}
}

func TestReloadContainsGuardrailPropertyPanic(t *testing.T) {
	provider := testkit.NewFaultAdapter(adapter.Capabilities{Operations: map[contract.Operation]bool{contract.OperationChat: true}})
	var closes atomic.Int64
	engine := newGuardedEngine(t, "input", provider, testkit.NewMeteringRecorder(), guardrail.FactoryFunc(func(catalog.Guardrail) (guardrail.Checker, error) {
		return panickingGuardrail{propertyPanic: true, closes: &closes}, nil
	}))
	if err := engine.Reload(context.Background()); err == nil || strings.Contains(err.Error(), "payload") {
		t.Fatalf("unsafe reload error: %v", err)
	}
	if status := engine.Status(); status.Ready || status.ReloadStage != "guardrail_config" {
		t.Fatalf("status = %+v", status)
	}
	if closes.Load() != 1 {
		t.Fatalf("rejected guardrail cleanup count = %d, want 1", closes.Load())
	}
}

func TestGuardrailPanicObeysConfiguredFailureMode(t *testing.T) {
	tests := []struct {
		name      string
		failMode  string
		auditOnly bool
		wantCode  contract.ErrorCode
	}{
		{name: "closed", failMode: "closed", wantCode: contract.ErrorUnavailable},
		{name: "open", failMode: "open"},
		{name: "audit only", failMode: "closed", auditOnly: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			requestID := contract.ID("guardrail-panic-" + strings.ReplaceAll(test.name, " ", "-"))
			provider := testkit.NewFaultAdapter(
				adapter.Capabilities{Operations: map[contract.Operation]bool{contract.OperationChat: true}},
				testkit.AdapterStep{Response: contract.Response{Chat: &contract.ChatResponse{ID: "response"}, Usage: contract.Usage{TotalTokens: 1}}},
			)
			metering := testkit.NewMeteringRecorder()
			document := guardedDocument("input")
			document.Guardrails[0].FailMode = test.failMode
			document.Guardrails[0].AuditOnly = test.auditOnly
			engine := newGuardedEngineForDocument(t, document, provider, metering, guardrail.FactoryFunc(func(catalog.Guardrail) (guardrail.Checker, error) {
				return panickingGuardrail{checkPanic: true}, nil
			}))
			if err := engine.Reload(context.Background()); err != nil {
				t.Fatal(err)
			}
			_, err := engine.Invoke(context.Background(), contract.Principal{}, chatRequest(string(requestID), false))
			if test.wantCode != "" {
				var typed *contract.Error
				if !errors.As(err, &typed) || typed.Code != test.wantCode || strings.Contains(err.Error(), "payload") {
					t.Fatalf("result = %v", err)
				}
				if metering.State(requestID) != "" || len(provider.Attempts()) != 0 {
					t.Fatalf("closed failure had side effects: state=%s attempts=%d", metering.State(requestID), len(provider.Attempts()))
				}
				return
			}
			if err != nil || metering.State(requestID) != "finalized" || len(provider.Attempts()) != 1 {
				t.Fatalf("open/audit result err=%v state=%s attempts=%d", err, metering.State(requestID), len(provider.Attempts()))
			}
		})
	}
}
