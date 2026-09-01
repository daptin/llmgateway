package openai

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/daptin/llmgateway/contract"
)

type fakeFileStore struct {
	created contract.CreateFileRequest
	files   []contract.File
	content contract.FileContent
	deleted contract.ID
	err     error
}

func fileHandler(t *testing.T, store FileStore) http.Handler {
	t.Helper()
	handler, err := NewHandler(&fakeEngine{}, fakeAuthenticator{}, Options{
		Files: store, NewRequestID: func() (contract.ID, error) { return "req_files", nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	return handler
}

func (store *fakeFileStore) Create(_ context.Context, _ contract.Principal, request contract.CreateFileRequest) (contract.File, error) {
	store.created = request
	if store.err != nil {
		return contract.File{}, store.err
	}
	return store.files[0], nil
}

func (store *fakeFileStore) List(_ context.Context, _ contract.Principal, _ contract.ListFilesRequest) (contract.FilePage, error) {
	return contract.FilePage{Data: store.files}, store.err
}

func (store *fakeFileStore) Get(_ context.Context, _ contract.Principal, id contract.ID) (contract.File, error) {
	if store.err != nil {
		return contract.File{}, store.err
	}
	for _, file := range store.files {
		if file.ID == id {
			return file, nil
		}
	}
	return contract.File{}, errors.New("missing")
}

func (store *fakeFileStore) Content(_ context.Context, _ contract.Principal, _ contract.ID) (contract.FileContent, error) {
	return store.content, store.err
}

func (store *fakeFileStore) Delete(_ context.Context, _ contract.Principal, id contract.ID) error {
	store.deleted = id
	return store.err
}

func TestFilesLifecycle(t *testing.T) {
	now := time.Unix(1700000000, 0).UTC()
	input := []byte(`{"custom_id":"one","method":"POST","url":"/v1/chat/completions","body":{"model":"allowed","messages":[{"role":"user","content":"hello"}]}}` + "\n")
	store := &fakeFileStore{files: []contract.File{{ID: "file-1", Bytes: 3, CreatedAt: now, Filename: "batch.jsonl", Purpose: contract.FilePurposeBatch}},
		content: contract.FileContent{Filename: "batch.jsonl", ContentType: "application/jsonl", Data: []byte("abc")}}
	handler := fileHandler(t, store)

	create := multipartRequest(t, "/v1/files", map[string][]string{"purpose": {"batch"}}, "batch.jsonl", input)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, create)
	if response.Code != http.StatusOK || store.created.Filename != "batch.jsonl" || store.created.Purpose != contract.FilePurposeBatch || string(store.created.Data) != string(input) {
		t.Fatalf("create = %d %#v body=%s", response.Code, store.created, response.Body.String())
	}

	for _, test := range []struct {
		method, path string
		status       int
	}{
		{http.MethodGet, "/v1/files", http.StatusOK},
		{http.MethodGet, "/v1/files/file-1", http.StatusOK},
		{http.MethodGet, "/v1/files/file-1/content", http.StatusOK},
		{http.MethodDelete, "/v1/files/file-1", http.StatusOK},
	} {
		request := httptest.NewRequest(test.method, test.path, nil)
		request.Header.Set("Authorization", "Bearer key")
		response = httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != test.status {
			t.Fatalf("%s %s = %d body=%s", test.method, test.path, response.Code, response.Body.String())
		}
	}
	if store.deleted != "file-1" {
		t.Fatalf("deleted %q", store.deleted)
	}
}

func TestBatchFilesAreValidatedBeforeStorage(t *testing.T) {
	store := &fakeFileStore{files: []contract.File{{ID: "file-1", Bytes: 1, CreatedAt: time.Now(), Filename: "batch.jsonl", Purpose: contract.FilePurposeBatch}}}
	handler := fileHandler(t, store)
	for _, test := range []struct {
		filename string
		data     string
	}{
		{filename: "batch.txt", data: `{"custom_id":"one","method":"POST","url":"/v1/chat/completions","body":{}}`},
		{filename: "batch.jsonl", data: `{"custom_id":"one","method":"GET","url":"/v1/chat/completions","body":{}}`},
		{filename: "batch.jsonl", data: `{"custom_id":"one","method":"POST","url":"/v1/chat/completions","body":{"stream":true}}`},
	} {
		request := multipartRequest(t, "/v1/files", map[string][]string{"purpose": {"batch"}}, test.filename, []byte(test.data))
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusBadRequest {
			t.Fatalf("%s accepted invalid batch file: %d %s", test.filename, response.Code, response.Body.String())
		}
	}
	if store.created.Filename != "" {
		t.Fatalf("invalid batch reached storage: %#v", store.created)
	}
}

func TestFilesRejectInvalidInputsAndNormalizeStoreErrors(t *testing.T) {
	store := &fakeFileStore{files: []contract.File{{ID: "file-1", Bytes: 1, CreatedAt: time.Now(), Filename: "a", Purpose: contract.FilePurposeBatch}}}
	handler := fileHandler(t, store)

	request := multipartRequest(t, "/v1/files", map[string][]string{"purpose": {"invalid"}}, "a", []byte("x"))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("invalid purpose = %d", response.Code)
	}

	request = httptest.NewRequest(http.MethodGet, "/v1/files?limit=0", nil)
	request.Header.Set("Authorization", "Bearer key")
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("invalid limit = %d", response.Code)
	}

	store.err = &contract.Error{Code: contract.ErrorPermission, Message: "file not found", HTTPStatus: http.StatusNotFound}
	request = httptest.NewRequest(http.MethodGet, "/v1/files/file-1", nil)
	request.Header.Set("Authorization", "Bearer key")
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	var body map[string]map[string]any
	if response.Code != http.StatusNotFound || json.Unmarshal(response.Body.Bytes(), &body) != nil || body["error"]["message"] != "file not found" {
		t.Fatalf("store error = %d %s", response.Code, response.Body.String())
	}
}
