package llmgateway_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
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

func testDocument() catalog.Document {
	return catalog.Document{
		Revision:    1,
		Providers:   []catalog.Provider{{ID: "p", Name: "provider", Type: "test", Enabled: true}},
		Models:      []catalog.Model{{ID: "m", Name: "model", Operations: []contract.Operation{contract.OperationChat}, RoutingStrategy: "priority_weighted", UnsupportedParameterPolicy: "reject", Enabled: true}},
		Deployments: []catalog.Deployment{{ID: "d", Name: "deployment", ProviderID: "p", ModelID: "m", UpstreamModel: "upstream", Operations: []contract.Operation{contract.OperationChat}, Weight: 1, MaxConcurrency: -1, RPM: -1, TPM: -1, Enabled: true}},
	}
}

func chatRequest(id string, stream bool) contract.Request {
	return contract.Request{
		ID: contract.ID(id), Operation: contract.OperationChat, PublicModel: "model", Stream: stream,
		Chat: &contract.ChatRequest{Messages: []contract.Message{{Role: "user", Content: []contract.ContentPart{{Type: "text", Text: "hello"}}}}, N: 1, MaxCompletionTokens: 10},
	}
}

func newEngine(t *testing.T, faultAdapter *testkit.FaultAdapter, metering llmgateway.MeteringPort, sinks ...llmgateway.TelemetrySink) *llmgateway.Engine {
	return newEngineForDocument(t, testDocument(), faultAdapter, metering, sinks...)
}

func newEngineForDocument(t *testing.T, document catalog.Document, faultAdapter *testkit.FaultAdapter, metering llmgateway.MeteringPort, sinks ...llmgateway.TelemetrySink) *llmgateway.Engine {
	t.Helper()
	registry := adapter.NewRegistry()
	if err := registry.Register("test", adapter.FactoryFunc(func(context.Context, catalog.Provider, adapter.Secret) (adapter.Adapter, error) {
		return faultAdapter, nil
	})); err != nil {
		t.Fatal(err)
	}
	clock := testkit.NewAutoClock(time.Date(2026, 8, 31, 0, 0, 0, 0, time.UTC))
	telemetry := llmgateway.TelemetrySink(llmgateway.DiscardTelemetrySink{})
	if len(sinks) == 1 {
		telemetry = sinks[0]
	}
	engine, err := llmgateway.New(llmgateway.Dependencies{
		Catalog: testkit.NewCatalogSource(document), Adapters: registry,
		Authorizer: testkit.AllowAuthorizer{}, Metering: metering, Counters: testkit.NewCounterStore(clock.Now), Cache: llmgateway.DisabledResponseCache{},
		Guardrails: guardrail.NewRegistry(), Telemetry: telemetry, Selector: testkit.NewSelector(0), Clock: clock,
	}, llmgateway.Options{})
	if err != nil {
		t.Fatal(err)
	}
	return engine
}

type captureTelemetry struct {
	mu     sync.Mutex
	events []llmgateway.TelemetryEvent
}

type deploymentValidatingAdapter struct {
	*testkit.FaultAdapter
	err error
}

type closeTrackingAdapter struct {
	*testkit.FaultAdapter
	validationErr error
	closePanic    bool
	closes        atomic.Int64
}

type panickingReloadAdapter struct {
	*testkit.FaultAdapter
	capabilities bool
	validation   bool
}

type capabilityCountingAdapter struct {
	adapter.Adapter
	calls atomic.Int64
}

type terminalFailureMetering struct {
	*testkit.MeteringRecorder
	completeCalls atomic.Int64
	cancelCalls   atomic.Int64
}

func (metering *terminalFailureMetering) Complete(context.Context, contract.Completion) error {
	metering.completeCalls.Add(1)
	return errors.New("terminal persistence unavailable")
}

func (metering *terminalFailureMetering) Cancel(ctx context.Context, cancellation contract.Cancellation) error {
	metering.cancelCalls.Add(1)
	return metering.MeteringRecorder.Cancel(ctx, cancellation)
}

func (instance *capabilityCountingAdapter) Capabilities() adapter.Capabilities {
	instance.calls.Add(1)
	return instance.Adapter.Capabilities()
}

func (adapter *closeTrackingAdapter) ValidateDeployment(catalog.Deployment) error {
	return adapter.validationErr
}

func (adapter *closeTrackingAdapter) CloseIdleConnections() {
	adapter.closes.Add(1)
	if adapter.closePanic {
		panic("cleanup payload must not escape")
	}
}

func (instance *panickingReloadAdapter) Capabilities() adapter.Capabilities {
	if instance.capabilities {
		panic("capability payload must not escape")
	}
	return instance.FaultAdapter.Capabilities()
}

