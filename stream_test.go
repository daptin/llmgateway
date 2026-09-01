package llmgateway_test

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/daptin/llmgateway"
	"github.com/daptin/llmgateway/adapter"
	"github.com/daptin/llmgateway/catalog"
	"github.com/daptin/llmgateway/contract"
	"github.com/daptin/llmgateway/guardrail"
	"github.com/daptin/llmgateway/testkit"
)

func streamEngine(t *testing.T, document catalog.Document, provider adapter.Adapter, store llmgateway.MeteringPort, options ...llmgateway.Options) *llmgateway.Engine {
	t.Helper()
	registry := adapter.NewRegistry()
	if err := registry.Register("test", adapter.FactoryFunc(func(context.Context, catalog.Provider, adapter.Secret) (adapter.Adapter, error) { return provider, nil })); err != nil {
		t.Fatal(err)
	}
	clock := testkit.NewAutoClock(time.Now())
	configured := llmgateway.Options{}
	if len(options) == 1 {
		configured = options[0]
	}
	engine, err := llmgateway.New(llmgateway.Dependencies{
		Catalog: testkit.NewCatalogSource(document), Adapters: registry, Authorizer: testkit.AllowAuthorizer{},
		Metering: store, Counters: testkit.NewCounterStore(clock.Now), Cache: llmgateway.DisabledResponseCache{}, Guardrails: guardrail.NewRegistry(), Telemetry: llmgateway.DiscardTelemetrySink{}, Selector: testkit.NewSelector(0), Clock: clock,
	}, configured)
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.Reload(context.Background()); err != nil {
		t.Fatal(err)
	}
	return engine
}

