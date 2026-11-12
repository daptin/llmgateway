package llmgateway_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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

type memoryResponseStore struct {
	authErr error
	records map[contract.ID][]byte
	owners  map[contract.ID]contract.ID
	saves   int
	err     error
	saveErr error
}

func (s *memoryResponseStore) Authorize(context.Context, contract.Principal, string) error {
	return s.authErr
}

func (s *memoryResponseStore) Begin(_ context.Context, p contract.Principal, _ contract.ReservationToken, _ string) (contract.ID, error) {
	if s.err != nil {
		return "", s.err
	}
	if s.records == nil {
		s.records = map[contract.ID][]byte{}
		s.owners = map[contract.ID]contract.ID{}
	}
	id := contract.ID(fmt.Sprintf("stored-%d", len(s.records)+1))
	s.records[id] = nil
	s.owners[id] = p.OwnerID
	return id, nil
}
func (s *memoryResponseStore) Save(_ context.Context, _ contract.Principal, r llmgateway.StoredResponse) error {
	s.saves++
	if s.saveErr != nil {
		err := s.saveErr
		s.saveErr = nil
		return err
	}
	if s.err != nil {
		return s.err
	}
	b, e := json.Marshal(r)
	s.records[r.ID] = b
	return e
}
func (s *memoryResponseStore) Load(_ context.Context, p contract.Principal, id contract.ID) (llmgateway.StoredResponse, error) {
	if s.err != nil {
		return llmgateway.StoredResponse{}, s.err
	}
	b, ok := s.records[id]
	if !ok || s.owners[id] != p.OwnerID {
		return llmgateway.StoredResponse{}, &contract.Error{Code: contract.ErrorPermission, Message: "response not found", HTTPStatus: 404}
	}
	var r llmgateway.StoredResponse
	e := json.Unmarshal(b, &r)
	return r, e
}
func (s *memoryResponseStore) Delete(_ context.Context, p contract.Principal, id contract.ID) error {
	if s.owners[id] != p.OwnerID {
		return errors.New("denied")
	}
	delete(s.records, id)
	return nil
}

