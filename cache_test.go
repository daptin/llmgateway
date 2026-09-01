package llmgateway_test

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/daptin/llmgateway"
	"github.com/daptin/llmgateway/adapter"
	exactcache "github.com/daptin/llmgateway/cache"
	"github.com/daptin/llmgateway/catalog"
	"github.com/daptin/llmgateway/contract"
	"github.com/daptin/llmgateway/guardrail"
	"github.com/daptin/llmgateway/testkit"
)

type blockingCacheAdapter struct {
	started chan int
	release chan struct{}
	mu      sync.Mutex
	calls   int
}

type failingAcquireCounter struct {
	llmgateway.CounterStore
	err error
}

func (counter failingAcquireCounter) Acquire(context.Context, string, int64, time.Duration) (string, error) {
	return "", counter.err
}

type cacheFillTelemetry struct {
	waiting chan struct{}
	once    sync.Once
}

func (telemetry *cacheFillTelemetry) Record(_ context.Context, event llmgateway.TelemetryEvent) {
	if event.Name == "cache" && event.RequestID == "fill-contender" && event.Attributes["status"] == "fill_wait" {
		telemetry.once.Do(func() { close(telemetry.waiting) })
	}
}

func (a *blockingCacheAdapter) Capabilities() adapter.Capabilities {
	return adapter.Capabilities{Operations: map[contract.Operation]bool{contract.OperationChat: true}}
}

func (a *blockingCacheAdapter) Invoke(ctx context.Context, _ catalog.Deployment, _ contract.Request) (contract.Response, error) {
	a.mu.Lock()
	a.calls++
	call := a.calls
	a.mu.Unlock()
	select {
	case <-ctx.Done():
		return contract.Response{}, ctx.Err()
	case a.started <- call:
	}
	select {
	case <-ctx.Done():
		return contract.Response{}, ctx.Err()
	case <-a.release:
		return contract.Response{Chat: &contract.ChatResponse{ID: "coalesced"}, Usage: contract.Usage{InputTokens: 2, OutputTokens: 1, TotalTokens: 3}}, nil
	}
}

func (*blockingCacheAdapter) Stream(context.Context, catalog.Deployment, contract.Request) (adapter.Stream, error) {
	return nil, errors.New("streaming is not supported")
}

func (a *blockingCacheAdapter) callCount() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.calls
}