func TestStreamCompletionFailureCannotFallThroughToCancellation(t *testing.T) {
	provider := testkit.NewFaultAdapter(streamCapabilities(), testkit.AdapterStep{Events: []contract.StreamEvent{
		{Type: "content_delta", Chat: &contract.ChatDelta{Content: "ok"}},
		{Type: "finish", Usage: &contract.Usage{InputTokens: 1, OutputTokens: 1, TotalTokens: 2}, Terminal: true},
	}})
	metering := &terminalFailureMetering{MeteringRecorder: testkit.NewMeteringRecorder()}
	stream, err := streamEngine(t, testDocument(), provider, metering).Stream(
		context.Background(), contract.Principal{}, chatRequest("stream-terminal-failure", true),
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := stream.Next(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := stream.Next(context.Background()); err == nil {
		t.Fatal("stream completion persistence failure was hidden")
	}
	if err := stream.Close(context.Background()); err != nil {
		t.Fatalf("close after terminalization failure: %v", err)
	}
	if metering.completeCalls.Load() != 1 || metering.cancelCalls.Load() != 0 {
		t.Fatalf("terminal calls: complete=%d cancel=%d", metering.completeCalls.Load(), metering.cancelCalls.Load())
	}
	if state := metering.State("stream-terminal-failure"); state != "held" {
		t.Fatalf("completion failure changed reservation to %q", state)
	}
}

type waitingAdapter struct {
	first   bool
	entered chan<- struct{}
}

func (waitingAdapter) Capabilities() adapter.Capabilities {
	return streamCapabilities()
}

func streamCapabilities() adapter.Capabilities {
	return adapter.Capabilities{Operations: map[contract.Operation]bool{contract.OperationChat: true}, Features: map[string]bool{"streaming": true}}
}
func (waitingAdapter) Invoke(context.Context, catalog.Deployment, contract.Request) (contract.Response, error) {
	return contract.Response{}, errors.New("not implemented")
}
func (a waitingAdapter) Stream(context.Context, catalog.Deployment, contract.Request) (adapter.Stream, error) {
	return &waitingStream{first: a.first, entered: a.entered}, nil
}

type waitingStream struct {
	first   bool
	entered chan<- struct{}
}

func (s *waitingStream) Next(ctx context.Context) (contract.StreamEvent, error) {
	if s.first {
		s.first = false
		return contract.StreamEvent{Type: "content_delta", Chat: &contract.ChatDelta{Content: "first"}}, nil
	}
	if s.entered != nil {
		s.entered <- struct{}{}
	}
	<-ctx.Done()
	return contract.StreamEvent{}, ctx.Err()
}
func (*waitingStream) Close() error { return nil }

type panickingAdapter struct {
	stage string
}

func (panickingAdapter) Capabilities() adapter.Capabilities { return streamCapabilities() }
func (instance panickingAdapter) Invoke(context.Context, catalog.Deployment, contract.Request) (contract.Response, error) {
	panic("provider invocation payload must not escape")
}
func (instance panickingAdapter) Stream(context.Context, catalog.Deployment, contract.Request) (adapter.Stream, error) {
	if instance.stage == "setup" {
		panic("provider setup payload must not escape")
	}
	return &panickingStream{stage: instance.stage}, nil
}

type panickingStream struct {
	stage string
	first bool
}

func (stream *panickingStream) Next(context.Context) (contract.StreamEvent, error) {
	if stream.stage == "read" {
		panic("provider event payload must not escape")
	}
	if !stream.first {
		stream.first = true
		return contract.StreamEvent{Type: "content_delta", Chat: &contract.ChatDelta{Content: "safe"}}, nil
	}
	return contract.StreamEvent{}, io.EOF
}
func (*panickingStream) Close() error { panic("provider close payload must not escape") }

func TestProviderPanicsAreContainedWithoutLeakingValues(t *testing.T) {
	for _, test := range []struct {
		name  string
		stage string
	}{
		{name: "invoke", stage: "invoke"},
		{name: "stream setup", stage: "setup"},
		{name: "stream read", stage: "read"},
		{name: "stream close", stage: "close"},
	} {
		t.Run(test.name, func(t *testing.T) {
			store := testkit.NewMeteringRecorder()
			engine := streamEngine(t, testDocument(), panickingAdapter{stage: test.stage}, store)
			var err error
			if test.stage == "invoke" {
				_, err = engine.Invoke(context.Background(), contract.Principal{}, chatRequest("panic-invoke", false))
			} else {
				var stream contract.EventStream
				stream, err = engine.Stream(context.Background(), contract.Principal{}, chatRequest("panic-"+test.stage, true))
				if err == nil && test.stage == "close" {
					err = stream.Close(context.Background())
				}
			}
			if err == nil || strings.Contains(err.Error(), "payload must not escape") {
				t.Fatalf("contained panic error = %v", err)
			}
			if cause := errors.Unwrap(err); cause != nil && strings.Contains(cause.Error(), "payload must not escape") {
				t.Fatalf("contained panic cause leaked value: %v", cause)
			}
		})
	}
}

func TestStreamRetriesBeforeFirstEvent(t *testing.T) {
	document := testDocument()
	document.Deployments = append(document.Deployments, catalog.Deployment{ID: "d2", Name: "second", ProviderID: "p", ModelID: "m", UpstreamModel: "other", Operations: []contract.Operation{contract.OperationChat}, Weight: 1, MaxConcurrency: -1, RPM: -1, TPM: -1, Enabled: true})
	fault := testkit.NewFaultAdapter(
		streamCapabilities(),
		testkit.AdapterStep{TerminalError: &contract.Error{Code: contract.ErrorRateLimit, Message: "limited", HTTPStatus: 429, Retryable: true}},
		testkit.AdapterStep{Events: []contract.StreamEvent{{Type: "content_delta", Chat: &contract.ChatDelta{Content: "ok"}}, {Type: "finish", Usage: &contract.Usage{TotalTokens: 1}, Terminal: true}}},
	)
	store := testkit.NewMeteringRecorder()
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
	completion, ok := store.Completion("request")
	if !ok || len(completion.Attempts) != 2 || completion.Attempts[0].Number != 1 ||
		completion.Attempts[0].Usage.TotalTokens != 10 || !completion.Attempts[0].Usage.Estimated ||
		completion.Attempts[1].Number != 2 || completion.Attempts[1].Usage.TotalTokens != 1 ||
		completion.Usage.TotalTokens != 11 || !completion.Usage.Estimated {
		t.Fatalf("stream retry usage did not reconcile: %+v present=%v", completion, ok)
	}
}

func TestStreamFirstEventTimeoutFinalizesWithoutCommit(t *testing.T) {
	store := testkit.NewMeteringRecorder()
	_, err := streamEngine(t, testDocument(), waitingAdapter{}, store, llmgateway.Options{FirstEventTimeout: 10 * time.Millisecond}).Stream(
		context.Background(), contract.Principal{}, chatRequest("first-timeout", true))
	var gatewayError *contract.Error
	if !errors.As(err, &gatewayError) || gatewayError.Code != contract.ErrorTimeout || gatewayError.HTTPStatus != 504 {
		t.Fatalf("timeout error=%v", err)
	}
	if store.State("first-timeout") != "finalized" {
		t.Fatalf("state=%s", store.State("first-timeout"))
	}
}

func TestStreamCancellationBeforeFirstEventCancelsAccounting(t *testing.T) {
	entered := make(chan struct{})
	store := testkit.NewMeteringRecorder()
	engine := streamEngine(t, testDocument(), waitingAdapter{entered: entered}, store)
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		_, err := engine.Stream(ctx, contract.Principal{}, chatRequest("setup-cancelled", true))
		result <- err
	}()
	<-entered
	cancel()
	err := <-result
	var gatewayError *contract.Error
	if !errors.As(err, &gatewayError) || gatewayError.HTTPStatus != 499 {
		t.Fatalf("setup cancellation error=%v", err)
	}
	if store.State("setup-cancelled") != "cancelled" {
		t.Fatalf("setup cancellation state=%s", store.State("setup-cancelled"))
	}
}

func TestNormalizedStreamCancellationCancelsAccounting(t *testing.T) {
	provider := testkit.NewFaultAdapter(streamCapabilities(), testkit.AdapterStep{
		TerminalError: &contract.Error{Code: contract.ErrorProvider, Message: "cancelled", HTTPStatus: 499},
	})
	store := testkit.NewMeteringRecorder()
	_, err := streamEngine(t, testDocument(), provider, store).Stream(
		context.Background(), contract.Principal{}, chatRequest("normalized-cancelled", true))
	var gatewayError *contract.Error
	if !errors.As(err, &gatewayError) || gatewayError.HTTPStatus != 499 {
		t.Fatalf("normalized cancellation error=%v", err)
	}
	if store.State("normalized-cancelled") != "cancelled" {
		t.Fatalf("normalized cancellation state=%s", store.State("normalized-cancelled"))
	}
}

func TestLogicalRequestDeadlineCoversStreamSetup(t *testing.T) {
	store := testkit.NewMeteringRecorder()
	_, err := streamEngine(t, testDocument(), waitingAdapter{}, store, llmgateway.Options{
		RequestTimeout: 10 * time.Millisecond, FirstEventTimeout: time.Second,
	}).Stream(context.Background(), contract.Principal{}, chatRequest("request-timeout", true))
	var gatewayError *contract.Error
	if !errors.As(err, &gatewayError) || gatewayError.Code != contract.ErrorTimeout || gatewayError.HTTPStatus != 504 {
		t.Fatalf("request timeout error=%v", err)
	}
	if store.State("request-timeout") != "finalized" {
		t.Fatalf("state=%s", store.State("request-timeout"))
	}
}

func TestStreamIdleTimeoutTerminatesAfterCommit(t *testing.T) {
	store := testkit.NewMeteringRecorder()
	stream, err := streamEngine(t, testDocument(), waitingAdapter{first: true}, store, llmgateway.Options{StreamIdleTimeout: 10 * time.Millisecond}).Stream(
		context.Background(), contract.Principal{}, chatRequest("idle-timeout", true))
	if err != nil {
		t.Fatal(err)
	}
	if event, err := stream.Next(context.Background()); err != nil || !event.OutputCommitted {
		t.Fatalf("first event=%+v err=%v", event, err)
	}
	_, err = stream.Next(context.Background())
	var gatewayError *contract.Error
	if !errors.As(err, &gatewayError) || gatewayError.Code != contract.ErrorTimeout || gatewayError.HTTPStatus != 504 {
		t.Fatalf("idle error=%v", err)
	}
	if store.State("idle-timeout") != "finalized" {
		t.Fatalf("state=%s", store.State("idle-timeout"))
	}
}

func TestStreamNeverRetriesAfterFirstEvent(t *testing.T) {
	fault := testkit.NewFaultAdapter(
		streamCapabilities(),
		testkit.AdapterStep{Events: []contract.StreamEvent{{Type: "content_delta", Chat: &contract.ChatDelta{Content: "partial"}}}, TerminalError: &contract.Error{Code: contract.ErrorProvider, Message: "reset", HTTPStatus: 502, Retryable: true}},
	)
	store := testkit.NewMeteringRecorder()
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

func TestStreamEOFWithoutTerminalEventIsFailure(t *testing.T) {
	fault := testkit.NewFaultAdapter(
		streamCapabilities(),
		testkit.AdapterStep{Events: []contract.StreamEvent{{Type: "content_delta", Chat: &contract.ChatDelta{Content: "partial"}}}},
	)
	store := testkit.NewMeteringRecorder()
	stream, err := streamEngine(t, testDocument(), fault, store).Stream(
		context.Background(), contract.Principal{}, chatRequest("unexpected-eof", true),
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := stream.Next(context.Background()); err != nil {
		t.Fatal(err)
	}
	_, err = stream.Next(context.Background())
	var gatewayError *contract.Error
	if !errors.As(err, &gatewayError) || gatewayError.Code != contract.ErrorProvider || gatewayError.HTTPStatus != 502 {
		t.Fatalf("unexpected EOF error = %v", err)
	}
	completion, ok := store.Completion("unexpected-eof")
	if !ok || completion.Status != "failed" || completion.ErrorCode != contract.ErrorProvider {
		t.Fatalf("unexpected EOF completion = %+v, present=%v", completion, ok)
	}
}

func TestStreamCloseCancelsAccounting(t *testing.T) {
	fault := testkit.NewFaultAdapter(
		streamCapabilities(),
		testkit.AdapterStep{Events: []contract.StreamEvent{{Type: "content_delta", Chat: &contract.ChatDelta{Content: "partial"}}}},
	)
	store := testkit.NewMeteringRecorder()
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

func TestStreamContextCancellationCancelsAccounting(t *testing.T) {
	fault := testkit.NewFaultAdapter(
		streamCapabilities(),
		testkit.AdapterStep{
			Events:        []contract.StreamEvent{{Type: "content_delta", Chat: &contract.ChatDelta{Content: "partial"}}},
			TerminalError: context.Canceled,
		},
	)
	store := testkit.NewMeteringRecorder()
	stream, err := streamEngine(t, testDocument(), fault, store).Stream(context.Background(), contract.Principal{}, chatRequest("disconnect", true))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := stream.Next(context.Background()); err != nil {
		t.Fatal(err)
	}
	_, err = stream.Next(context.Background())
	var gatewayError *contract.Error
	if !errors.As(err, &gatewayError) || gatewayError.HTTPStatus != 499 {
		t.Fatalf("disconnect error=%v", err)
	}
	if store.State("disconnect") != "cancelled" {
		t.Fatalf("disconnect state=%s", store.State("disconnect"))
	}
}

func TestStreamConservativelySettlesMissingProviderUsage(t *testing.T) {
	fault := testkit.NewFaultAdapter(
		streamCapabilities(),
		testkit.AdapterStep{Events: []contract.StreamEvent{
			{Type: "content_delta", Chat: &contract.ChatDelta{Content: "complete"}},
			{Type: "finish", Terminal: true},
		}},
	)
	store := testkit.NewMeteringRecorder()
	request := chatRequest("stream-missing-usage", true)
	request.EstimatedUsage = contract.Usage{InputTokens: 5, OutputTokens: 13, TotalTokens: 18, Estimated: true}
	stream, err := streamEngine(t, testDocument(), fault, store).Stream(context.Background(), contract.Principal{}, request)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := stream.Next(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := stream.Next(context.Background()); err != nil {
		t.Fatal(err)
	}
	completion, ok := store.Completion(request.ID)
	if !ok || completion.Usage.TotalTokens != 18 || !completion.Usage.Estimated || len(completion.Attempts) != 1 || completion.Attempts[0].Usage != completion.Usage {
		t.Fatalf("completion = %+v, present=%v", completion, ok)
	}
}

func TestStreamSkipsBoundedPreambleUntilSemanticOutput(t *testing.T) {
	fault := testkit.NewFaultAdapter(
		streamCapabilities(),
		testkit.AdapterStep{Events: []contract.StreamEvent{
			{Type: "usage", Usage: &contract.Usage{InputTokens: 2, TotalTokens: 2}},
			{Type: "content_delta", Chat: &contract.ChatDelta{Content: "hello"}},
			{Type: "finish", Usage: &contract.Usage{InputTokens: 2, OutputTokens: 1, TotalTokens: 3}, Terminal: true},
		}},
	)
	store := testkit.NewMeteringRecorder()
	stream, err := streamEngine(t, testDocument(), fault, store).Stream(context.Background(), contract.Principal{}, chatRequest("preamble", true))
	if err != nil {
		t.Fatal(err)
	}
	first, err := stream.Next(context.Background())
	if err != nil || first.Chat == nil || first.Chat.Content != "hello" || !first.OutputCommitted {
		t.Fatalf("first client event=%+v err=%v", first, err)
	}
	if _, err := stream.Next(context.Background()); err != nil {
		t.Fatal(err)
	}
	completion, ok := store.Completion("preamble")
	if !ok || completion.Usage.TotalTokens != 3 {
		t.Fatalf("completion=%+v present=%v", completion, ok)
	}
}

func TestStreamRejectsTerminalPreambleWithoutCommit(t *testing.T) {
	fault := testkit.NewFaultAdapter(streamCapabilities(), testkit.AdapterStep{Events: []contract.StreamEvent{
		{Type: "usage", Usage: &contract.Usage{TotalTokens: 1}, Terminal: true},
	}})
	store := testkit.NewMeteringRecorder()
	_, err := streamEngine(t, testDocument(), fault, store).Stream(context.Background(), contract.Principal{}, chatRequest("terminal-preamble", true))
	var public *contract.Error
	if !errors.As(err, &public) || public.Code != contract.ErrorProvider || store.State("terminal-preamble") != "finalized" {
		t.Fatalf("error=%v state=%q", err, store.State("terminal-preamble"))
	}
	completion, ok := store.Completion("terminal-preamble")
	if !ok || len(completion.Attempts) != 1 || completion.Attempts[0].OutputCommitted {
		t.Fatalf("completion=%+v present=%v", completion, ok)
	}
}