func newResponseEngine(t *testing.T, store llmgateway.ResponseStore, source *testkit.CatalogSource, fault *testkit.FaultAdapter, meter *testkit.MeteringRecorder) *llmgateway.Engine {
	t.Helper()
	registry := adapter.NewRegistry()
	if err := registry.Register("test", adapter.FactoryFunc(func(context.Context, catalog.Provider, adapter.Secret) (adapter.Adapter, error) { return fault, nil })); err != nil {
		t.Fatal(err)
	}
	clock := testkit.NewAutoClock(time.Now())
	e, err := llmgateway.New(llmgateway.Dependencies{Catalog: source, Adapters: registry, Authorizer: testkit.AllowAuthorizer{}, Metering: meter, Counters: testkit.NewCounterStore(clock.Now), Cache: llmgateway.DisabledResponseCache{}, Guardrails: guardrail.NewRegistry(), Telemetry: llmgateway.DiscardTelemetrySink{}, Selector: testkit.NewSelector(0), Clock: clock, Responses: store}, llmgateway.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err = e.Reload(context.Background()); err != nil {
		t.Fatal(err)
	}
	return e
}
func responseRequest(id, text string) contract.Request {
	return contract.Request{ID: contract.ID(id), PublicModel: "model", Operation: contract.OperationResponses, MaxOutputTokens: 10, Responses: &contract.ResponsesRequest{Store: true, Input: []contract.ResponseInputItem{{Type: "message", Role: "user", Content: []contract.ContentPart{{Type: "input_text", Text: text}}}}}}
}
func responseDocument() catalog.Document {
	d := testDocument()
	d.Models[0].Operations = []contract.Operation{contract.OperationResponses}
	d.Models[0].Capabilities = map[string]bool{"tools": true, "reasoning": true}
	d.Deployments[0].Operations = d.Models[0].Operations
	return d
}
func responseResult() contract.Response {
	return contract.Response{Responses: &contract.ResponsesResponse{ID: "provider-id", Status: "completed", Output: []contract.ResponseOutputItem{
		{ID: "reason-1", Type: "reasoning", EncryptedContent: "opaque", Summary: []contract.ContentPart{{Type: "summary_text", Text: "reason"}}},
		{ID: "call-1", Type: "function_call", CallID: "call-1", Name: "lookup", Arguments: `{"key":"value"}`, Status: "completed"},
		{ID: "search-1", Type: "web_search_call", Status: "completed", Action: json.RawMessage(`{"type":"search","query":"blue"}`)},
		{ID: "msg-1", Type: "message", Role: "assistant", Status: "completed", Content: []contract.ContentPart{{Type: "output_text", Text: "blue", Annotations: json.RawMessage(`[]`)}}},
	}}, Usage: contract.Usage{InputTokens: 3, OutputTokens: 2, TotalTokens: 5}}
}

func TestStoredResponsesReplayAcrossProvidersAndMeterFullHistory(t *testing.T) {
	ctx := context.Background()
	owner := contract.Principal{OwnerID: "owner"}
	store := &memoryResponseStore{}
	meter := testkit.NewMeteringRecorder()
	source := testkit.NewCatalogSource(responseDocument())
	fault := testkit.NewFaultAdapter(adapter.Capabilities{Operations: map[contract.Operation]bool{contract.OperationResponses: true}, Features: map[string]bool{"tools": true, "reasoning": true}}, testkit.AdapterStep{Response: responseResult()}, testkit.AdapterStep{Response: responseResult()})
	e := newResponseEngine(t, store, source, fault, meter)
	first, err := e.Invoke(ctx, owner, responseRequest("first", "My color is blue"))
	if err != nil {
		t.Fatal(err)
	}
	if first.Responses.ID != "stored-1" || !first.Responses.Store {
		t.Fatalf("stored result: %#v", first.Responses)
	}
	// Backend catalog replacement moves the next turn to a different provider.
	d := responseDocument()
	d.Revision = 2
	d.Providers[0].ID = "another-provider"
	d.Deployments[0].ProviderID = "another-provider"
	d.Deployments[0].ID = "another-deployment"
	source.Set(d)
	if err = e.Reload(ctx); err != nil {
		t.Fatal(err)
	}
	next := responseRequest("next", "What color?")
	next.Responses.PreviousResponseID = first.Responses.ID
	result, err := e.Invoke(ctx, owner, next)
	if err != nil {
		t.Fatal(err)
	}
	got := fault.Requests()[1].Responses.Input
	if fault.Requests()[1].Responses.Store || fault.Requests()[1].Responses.PreviousResponseID != "" {
		t.Fatal("provider received host-owned state controls")
	}
	if len(got) != 6 || got[0].Content[0].Text != "My color is blue" || got[1].EncryptedContent != "opaque" || got[2].CallID != "call-1" || got[3].Type != "web_search_call" || got[4].Content[0].Text != "blue" || got[5].Content[0].Text != "What color?" {
		t.Fatalf("replay lost history: %#v", got)
	}
	if fault.Attempts()[1] != "another-deployment" || result.Responses.PreviousResponseID != "stored-1" {
		t.Fatalf("continuation: %#v", result)
	}
	if len(next.Responses.Input) != 1 {
		t.Fatal("caller input mutated")
	}
	admissions := meter.Admissions()
	if len(admissions) != 2 || admissions[1].EstimatedUsage.InputTokens < 100 || len(admissions[1].Request.Responses.Input) != 6 {
		t.Fatalf("history not reserved: %#v", admissions)
	}
	completion, ok := meter.Completion("next")
	if !ok || completion.Response == nil || completion.Response.Responses.ID != result.Responses.ID {
		t.Fatalf("canonical result missing from completion: %#v", completion)
	}
	other := contract.Principal{OwnerID: "other"}
	denied := responseRequest("denied", "steal")
	denied.Responses.PreviousResponseID = "stored-1"
	if _, err = e.Invoke(ctx, other, denied); err == nil || len(fault.Attempts()) != 2 || len(meter.Admissions()) != 2 {
		t.Fatal("unauthorized history reached admission or provider")
	}
}

func TestStoredResponsesFailClosedOnStorageAndBounds(t *testing.T) {
	for _, test := range []struct {
		name     string
		store    *memoryResponseStore
		previous string
		status   int
	}{
		{name: "unavailable", store: &memoryResponseStore{err: errors.New("database unavailable")}, status: 503},
		{name: "missing", store: &memoryResponseStore{}, previous: "missing", status: 404},
	} {
		t.Run(test.name, func(t *testing.T) {
			meter := testkit.NewMeteringRecorder()
			fault := testkit.NewFaultAdapter(adapter.Capabilities{Operations: map[contract.Operation]bool{contract.OperationResponses: true}})
			e := newResponseEngine(t, test.store, testkit.NewCatalogSource(responseDocument()), fault, meter)
			r := responseRequest(test.name, "hello")
			r.Responses.PreviousResponseID = test.previous
			_, err := e.Invoke(context.Background(), contract.Principal{OwnerID: "owner"}, r)
			var typed *contract.Error
			if !errors.As(err, &typed) || typed.HTTPStatus != test.status || len(fault.Attempts()) != 0 {
				t.Fatalf("error=%v provider calls=%d", err, len(fault.Attempts()))
			}
		})
	}
	store := &memoryResponseStore{}
	owner := contract.Principal{OwnerID: "owner"}
	id, _ := store.Begin(context.Background(), owner, contract.ReservationToken{}, "")
	stored := llmgateway.StoredResponse{ID: id, Input: []contract.ResponseInputItem{{Type: "message", Role: "user", Content: []contract.ContentPart{{Type: "input_text", Text: strings.Repeat("x", llmgateway.MaxResponseHistoryBytes)}}}}, Response: responseResult()}
	store.Save(context.Background(), owner, stored)
	fault := testkit.NewFaultAdapter(adapter.Capabilities{Operations: map[contract.Operation]bool{contract.OperationResponses: true}})
	meter := testkit.NewMeteringRecorder()
	e := newResponseEngine(t, store, testkit.NewCatalogSource(responseDocument()), fault, meter)
	r := responseRequest("oversize", "hello")
	r.Responses.PreviousResponseID = string(id)
	_, err := e.Invoke(context.Background(), owner, r)
	var typed *contract.Error
	if !errors.As(err, &typed) || typed.HTTPStatus != 413 || len(meter.Admissions()) != 0 {
		t.Fatalf("oversize history: %v", err)
	}
}

func TestStoredResponseStreamingIDsAndTerminalPersistence(t *testing.T) {
	for _, firstTerminal := range []bool{false, true} {
		t.Run(fmt.Sprintf("first-terminal=%t", firstTerminal), func(t *testing.T) {
			result := responseResult()
			snapshot := result
			events := []contract.StreamEvent{{Type: "response.completed", Terminal: true, Usage: &result.Usage, Response: &contract.ResponseDelta{ResponseID: "provider-id", Sequence: 2, Snapshot: &snapshot}}}
			if !firstTerminal {
				created := contract.Response{Responses: &contract.ResponsesResponse{ID: "provider-id", Status: "in_progress"}}
				events = append([]contract.StreamEvent{{Type: "response.created", Response: &contract.ResponseDelta{ResponseID: "provider-id", Sequence: 1, Snapshot: &created}}}, events...)
			}
			store := &memoryResponseStore{}
			owner := contract.Principal{OwnerID: "owner"}
			meter := testkit.NewMeteringRecorder()
			fault := testkit.NewFaultAdapter(adapter.Capabilities{Operations: map[contract.Operation]bool{contract.OperationResponses: true}, Features: map[string]bool{"streaming": true}}, testkit.AdapterStep{Events: events})
			e := newResponseEngine(t, store, testkit.NewCatalogSource(responseDocument()), fault, meter)
			request := responseRequest("stream", "hello")
			request.Stream = true
			stream, err := e.Stream(context.Background(), owner, request)
			if err != nil {
				t.Fatal(err)
			}
			for range events {
				event, err := stream.Next(context.Background())
				if err != nil {
					t.Fatal(err)
				}
				if event.Response.ResponseID != "stored-1" || event.Response.Snapshot.Responses.ID != "stored-1" {
					t.Fatalf("upstream ID escaped: %#v", event.Response)
				}
			}
			if err = stream.Close(context.Background()); err != nil {
				t.Fatal(err)
			}
			completion, ok := meter.Completion("stream")
			if !ok || completion.Response == nil || completion.Response.Responses.ID != "stored-1" {
				t.Fatalf("canonical stream snapshot missing from completion: %#v", completion)
			}
			stored, err := store.Load(context.Background(), owner, "stored-1")
			if err != nil || stored.Response.Responses.Status != "completed" || store.saves != 1 {
				t.Fatalf("terminal persistence: %#v saves=%d error=%v", stored, store.saves, err)
			}
		})
	}
}

func TestStoredResponseStorageFailureStillMetersProviderUsage(t *testing.T) {
	store := &memoryResponseStore{saveErr: errors.New("storage unavailable")}
	meter := testkit.NewMeteringRecorder()
	fault := testkit.NewFaultAdapter(adapter.Capabilities{Operations: map[contract.Operation]bool{contract.OperationResponses: true}}, testkit.AdapterStep{Response: responseResult()})
	e := newResponseEngine(t, store, testkit.NewCatalogSource(responseDocument()), fault, meter)
	_, err := e.Invoke(context.Background(), contract.Principal{OwnerID: "owner"}, responseRequest("save-failed", "hello"))
	var typed *contract.Error
	if !errors.As(err, &typed) || typed.HTTPStatus != 503 {
		t.Fatalf("save error: %v", err)
	}
	completion, ok := meter.Completion("save-failed")
	if !ok || completion.Status != "failed" || completion.HTTPStatus != 503 || completion.Usage.TotalTokens != 5 {
		t.Fatalf("provider usage not settled after storage failure: %#v", completion)
	}
	record, err := store.Load(context.Background(), contract.Principal{OwnerID: "owner"}, "stored-1")
	if err != nil || record.Response.Responses.Status != "failed" {
		t.Fatalf("failed stored state: %#v %v", record, err)
	}
}

func TestCancelledStoredResponseCannotBeContinued(t *testing.T) {
	store := &memoryResponseStore{}
	owner := contract.Principal{OwnerID: "owner"}
	meter := testkit.NewMeteringRecorder()
	snapshot := contract.Response{Responses: &contract.ResponsesResponse{ID: "provider-id", Status: "in_progress"}}
	fault := testkit.NewFaultAdapter(adapter.Capabilities{Operations: map[contract.Operation]bool{contract.OperationResponses: true}, Features: map[string]bool{"streaming": true}}, testkit.AdapterStep{Events: []contract.StreamEvent{{Type: "response.created", Response: &contract.ResponseDelta{ResponseID: "provider-id", Snapshot: &snapshot}}}})
	e := newResponseEngine(t, store, testkit.NewCatalogSource(responseDocument()), fault, meter)
	r := responseRequest("cancelled", "hello")
	r.Stream = true
	stream, err := e.Stream(context.Background(), owner, r)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = stream.Next(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err = stream.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	stored, err := store.Load(context.Background(), owner, "stored-1")
	if err != nil || stored.Response.Responses.Status != "cancelled" {
		t.Fatalf("cancelled state: %#v %v", stored, err)
	}
	next := responseRequest("after-cancel", "next")
	next.Responses.PreviousResponseID = "stored-1"
	_, err = e.Invoke(context.Background(), owner, next)
	var typed *contract.Error
	if !errors.As(err, &typed) || typed.HTTPStatus != 400 || len(fault.Attempts()) != 1 {
		t.Fatalf("cancelled continuation: %v", err)
	}
}

func TestStoredResponsesRequireHostStore(t *testing.T) {
	meter := testkit.NewMeteringRecorder()
	fault := testkit.NewFaultAdapter(adapter.Capabilities{Operations: map[contract.Operation]bool{contract.OperationResponses: true}})
	e := newResponseEngine(t, nil, testkit.NewCatalogSource(responseDocument()), fault, meter)
	_, err := e.Invoke(context.Background(), contract.Principal{OwnerID: "owner"}, responseRequest("without-store", "hello"))
	var typed *contract.Error
	if !errors.As(err, &typed) || typed.HTTPStatus != 503 || len(fault.Attempts()) != 0 || len(meter.Admissions()) != 0 {
		t.Fatalf("missing host storage: %v", err)
	}
}

func TestStoredResponseRetentionDeniedBeforeAdmission(t *testing.T) {
	meter := testkit.NewMeteringRecorder()
	store := &memoryResponseStore{authErr: &contract.Error{Code: contract.ErrorPermission, Message: "denied", HTTPStatus: 403}}
	fault := testkit.NewFaultAdapter(adapter.Capabilities{Operations: map[contract.Operation]bool{contract.OperationResponses: true}})
	engine := newResponseEngine(t, store, testkit.NewCatalogSource(responseDocument()), fault, meter)
	_, err := engine.Invoke(context.Background(), contract.Principal{OwnerID: "owner"}, responseRequest("denied-retention", "hello"))
	var typed *contract.Error
	if !errors.As(err, &typed) || typed.HTTPStatus != 403 || len(meter.Admissions()) != 0 || len(fault.Attempts()) != 0 {
		t.Fatalf("denied retention reached admission or provider: %v", err)
	}
}