func TestExactCacheHitUsesSameAccountingPathAndCannotCrossKeys(t *testing.T) {
	document := testDocument()
	document.Models[0].Capabilities = map[string]bool{"exact_cache": true}
	document.Deployments[0].Pricing.Rates = map[string]int64{"input_tokens": 1_000_000}
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
	metering := testkit.NewMeteringRecorder()
	responseCache := testkit.NewResponseCache(clock.Now)
	engine, err := llmgateway.New(llmgateway.Dependencies{
		Catalog: testkit.NewCatalogSource(document), Adapters: registry, Authorizer: testkit.AllowAuthorizer{}, Metering: metering,
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
	completion, ok := metering.Completion("cached-request")
	if !ok || completion.CacheStatus != "hit" || len(completion.Attempts) != 0 || completion.Usage.TotalTokens != 3 {
		t.Fatalf("cache hit bypassed metering: %#v", completion)
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

func TestExactCacheAdmissionUsesCachedUsageInsteadOfRetryExposure(t *testing.T) {
	document := testDocument()
	document.Models[0].Capabilities = map[string]bool{"exact_cache": true}
	document.Deployments = append(document.Deployments, catalog.Deployment{
		ID: "d2", Name: "second", ProviderID: "p", ModelID: "m", UpstreamModel: "other",
		Operations: []contract.Operation{contract.OperationChat}, Weight: 1, MaxConcurrency: -1, RPM: -1, TPM: -1, Enabled: true,
	})
	provider := testkit.NewFaultAdapter(adapter.Capabilities{Operations: map[contract.Operation]bool{contract.OperationChat: true}})
	registry := adapter.NewRegistry()
	if err := registry.Register("test", adapter.FactoryFunc(func(context.Context, catalog.Provider, adapter.Secret) (adapter.Adapter, error) { return provider, nil })); err != nil {
		t.Fatal(err)
	}
	clock := testkit.NewAutoClock(time.Now())
	metering := testkit.NewMeteringRecorder()
	responseCache := testkit.NewResponseCache(clock.Now)
	engine, err := llmgateway.New(llmgateway.Dependencies{
		Catalog: testkit.NewCatalogSource(document), Adapters: registry, Authorizer: testkit.AllowAuthorizer{}, Metering: metering,
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
	request := chatRequest("cached-budget", false)
	request.Chat.MaxCompletionTokens = 1
	request.Chat.Temperature = &zero
	request.EstimatedUsage = contract.Usage{InputTokens: 2, OutputTokens: 1, TotalTokens: 3, Estimated: true}
	request.MaxOutputTokens = 1
	snapshot, err := engine.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	model, _ := snapshot.ModelByName("model")
	key, err := exactcache.Key(snapshot.Revision(), model, contract.Principal{KeyID: "key"}, request)
	if err != nil {
		t.Fatal(err)
	}
	cached := contract.Response{RequestID: "original", Model: "model", Chat: &contract.ChatResponse{ID: "cached"}, Usage: contract.Usage{InputTokens: 2, OutputTokens: 1, TotalTokens: 3}}
	payload, err := json.Marshal(cached)
	if err != nil {
		t.Fatal(err)
	}
	if err := responseCache.Set(context.Background(), key, payload, time.Minute); err != nil {
		t.Fatal(err)
	}
	response, err := engine.Invoke(context.Background(), contract.Principal{KeyID: "key"}, request)
	if err != nil {
		t.Fatal(err)
	}
	completion, ok := metering.Completion(request.ID)
	if response.Chat == nil || response.Chat.ID != "cached" || !ok || completion.CacheStatus != "hit" ||
		completion.Usage.TotalTokens != 3 || completion.Usage.CostMicros != 0 || len(completion.Attempts) != 0 {
		t.Fatalf("response=%+v completion=%+v present=%v", response, completion, ok)
	}
	if attempts := provider.Attempts(); len(attempts) != 0 {
		t.Fatalf("cached request called provider: %v", attempts)
	}
	admissions := metering.Admissions()
	if len(admissions) != 1 || admissions[0].EstimatedUsage.TotalTokens != 3 || admissions[0].ModelID != "m" {
		t.Fatalf("cached admission did not carry actual cached exposure: %+v", admissions)
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
		Catalog: testkit.NewCatalogSource(document), Adapters: registry, Authorizer: testkit.AllowAuthorizer{}, Metering: testkit.NewMeteringRecorder(),
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

func TestCacheFillCoordinationFailureDegradesToProvider(t *testing.T) {
	document := testDocument()
	document.Models[0].Capabilities = map[string]bool{"exact_cache": true}
	provider := testkit.NewFaultAdapter(
		adapter.Capabilities{Operations: map[contract.Operation]bool{contract.OperationChat: true}},
		testkit.AdapterStep{Response: contract.Response{Chat: &contract.ChatResponse{ID: "provider"}, Usage: contract.Usage{TotalTokens: 1}}},
	)
	registry := adapter.NewRegistry()
	if err := registry.Register("test", adapter.FactoryFunc(func(context.Context, catalog.Provider, adapter.Secret) (adapter.Adapter, error) {
		return provider, nil
	})); err != nil {
		t.Fatal(err)
	}
	clock := testkit.NewAutoClock(time.Now())
	counters := testkit.NewCounterStore(clock.Now)
	engine, err := llmgateway.New(llmgateway.Dependencies{
		Catalog: testkit.NewCatalogSource(document), Adapters: registry, Authorizer: testkit.AllowAuthorizer{}, Metering: testkit.NewMeteringRecorder(),
		Counters: failingAcquireCounter{CounterStore: counters, err: errors.New("coordination unavailable")},
		Cache:    testkit.NewResponseCache(clock.Now), Guardrails: guardrail.NewRegistry(), Telemetry: llmgateway.DiscardTelemetrySink{},
		Selector: testkit.NewSelector(0), Clock: clock,
	}, llmgateway.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.Reload(context.Background()); err != nil {
		t.Fatal(err)
	}
	zero := 0.0
	request := chatRequest("coordination-failure", false)
	request.Chat.Temperature = &zero
	response, err := engine.Invoke(context.Background(), contract.Principal{KeyID: "key"}, request)
	if err != nil || response.Chat == nil || response.Chat.ID != "provider" {
		t.Fatalf("cache coordination failure affected inference: %#v, %v", response, err)
	}
	if calls := len(provider.Attempts()); calls != 1 {
		t.Fatalf("provider calls = %d, want 1", calls)
	}
}

func TestCacheMissesAreCoalescedAcrossConcurrentRequests(t *testing.T) {
	document := testDocument()
	document.Models[0].Capabilities = map[string]bool{"exact_cache": true}
	provider := &blockingCacheAdapter{started: make(chan int, 2), release: make(chan struct{})}
	registry := adapter.NewRegistry()
	if err := registry.Register("test", adapter.FactoryFunc(func(context.Context, catalog.Provider, adapter.Secret) (adapter.Adapter, error) {
		return provider, nil
	})); err != nil {
		t.Fatal(err)
	}
	metering := testkit.NewMeteringRecorder()
	telemetry := &cacheFillTelemetry{waiting: make(chan struct{})}
	engine, err := llmgateway.New(llmgateway.Dependencies{
		Catalog: testkit.NewCatalogSource(document), Adapters: registry, Authorizer: testkit.AllowAuthorizer{}, Metering: metering,
		Counters: testkit.NewCounterStore(time.Now), Cache: testkit.NewResponseCache(time.Now), Guardrails: guardrail.NewRegistry(),
		Telemetry: telemetry, Selector: testkit.NewSelector(0), Clock: llmgateway.SystemClock{},
	}, llmgateway.Options{CacheFillWait: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.Reload(context.Background()); err != nil {
		t.Fatal(err)
	}
	zero := 0.0
	request := chatRequest("fill-owner", false)
	request.Chat.Temperature = &zero
	type result struct {
		response contract.Response
		err      error
	}
	results := make(chan result, 2)
	go func() {
		response, invokeErr := engine.Invoke(context.Background(), contract.Principal{KeyID: "key"}, request)
		results <- result{response: response, err: invokeErr}
	}()
	select {
	case call := <-provider.started:
		if call != 1 {
			t.Fatalf("first provider call = %d", call)
		}
	case <-time.After(time.Second):
		t.Fatal("fill owner did not reach the provider")
	}
	request.ID = "fill-contender"
	go func() {
		response, invokeErr := engine.Invoke(context.Background(), contract.Principal{KeyID: "key"}, request)
		results <- result{response: response, err: invokeErr}
	}()
	select {
	case <-telemetry.waiting:
	case <-time.After(time.Second):
		t.Fatal("fill contender did not enter the bounded wait")
	}
	close(provider.release)
	for index := 0; index < 2; index++ {
		select {
		case outcome := <-results:
			if outcome.err != nil || outcome.response.Chat == nil || outcome.response.Chat.ID != "coalesced" {
				t.Fatalf("coalesced invocation = %#v, %v", outcome.response, outcome.err)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("coalesced invocation timed out")
		}
	}
	if calls := provider.callCount(); calls != 1 {
		t.Fatalf("provider calls = %d, want 1", calls)
	}
	if _, ok := metering.Completion("fill-owner"); !ok {
		t.Fatal("fill owner was not terminalized")
	}
	completion, ok := metering.Completion("fill-contender")
	if !ok || completion.CacheStatus != "hit" {
		t.Fatalf("fill contender completion = %#v, present=%v", completion, ok)
	}
}

func TestCacheFillWaitIsBoundedAndFallsBackToProvider(t *testing.T) {
	document := testDocument()
	document.Models[0].Capabilities = map[string]bool{"exact_cache": true}
	provider := &blockingCacheAdapter{started: make(chan int, 2), release: make(chan struct{})}
	registry := adapter.NewRegistry()
	if err := registry.Register("test", adapter.FactoryFunc(func(context.Context, catalog.Provider, adapter.Secret) (adapter.Adapter, error) {
		return provider, nil
	})); err != nil {
		t.Fatal(err)
	}
	engine, err := llmgateway.New(llmgateway.Dependencies{
		Catalog: testkit.NewCatalogSource(document), Adapters: registry, Authorizer: testkit.AllowAuthorizer{}, Metering: testkit.NewMeteringRecorder(),
		Counters: testkit.NewCounterStore(time.Now), Cache: testkit.NewResponseCache(time.Now), Guardrails: guardrail.NewRegistry(),
		Telemetry: llmgateway.DiscardTelemetrySink{}, Selector: testkit.NewSelector(0), Clock: llmgateway.SystemClock{},
	}, llmgateway.Options{CacheFillWait: 50 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.Reload(context.Background()); err != nil {
		t.Fatal(err)
	}
	zero := 0.0
	request := chatRequest("bounded-owner", false)
	request.Chat.Temperature = &zero
	results := make(chan error, 2)
	invoke := func(request contract.Request) {
		response, invokeErr := engine.Invoke(context.Background(), contract.Principal{KeyID: "key"}, request)
		if invokeErr == nil && (response.Chat == nil || response.Chat.ID != "coalesced") {
			invokeErr = errors.New("invalid provider response")
		}
		results <- invokeErr
	}
	go invoke(request)
	select {
	case call := <-provider.started:
		if call != 1 {
			t.Fatalf("first provider call = %d", call)
		}
	case <-time.After(time.Second):
		t.Fatal("fill owner did not reach the provider")
	}
	request.ID = "bounded-contender"
	go invoke(request)
	select {
	case call := <-provider.started:
		if call != 2 {
			t.Fatalf("fallback provider call = %d", call)
		}
	case <-time.After(time.Second):
		t.Fatal("cache fill wait did not fall back within its bound")
	}
	close(provider.release)
	for index := 0; index < 2; index++ {
		select {
		case err := <-results:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("fallback invocation timed out")
		}
	}
}
