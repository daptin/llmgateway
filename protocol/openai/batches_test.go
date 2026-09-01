package openai

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/daptin/llmgateway/contract"
)

type fakeBatchStore struct {
	request   contract.CreateBatchRequest
	batch     contract.Batch
	cancelled contract.ID
	err       error
}

func (store *fakeBatchStore) Create(_ context.Context, _ contract.Principal, request contract.CreateBatchRequest) (contract.Batch, error) {
	store.request = request
	return store.batch, store.err
}

func (store *fakeBatchStore) List(context.Context, contract.Principal, contract.ListBatchesRequest) (contract.BatchPage, error) {
	return contract.BatchPage{Data: []contract.Batch{store.batch}}, store.err
}

func (store *fakeBatchStore) Get(context.Context, contract.Principal, contract.ID) (contract.Batch, error) {
	return store.batch, store.err
}

func (store *fakeBatchStore) Cancel(_ context.Context, _ contract.Principal, id contract.ID) (contract.Batch, error) {
	store.cancelled = id
	return store.batch, store.err
}

func TestBatchesLifecycle(t *testing.T) {
	store := &fakeBatchStore{batch: contract.Batch{ID: "batch-1", Endpoint: "/v1/chat/completions", InputFileID: "file-1",
		CompletionWindow: "24h", Status: contract.BatchStatusValidating, CreatedAt: time.Unix(1700000000, 0).UTC(), Metadata: map[string]string{"job": "test"}}}
	handler, err := NewHandler(&fakeEngine{}, fakeAuthenticator{}, Options{Batches: store,
		NewRequestID: func() (contract.ID, error) { return "req_batch", nil }})
	if err != nil {
		t.Fatal(err)
	}
	response := perform(handler, http.MethodPost, "/v1/batches",
		`{"input_file_id":"file-1","endpoint":"/v1/chat/completions","completion_window":"24h","metadata":{"job":"test"}}`, "key")
	if response.Code != http.StatusOK || store.request.InputFileID != "file-1" || store.request.Metadata["job"] != "test" {
		t.Fatalf("create = %d request=%#v body=%s", response.Code, store.request, response.Body.String())
	}
	for _, request := range []struct{ method, path string }{
		{http.MethodGet, "/v1/batches"},
		{http.MethodGet, "/v1/batches/batch-1"},
		{http.MethodPost, "/v1/batches/batch-1/cancel"},
	} {
		response = perform(handler, request.method, request.path, "", "key")
		if response.Code != http.StatusOK {
			t.Fatalf("%s %s = %d body=%s", request.method, request.path, response.Code, response.Body.String())
		}
	}
	if store.cancelled != "batch-1" {
		t.Fatalf("cancelled %q", store.cancelled)
	}
}

func TestBatchesRejectUnsupportedContract(t *testing.T) {
	store := &fakeBatchStore{}
	handler, err := NewHandler(&fakeEngine{}, fakeAuthenticator{}, Options{Batches: store,
		NewRequestID: func() (contract.ID, error) { return "req_batch", nil }})
	if err != nil {
		t.Fatal(err)
	}
	for _, body := range []string{
		`{"input_file_id":"file-1","endpoint":"/v1/unknown","completion_window":"24h"}`,
		`{"input_file_id":"file-1","endpoint":"/v1/chat/completions","completion_window":"1h"}`,
		`{"input_file_id":"file-1","endpoint":"/v1/chat/completions","completion_window":"24h","unknown":true}`,
	} {
		response := perform(handler, http.MethodPost, "/v1/batches", body, "key")
		if response.Code != http.StatusBadRequest {
			t.Fatalf("accepted invalid batch request: %d %s", response.Code, response.Body.String())
		}
	}
	if store.request.InputFileID != "" {
		t.Fatalf("invalid batch reached storage: %#v", store.request)
	}
}
