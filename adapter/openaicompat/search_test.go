package openaicompat

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/daptin/llmgateway/contract"
)

func TestInvokeSearchTranslatesCanonicalRequestAndResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/v1/search" {
			t.Fatalf("path=%s", request.URL.Path)
		}
		var body map[string]any
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		queries := body["query"].([]any)
		if len(queries) != 2 || body["max_results"] != float64(2) || body["max_tokens_per_page"] != float64(256) || body["country"] != "IN" {
			t.Fatalf("search body=%#v", body)
		}
		response.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(response, `{"object":"search","results":[{"title":"One","url":"https://example.com/1","snippet":"First","last_updated":"2026-09-01"}]}`)
	}))
	defer server.Close()
	adapter := buildAdapter(t, server, `{}`, Factory{})
	result, err := adapter.Invoke(context.Background(), deployment(), contract.Request{Operation: contract.OperationSearch, Search: &contract.SearchRequest{
		Queries: []string{"one", "two"}, MaxResults: 2, MaxTokensPerPage: 256, Country: "IN",
	}})
	if err != nil || result.Search == nil || len(result.Search.Results) != 1 || result.Search.Results[0].LastUpdated != "2026-09-01" || result.Usage.Measures["search_results"] != 1 {
		t.Fatalf("search result=%#v err=%v", result, err)
	}
}
