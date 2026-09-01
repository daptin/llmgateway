package openai

import (
	"net/http"
	"testing"

	"github.com/daptin/llmgateway/contract"
)

func TestSearchProtocolSupportsBodyAndPathToolSelection(t *testing.T) {
	for _, test := range []struct{ path, body string }{
		{path: "/v1/search", body: `{"search_tool_name":"web","query":"latest","max_results":2,"search_domain_filter":["example.com"],"max_tokens_per_page":256,"country":"IN"}`},
		{path: "/v1/search/web", body: `{"query":["latest","today"],"max_results":2}`},
	} {
		engine := &fakeEngine{snapshot: testSnapshot(t), invokeResult: contract.Response{Search: &contract.SearchResponse{Results: []contract.SearchResult{
			{Title: "Result", URL: "https://example.com/result", Snippet: "Summary", Date: "2026-09-01"},
		}}}}
		response := perform(testHandler(t, engine, fakeAuthenticator{}), http.MethodPost, test.path, test.body, "key")
		if response.Code != http.StatusOK {
			t.Fatalf("path=%s status=%d body=%s", test.path, response.Code, response.Body.String())
		}
		if engine.invokeRequest.Operation != contract.OperationSearch || engine.invokeRequest.PublicModel != "web" || engine.invokeRequest.Search == nil || engine.invokeRequest.Search.MaxResults != 2 {
			t.Fatalf("canonical search request=%#v", engine.invokeRequest)
		}
		usage := engine.invokeRequest.EstimatedUsage
		if usage.TotalTokens != 0 || usage.InputTokens != 0 || usage.Measures["request_bytes"] == 0 || usage.Measures["search_queries"] == 0 || usage.Measures["search_results"] != 2 {
			t.Fatalf("search usage estimate=%#v", usage)
		}
		assertJSONEqual(t, response.Body.String(), `{"object":"search","results":[{"title":"Result","url":"https://example.com/result","snippet":"Summary","date":"2026-09-01"}]}`)
	}
}

func TestSearchProtocolRejectsInvalidRequests(t *testing.T) {
	for _, test := range []struct{ path, body string }{
		{path: "/v1/search", body: `{"query":"missing tool"}`},
		{path: "/v1/search/web", body: `{"search_tool_name":"other","query":"q"}`},
		{path: "/v1/search/web", body: `{"query":[],"max_results":2}`},
		{path: "/v1/search/web", body: `{"query":"q","max_results":21}`},
	} {
		response := perform(testHandler(t, &fakeEngine{snapshot: testSnapshot(t)}, fakeAuthenticator{}), http.MethodPost, test.path, test.body, "key")
		if response.Code != http.StatusBadRequest {
			t.Fatalf("invalid search accepted: path=%s status=%d body=%s", test.path, response.Code, response.Body.String())
		}
	}
}