func (instance *panickingReloadAdapter) ValidateDeployment(catalog.Deployment) error {
	if instance.validation {
		panic("validation payload must not escape")
	}
	return nil
}

func (a deploymentValidatingAdapter) ValidateDeployment(catalog.Deployment) error { return a.err }

func (s *captureTelemetry) Record(_ context.Context, event llmgateway.TelemetryEvent) {
	s.mu.Lock()
	s.events = append(s.events, event)
	s.mu.Unlock()
}

func (s *captureTelemetry) snapshot() []llmgateway.TelemetryEvent {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]llmgateway.TelemetryEvent(nil), s.events...)
}

func TestEngineReloadInvokeAndDrain(t *testing.T) {
	faultAdapter := testkit.NewFaultAdapter(
		adapter.Capabilities{Operations: map[contract.Operation]bool{contract.OperationChat: true}},
		testkit.AdapterStep{Response: contract.Response{Chat: &contract.ChatResponse{ID: "response"}, Usage: contract.Usage{InputTokens: 2, OutputTokens: 3, TotalTokens: 5}}},
	)
	store := testkit.NewMeteringRecorder()
	engine := newEngine(t, faultAdapter, store)
	handler, err := engine.Handler(llmgateway.HTTPOptions{Authenticator: llmgateway.AuthenticatorFunc(func(context.Context, string) (contract.Principal, error) {
		return contract.Principal{}, nil
	})})
	if err != nil {
		t.Fatalf("construct reusable HTTP handler: %v", err)
	}
	assertReadyStatus(t, handler, http.StatusServiceUnavailable)
	if _, err := engine.Snapshot(); err != llmgateway.ErrNotReady {
		t.Fatalf("expected ErrNotReady, got %v", err)
	}
	if err := engine.Reload(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := engine.Reload(context.Background()); !errors.Is(err, catalog.ErrStaleRevision) {
		t.Fatalf("expected unchanged catalog result, got %v", err)
	}
	if status := engine.Status(); status.Degraded {
		t.Fatalf("unchanged catalog degraded readiness: %+v", status)
	}
	assertReadyStatus(t, handler, http.StatusOK)
	request := chatRequest("request", false)
	request.EstimatedUsage = contract.Usage{InputTokens: 2, OutputTokens: 8, TotalTokens: 10}
	response, err := engine.Invoke(context.Background(), contract.Principal{KeyID: "key"}, request)
	if err != nil {
		t.Fatal(err)
	}
	if response.Model != "model" || response.Usage.TotalTokens != 5 || store.State("request") != "finalized" {
		t.Fatalf("response=%+v metering=%s", response, store.State("request"))
	}
	if err := engine.Drain(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := engine.Snapshot(); err != llmgateway.ErrDraining {
		t.Fatalf("expected ErrDraining, got %v", err)
	}
	assertReadyStatus(t, handler, http.StatusServiceUnavailable)
}

func TestCompletionFailureDoesNotCancelBillableAdmission(t *testing.T) {
	provider := testkit.NewFaultAdapter(
		adapter.Capabilities{Operations: map[contract.Operation]bool{contract.OperationChat: true}},
		testkit.AdapterStep{Response: contract.Response{Chat: &contract.ChatResponse{ID: "response"}, Usage: contract.Usage{InputTokens: 1, OutputTokens: 1, TotalTokens: 2}}},
	)
	metering := &terminalFailureMetering{MeteringRecorder: testkit.NewMeteringRecorder()}
	engine := newEngine(t, provider, metering)
	if err := engine.Reload(context.Background()); err != nil {
		t.Fatal(err)
	}
	_, err := engine.Invoke(context.Background(), contract.Principal{}, chatRequest("terminal-failure", false))
	var public *contract.Error
	if !errors.As(err, &public) || public.Code != contract.ErrorInternal {
		t.Fatalf("completion failure = %v", err)
	}
	if metering.completeCalls.Load() != 1 || metering.cancelCalls.Load() != 0 {
		t.Fatalf("terminal calls: complete=%d cancel=%d", metering.completeCalls.Load(), metering.cancelCalls.Load())
	}
	if state := metering.State("terminal-failure"); state != "held" {
		t.Fatalf("completion failure changed reservation to %q", state)
	}
}

func TestAdapterCapabilitiesAreSnapshottedAtReload(t *testing.T) {
	provider := &capabilityCountingAdapter{Adapter: testkit.NewFaultAdapter(
		adapter.Capabilities{Operations: map[contract.Operation]bool{contract.OperationChat: true}},
		testkit.AdapterStep{Response: contract.Response{
			Chat: &contract.ChatResponse{ID: "first"}, Usage: contract.Usage{InputTokens: 1, TotalTokens: 1},
		}},
		testkit.AdapterStep{Response: contract.Response{
			Chat: &contract.ChatResponse{ID: "second"}, Usage: contract.Usage{InputTokens: 1, TotalTokens: 1},
		}},
	)}
	registry := adapter.NewRegistry()
	if err := registry.Register("test", adapter.FactoryFunc(func(context.Context, catalog.Provider, adapter.Secret) (adapter.Adapter, error) {
		return provider, nil
	})); err != nil {
		t.Fatal(err)
	}
	clock := testkit.NewAutoClock(time.Now())
	engine, err := llmgateway.New(llmgateway.Dependencies{
		Catalog: testkit.NewCatalogSource(testDocument()), Adapters: registry,
		Authorizer: testkit.AllowAuthorizer{}, Metering: testkit.NewMeteringRecorder(),
		Counters: testkit.NewCounterStore(clock.Now), Cache: llmgateway.DisabledResponseCache{},
		Guardrails: guardrail.NewRegistry(), Telemetry: llmgateway.DiscardTelemetrySink{},
		Selector: testkit.NewSelector(0), Clock: clock,
	}, llmgateway.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.Reload(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, requestID := range []string{"first", "second"} {
		if _, err := engine.Invoke(context.Background(), contract.Principal{KeyID: "key"}, chatRequest(requestID, false)); err != nil {
			t.Fatal(err)
		}
	}
	if calls := provider.calls.Load(); calls != 1 {
		t.Fatalf("adapter Capabilities calls = %d, want one call during reload", calls)
	}
}

func TestDrainBeforeFirstReloadIsIdempotent(t *testing.T) {
	engine := newEngine(t, testkit.NewFaultAdapter(adapter.Capabilities{
		Operations: map[contract.Operation]bool{contract.OperationChat: true},
	}), testkit.NewMeteringRecorder())
	if err := engine.Drain(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := engine.Drain(context.Background()); err != nil {
		t.Fatal(err)
	}
	if status := engine.Status(); !status.Draining || status.Ready {
		t.Fatalf("status after pre-reload drain = %+v", status)
	}
}

func TestReloadAndDrainCloseOnlyUnreachableAdapterSnapshots(t *testing.T) {
	document := testDocument()
	source := testkit.NewCatalogSource(document)
	first := &closeTrackingAdapter{FaultAdapter: testkit.NewFaultAdapter(adapter.Capabilities{Operations: map[contract.Operation]bool{contract.OperationChat: true}})}
	second := &closeTrackingAdapter{FaultAdapter: testkit.NewFaultAdapter(adapter.Capabilities{Operations: map[contract.Operation]bool{contract.OperationChat: true}})}
	failed := &closeTrackingAdapter{FaultAdapter: testkit.NewFaultAdapter(adapter.Capabilities{Operations: map[contract.Operation]bool{contract.OperationChat: true}}), validationErr: errors.New("invalid deployment"), closePanic: true}
	instances := []*closeTrackingAdapter{first, second, failed}
	var build atomic.Int64
	registry := adapter.NewRegistry()
	if err := registry.Register("test", adapter.FactoryFunc(func(context.Context, catalog.Provider, adapter.Secret) (adapter.Adapter, error) {
		return instances[int(build.Add(1))-1], nil
	})); err != nil {
		t.Fatal(err)
	}
	clock := testkit.NewAutoClock(time.Now())
	engine, err := llmgateway.New(llmgateway.Dependencies{
		Catalog: source, Adapters: registry, Authorizer: testkit.AllowAuthorizer{}, Metering: testkit.NewMeteringRecorder(),
		Counters: testkit.NewCounterStore(clock.Now), Cache: llmgateway.DisabledResponseCache{}, Guardrails: guardrail.NewRegistry(),
		Telemetry: llmgateway.DiscardTelemetrySink{}, Selector: testkit.NewSelector(0), Clock: clock,
	}, llmgateway.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.Reload(context.Background()); err != nil {
		t.Fatal(err)
	}
	document.Revision = 2
	source.Set(document)
	if err := engine.Reload(context.Background()); err != nil {
		t.Fatal(err)
	}
	if first.closes.Load() != 1 || second.closes.Load() != 0 {
		t.Fatalf("reload closes: first=%d second=%d", first.closes.Load(), second.closes.Load())
	}
	document.Revision = 3
	source.Set(document)
	if err := engine.Reload(context.Background()); err == nil {
		t.Fatal("accepted invalid candidate adapter")
	}
	if failed.closes.Load() != 1 || second.closes.Load() != 0 {
		t.Fatalf("rejected reload closes: failed=%d active=%d", failed.closes.Load(), second.closes.Load())
	}
	if err := engine.Drain(context.Background()); err != nil {
		t.Fatal(err)
	}
	if second.closes.Load() != 1 {
		t.Fatalf("active adapter close count = %d, want 1", second.closes.Load())
	}
}

func TestDrainPreventsInProgressReloadFromInstallingANewSnapshot(t *testing.T) {
	document := testDocument()
	source := testkit.NewCatalogSource(document)
	first := &closeTrackingAdapter{FaultAdapter: testkit.NewFaultAdapter(adapter.Capabilities{Operations: map[contract.Operation]bool{contract.OperationChat: true}})}
	candidate := &closeTrackingAdapter{FaultAdapter: testkit.NewFaultAdapter(adapter.Capabilities{Operations: map[contract.Operation]bool{contract.OperationChat: true}})}
	buildStarted := make(chan struct{})
	releaseBuild := make(chan struct{})
	var builds atomic.Int64
	registry := adapter.NewRegistry()
	if err := registry.Register("test", adapter.FactoryFunc(func(context.Context, catalog.Provider, adapter.Secret) (adapter.Adapter, error) {
		if builds.Add(1) == 1 {
			return first, nil
		}
		close(buildStarted)
		<-releaseBuild
		return candidate, nil
	})); err != nil {
		t.Fatal(err)
	}
	clock := testkit.NewAutoClock(time.Now())
	engine, err := llmgateway.New(llmgateway.Dependencies{
		Catalog: source, Adapters: registry, Authorizer: testkit.AllowAuthorizer{}, Metering: testkit.NewMeteringRecorder(),
		Counters: testkit.NewCounterStore(clock.Now), Cache: llmgateway.DisabledResponseCache{}, Guardrails: guardrail.NewRegistry(),
		Telemetry: llmgateway.DiscardTelemetrySink{}, Selector: testkit.NewSelector(0), Clock: clock,
	}, llmgateway.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.Reload(context.Background()); err != nil {
		t.Fatal(err)
	}
	document.Revision = 2
	source.Set(document)
	reloadResult := make(chan error, 1)
	go func() { reloadResult <- engine.Reload(context.Background()) }()
	select {
	case <-buildStarted:
	case <-time.After(time.Second):
		t.Fatal("replacement adapter build did not start")
	}
	if err := engine.Drain(context.Background()); err != nil {
		t.Fatal(err)
	}
	close(releaseBuild)
	if err := <-reloadResult; !errors.Is(err, llmgateway.ErrDraining) {
		t.Fatalf("in-progress reload after drain = %v", err)
	}
	if first.closes.Load() != 1 || candidate.closes.Load() != 1 {
		t.Fatalf("drain/reload closes: active=%d rejected=%d", first.closes.Load(), candidate.closes.Load())
	}
}

func TestReloadContainsAdapterExtensionPanics(t *testing.T) {
	tests := []struct {
		name    string
		stage   string
		factory bool
		adapter *panickingReloadAdapter
	}{
		{name: "factory", stage: "adapter_build", factory: true},
		{name: "capabilities", stage: "adapter_capabilities", adapter: &panickingReloadAdapter{
			FaultAdapter: testkit.NewFaultAdapter(adapter.Capabilities{Operations: map[contract.Operation]bool{contract.OperationChat: true}}), capabilities: true,
		}},
		{name: "deployment validation", stage: "deployment_config", adapter: &panickingReloadAdapter{
			FaultAdapter: testkit.NewFaultAdapter(adapter.Capabilities{Operations: map[contract.Operation]bool{contract.OperationChat: true}}), validation: true,
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			registry := adapter.NewRegistry()
			if err := registry.Register("test", adapter.FactoryFunc(func(context.Context, catalog.Provider, adapter.Secret) (adapter.Adapter, error) {
				if test.factory {
					panic("factory payload must not escape")
				}
				return test.adapter, nil
			})); err != nil {
				t.Fatal(err)
			}
			clock := testkit.NewAutoClock(time.Now())
			engine, err := llmgateway.New(llmgateway.Dependencies{
				Catalog: testkit.NewCatalogSource(testDocument()), Adapters: registry, Authorizer: testkit.AllowAuthorizer{},
				Metering: testkit.NewMeteringRecorder(), Counters: testkit.NewCounterStore(clock.Now), Cache: llmgateway.DisabledResponseCache{},
				Guardrails: guardrail.NewRegistry(), Telemetry: llmgateway.DiscardTelemetrySink{}, Selector: testkit.NewSelector(0), Clock: clock,
			}, llmgateway.Options{})
			if err != nil {
				t.Fatal(err)
			}
			if err := engine.Reload(context.Background()); err == nil || strings.Contains(err.Error(), "payload") {
				t.Fatalf("unsafe reload error: %v", err)
			}
			if status := engine.Status(); status.Ready || status.ReloadStage != test.stage {
				t.Fatalf("status = %+v", status)
			}
		})
	}
}

func TestReloadRejectsInvalidAdapterDeploymentConfiguration(t *testing.T) {
	provider := deploymentValidatingAdapter{
		FaultAdapter: testkit.NewFaultAdapter(adapter.Capabilities{Operations: map[contract.Operation]bool{contract.OperationChat: true}}),
		err:          errors.New("invalid adapter deployment configuration"),
	}
	registry := adapter.NewRegistry()
	if err := registry.Register("test", adapter.FactoryFunc(func(context.Context, catalog.Provider, adapter.Secret) (adapter.Adapter, error) {
		return provider, nil
	})); err != nil {
		t.Fatal(err)
	}
	clock := testkit.NewAutoClock(time.Now())
	engine, err := llmgateway.New(llmgateway.Dependencies{
		Catalog: testkit.NewCatalogSource(testDocument()), Adapters: registry, Authorizer: testkit.AllowAuthorizer{},
		Metering: testkit.NewMeteringRecorder(), Counters: testkit.NewCounterStore(clock.Now), Cache: llmgateway.DisabledResponseCache{},
		Guardrails: guardrail.NewRegistry(), Telemetry: llmgateway.DiscardTelemetrySink{}, Selector: testkit.NewSelector(0), Clock: clock,
	}, llmgateway.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.Reload(context.Background()); err == nil {
		t.Fatal("expected deployment configuration rejection")
	}
	if status := engine.Status(); status.Ready || status.ReloadStage != "deployment_config" {
		t.Fatalf("status = %+v", status)
	}
}

func TestInvokeConservativelySettlesMissingProviderUsage(t *testing.T) {
	provider := testkit.NewFaultAdapter(
		adapter.Capabilities{Operations: map[contract.Operation]bool{contract.OperationChat: true}},
		testkit.AdapterStep{Response: contract.Response{Chat: &contract.ChatResponse{ID: "response"}}},
	)
	store := testkit.NewMeteringRecorder()
	engine := newEngine(t, provider, store)
	if err := engine.Reload(context.Background()); err != nil {
		t.Fatal(err)
	}
	request := chatRequest("missing-usage", false)
	request.EstimatedUsage = contract.Usage{InputTokens: 7, OutputTokens: 11, TotalTokens: 18, Estimated: true}
	response, err := engine.Invoke(context.Background(), contract.Principal{KeyID: "key"}, request)
	if err != nil {
		t.Fatal(err)
	}
	if response.Usage.TotalTokens != 18 || !response.Usage.Estimated {
		t.Fatalf("usage = %+v", response.Usage)
	}
	completion, ok := store.Completion(request.ID)
	if !ok || completion.Usage != response.Usage || len(completion.Attempts) != 1 || completion.Attempts[0].Usage != response.Usage {
		t.Fatalf("completion = %+v, present=%v", completion, ok)
	}
}

func TestEngineAppliesCompiledDefaultsOnceBeforeRoutingAndAccounting(t *testing.T) {
	document := testDocument()
	document.Models[0].DefaultParameters = []byte(`{"chat":{"n":2,"temperature":0,"max_completion_tokens":6}}`)
	provider := testkit.NewFaultAdapter(
		adapter.Capabilities{Operations: map[contract.Operation]bool{contract.OperationChat: true}},
		testkit.AdapterStep{Response: contract.Response{Chat: &contract.ChatResponse{ID: "response"}}},
	)
	engine := newEngineForDocument(t, document, provider, testkit.NewMeteringRecorder())
	if err := engine.Reload(context.Background()); err != nil {
		t.Fatal(err)
	}
	request := chatRequest("model-defaults", false)
	request.Chat.N = 0
	request.Chat.MaxCompletionTokens = 0
	request.EstimatedUsage = contract.Usage{InputTokens: 3, TotalTokens: 3, Estimated: true}
	response, err := engine.Invoke(context.Background(), contract.Principal{KeyID: "key"}, request)
	if err != nil {
		t.Fatal(err)
	}
	requests := provider.Requests()
	if len(requests) != 1 || requests[0].Chat.N != 2 || requests[0].Chat.MaxCompletionTokens != 6 ||
		requests[0].Chat.Temperature == nil || *requests[0].Chat.Temperature != 0 || requests[0].EstimatedUsage.OutputTokens != 12 {
		t.Fatalf("upstream request = %+v", requests)
	}
	if request.Chat.N != 0 || request.Chat.MaxCompletionTokens != 0 {
		t.Fatalf("caller request was mutated: %+v", request.Chat)
	}
	if response.Usage.TotalTokens != 15 || !response.Usage.Estimated {
		t.Fatalf("settled usage = %+v", response.Usage)
	}
}

func TestInvokeRejectsModelCapabilityBeforeAdmission(t *testing.T) {
	provider := testkit.NewFaultAdapter(adapter.Capabilities{Operations: map[contract.Operation]bool{contract.OperationChat: true}})
	store := testkit.NewMeteringRecorder()
	engine := newEngine(t, provider, store)
	if err := engine.Reload(context.Background()); err != nil {
		t.Fatal(err)
	}
	request := chatRequest("unsupported-tools", false)
	request.Chat.Tools = []contract.Tool{{Type: "function", Function: contract.FunctionDefinition{Name: "lookup", Parameters: []byte(`{"type":"object"}`)}}}
	_, err := engine.Invoke(context.Background(), contract.Principal{KeyID: "key"}, request)
	var public *contract.Error
	if !errors.As(err, &public) || public.Code != contract.ErrorInvalidRequest || public.HTTPStatus != http.StatusBadRequest {
		t.Fatalf("error = %v", err)
	}
	if state := store.State(request.ID); state != "" {
		t.Fatalf("unsupported request reached metering: %s", state)
	}
}

func TestModelParameterPoliciesAreExplicitAndDoNotMutateCaller(t *testing.T) {
	tool := contract.Tool{Type: "function", Function: contract.FunctionDefinition{Name: "lookup", Parameters: []byte(`{"type":"object"}`)}}
	for _, test := range []struct {
		name      string
		policy    string
		features  map[string]bool
		wantTools bool
	}{
		{name: "drop", policy: "drop", wantTools: false},
		{name: "passthrough", policy: "passthrough", features: map[string]bool{"tools": true}, wantTools: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			document := testDocument()
			document.Models[0].UnsupportedParameterPolicy = test.policy
			provider := testkit.NewFaultAdapter(
				adapter.Capabilities{Operations: map[contract.Operation]bool{contract.OperationChat: true}, Features: test.features},
				testkit.AdapterStep{Response: contract.Response{Chat: &contract.ChatResponse{ID: "response"}, Usage: contract.Usage{TotalTokens: 1}}},
			)
			engine := newEngineForDocument(t, document, provider, testkit.NewMeteringRecorder())
			if err := engine.Reload(context.Background()); err != nil {
				t.Fatal(err)
			}
			request := chatRequest("policy-"+test.name, false)
			request.Chat.Tools = []contract.Tool{tool}
			if _, err := engine.Invoke(context.Background(), contract.Principal{KeyID: "key"}, request); err != nil {
				t.Fatal(err)
			}
			seen := provider.Requests()
			if len(seen) != 1 || (len(seen[0].Chat.Tools) != 0) != test.wantTools {
				t.Fatalf("upstream request = %+v", seen)
			}
			if len(request.Chat.Tools) != 1 {
				t.Fatalf("caller request was mutated: %+v", request.Chat)
			}
		})
	}
}

func TestDropPolicyRejectsSemanticCapabilityInput(t *testing.T) {
	document := testDocument()
	document.Models[0].UnsupportedParameterPolicy = "drop"
	provider := testkit.NewFaultAdapter(adapter.Capabilities{
		Operations: map[contract.Operation]bool{contract.OperationChat: true}, Features: map[string]bool{"vision": true},
	})
	store := testkit.NewMeteringRecorder()
	engine := newEngineForDocument(t, document, provider, store)
	if err := engine.Reload(context.Background()); err != nil {
		t.Fatal(err)
	}
	request := chatRequest("drop-semantic", false)
	request.Chat.Messages[0].Content = append(request.Chat.Messages[0].Content,
		contract.ContentPart{Type: "image_url", ImageURL: &contract.ImageURL{URL: "https://example.test/image.png"}})
	_, err := engine.Invoke(context.Background(), contract.Principal{KeyID: "key"}, request)
	var public *contract.Error
	if !errors.As(err, &public) || public.Code != contract.ErrorInvalidRequest || store.State(request.ID) != "" || len(provider.Requests()) != 0 {
		t.Fatalf("error=%v metering=%q requests=%d", err, store.State(request.ID), len(provider.Requests()))
	}
}

func TestTerminalTelemetryHasStableSafeDimensions(t *testing.T) {
	provider := testkit.NewFaultAdapter(adapter.Capabilities{Operations: map[contract.Operation]bool{contract.OperationChat: true}},
		testkit.AdapterStep{Response: contract.Response{Usage: contract.Usage{InputTokens: 2, OutputTokens: 3, TotalTokens: 5}}})
	sink := &captureTelemetry{}
	engine := newEngine(t, provider, testkit.NewMeteringRecorder(), sink)
	if err := engine.Reload(context.Background()); err != nil {
		t.Fatal(err)
	}
	request := chatRequest("telemetry-request", false)
	request.EstimatedUsage = contract.Usage{TotalTokens: 10}
	if _, err := engine.Invoke(context.Background(), contract.Principal{KeyID: "key", OwnerID: "owner", TeamID: "team"}, request); err != nil {
		t.Fatal(err)
	}
	events := sink.snapshot()
	if len(events) != 2 || events[0].Name != "attempt.completed" || events[1].Name != "request.completed" {
		t.Fatalf("events=%+v", events)
	}
	terminal := events[1]
	if terminal.RequestID != request.ID || terminal.Revision != 1 || terminal.Attributes["model"] != "model" ||
		terminal.Attributes["key_id"] != "key" || terminal.Measures["total_tokens"] != 5 {
		t.Fatalf("terminal=%+v", terminal)
	}
	for _, event := range events {
		for _, value := range event.Attributes {
			if value == "hello" {
				t.Fatal("telemetry leaked prompt content")
			}
		}
	}
}

func assertReadyStatus(t *testing.T, handler http.Handler, expected int) {
	t.Helper()
	request := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != expected {
		t.Fatalf("ready status = %d, body = %s", response.Code, response.Body.String())
	}
	if !strings.Contains(response.Body.String(), `"active_snapshot_age_seconds":`) {
		t.Fatalf("ready status omitted active snapshot age: %s", response.Body.String())
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
	store := testkit.NewMeteringRecorder()
	clock := testkit.NewAutoClock(time.Now())
	engine, err := llmgateway.New(llmgateway.Dependencies{Catalog: source, Adapters: registry, Authorizer: testkit.AllowAuthorizer{}, Metering: store, Counters: testkit.NewCounterStore(clock.Now), Cache: llmgateway.DisabledResponseCache{}, Guardrails: guardrail.NewRegistry(), Telemetry: llmgateway.DiscardTelemetrySink{}, Selector: testkit.NewSelector(0), Clock: clock}, llmgateway.Options{})
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
	completion, ok := store.Completion("request")
	if !ok || len(completion.Attempts) != 2 {
		t.Fatalf("completion=%+v present=%v", completion, ok)
	}
	if completion.Attempts[0].Number != 1 || completion.Attempts[0].Usage.TotalTokens != 10 ||
		!completion.Attempts[0].Usage.Estimated || completion.Attempts[1].Number != 2 ||
		completion.Attempts[1].Usage.TotalTokens != 1 || completion.Usage.TotalTokens != 11 || !completion.Usage.Estimated {
		t.Fatalf("retry usage did not reconcile: %+v", completion)
	}
}

func TestEngineReservesBoundedExposureForEveryPossibleAttempt(t *testing.T) {
	document := testDocument()
	document.Deployments = append(document.Deployments, catalog.Deployment{
		ID: "d2", Name: "second", ProviderID: "p", ModelID: "m", UpstreamModel: "other",
		Operations: []contract.Operation{contract.OperationChat}, Weight: 1, MaxConcurrency: -1, RPM: -1, TPM: -1, Enabled: true,
	})
	provider := testkit.NewFaultAdapter(
		adapter.Capabilities{Operations: map[contract.Operation]bool{contract.OperationChat: true}},
		testkit.AdapterStep{Response: contract.Response{Chat: &contract.ChatResponse{ID: "must-not-run"}}},
	)
	store := testkit.NewMeteringRecorder()
	store.RejectAdmissions(llmgateway.ErrAdmissionDenied)
	engine := newEngineForDocument(t, document, provider, store)
	if err := engine.Reload(context.Background()); err != nil {
		t.Fatal(err)
	}
	request := chatRequest("retry-budget", false)
	_, err := engine.Invoke(context.Background(), contract.Principal{}, request)
	var public *contract.Error
	if !errors.As(err, &public) || public.Code != contract.ErrorInsufficientQuota || public.HTTPStatus != http.StatusPaymentRequired {
		t.Fatalf("error=%v", err)
	}
	if state := store.State(request.ID); state != "" {
		t.Fatalf("denied retry exposure created usage state %q", state)
	}
	if attempts := provider.Attempts(); len(attempts) != 0 {
		t.Fatalf("provider was called before reserving all retry exposure: %v", attempts)
	}
	admissions := store.Admissions()
	if len(admissions) != 1 || admissions[0].EstimatedUsage.TotalTokens != 20 || admissions[0].ModelID != "m" {
		t.Fatalf("admission did not carry bounded exposure and model identity: %+v", admissions)
	}
}

func TestRejectedNewerCatalogDegradesReadinessWithoutReplacingSnapshot(t *testing.T) {
	source := testkit.NewCatalogSource(testDocument())
	registry := adapter.NewRegistry()
	provider := testkit.NewFaultAdapter(adapter.Capabilities{Operations: map[contract.Operation]bool{contract.OperationChat: true}})
	if err := registry.Register("test", adapter.FactoryFunc(func(context.Context, catalog.Provider, adapter.Secret) (adapter.Adapter, error) { return provider, nil })); err != nil {
		t.Fatal(err)
	}
	clock := testkit.NewClock(time.Now())
	engine, err := llmgateway.New(llmgateway.Dependencies{
		Catalog: source, Adapters: registry, Authorizer: testkit.AllowAuthorizer{}, Metering: testkit.NewMeteringRecorder(),
		Counters: testkit.NewCounterStore(clock.Now), Cache: llmgateway.DisabledResponseCache{}, Guardrails: guardrail.NewRegistry(),
		Telemetry: llmgateway.DiscardTelemetrySink{}, Selector: testkit.NewSelector(0), Clock: clock,
	}, llmgateway.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.Reload(context.Background()); err != nil {
		t.Fatal(err)
	}
	clock.Advance(3 * time.Second)
	invalid := testDocument()
	invalid.Revision = 2
	invalid.Models[0].Name = ""
	source.Set(invalid)
	if err := engine.Reload(context.Background()); err == nil {
		t.Fatal("expected invalid catalog rejection")
	}
	status := engine.Status()
	if status.Revision != 1 || status.ActiveSnapshotAgeSeconds != 3 || status.RejectedRevision != 2 || !status.Degraded || !status.Ready {
		t.Fatalf("status = %+v", status)
	}
	snapshot, err := engine.Snapshot()
	if err != nil || snapshot.Revision() != 1 {
		t.Fatalf("last valid snapshot was not retained: revision=%v err=%v", snapshot, err)
	}
}

func TestEngineDoesNotRetryNonRetryableFailure(t *testing.T) {
	faultAdapter := testkit.NewFaultAdapter(
		adapter.Capabilities{Operations: map[contract.Operation]bool{contract.OperationChat: true}},
		testkit.AdapterStep{InvokeError: errors.New("bad request")},
	)
	store := testkit.NewMeteringRecorder()
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
		t.Fatalf("metering state=%s", store.State("request"))
	}
}

func TestEngineNormalizesMalformedTypedProviderError(t *testing.T) {
	provider := testkit.NewFaultAdapter(
		adapter.Capabilities{Operations: map[contract.Operation]bool{contract.OperationChat: true}},
		testkit.AdapterStep{InvokeError: &contract.Error{Code: "provider-secret", Message: "unsafe", HTTPStatus: 0, Retryable: true, RetryAfter: time.Hour}},
	)
	store := testkit.NewMeteringRecorder()
	engine := newEngine(t, provider, store)
	if err := engine.Reload(context.Background()); err != nil {
		t.Fatal(err)
	}
	_, err := engine.Invoke(context.Background(), contract.Principal{}, chatRequest("malformed-error", false))
	var public *contract.Error
	if !errors.As(err, &public) || public.Code != contract.ErrorProvider || public.HTTPStatus != http.StatusBadGateway || public.Retryable {
		t.Fatalf("malformed typed error normalized to %#v", err)
	}
	if attempts := provider.Attempts(); len(attempts) != 1 {
		t.Fatalf("malformed typed error triggered retries: %v", attempts)
	}
}

func TestDrainWaitsForAdmittedStreamAndRejectsNewWork(t *testing.T) {
	faultAdapter := testkit.NewFaultAdapter(
		adapter.Capabilities{Operations: map[contract.Operation]bool{contract.OperationChat: true}, Features: map[string]bool{"streaming": true}},
		testkit.AdapterStep{Events: []contract.StreamEvent{{Chat: &contract.ChatDelta{ID: "chunk"}}}},
	)
	store := testkit.NewMeteringRecorder()
	engine := newEngine(t, faultAdapter, store)
	if err := engine.Reload(context.Background()); err != nil {
		t.Fatal(err)
	}
	stream, err := engine.Stream(context.Background(), contract.Principal{}, chatRequest("stream", true))
	if err != nil {
		t.Fatal(err)
	}
	drainContext, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if err := engine.Drain(drainContext); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected drain deadline while stream is active, got %v", err)
	}
	if _, err := engine.Invoke(context.Background(), contract.Principal{}, chatRequest("late", false)); !errors.Is(err, llmgateway.ErrDraining) {
		t.Fatalf("expected new request rejection, got %v", err)
	}
	if err := stream.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := engine.Drain(context.Background()); err != nil {
		t.Fatal(err)
	}
	if state := store.State("stream"); state != "cancelled" {
		t.Fatalf("stream metering state=%s", state)
	}
}
