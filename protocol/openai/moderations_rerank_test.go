package openai

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/daptin/llmgateway/contract"
)

func TestModerationProtocolPreservesMultimodalInputAndCategories(t *testing.T) {
	engine := &fakeEngine{snapshot: testSnapshot(t), invokeResult: contract.Response{Model: "allowed", Moderation: &contract.ModerationResponse{
		ID: "modr_1", Results: []contract.ModerationResult{{Flagged: true, Categories: map[string]bool{"violence": true}, CategoryScores: map[string]float64{"violence": 0.9}}},
	}}}
	response := perform(testHandler(t, engine, fakeAuthenticator{}), http.MethodPost, "/v1/moderations",
		`{"model":"allowed","input":[{"type":"text","text":"inspect"},{"type":"image_url","image_url":{"url":"https://example.test/image.png"}}]}`, "key")
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	request := engine.invokeRequest
	if request.Operation != contract.OperationModeration || request.Moderation == nil || len(request.Moderation.Input) != 2 ||
		request.Moderation.Input[1].ImageURL == nil || request.Moderation.Input[1].ImageURL.URL != "https://example.test/image.png" {
		t.Fatalf("canonical request=%#v", request)
	}
	assertJSONEqual(t, response.Body.String(), `{"id":"modr_1","model":"allowed","results":[{"flagged":true,"categories":{"violence":true},"category_scores":{"violence":0.9}}]}`)
}

func TestRerankProtocolPreservesDocumentObjectsAndMetadata(t *testing.T) {
	for _, path := range []string{"/v1/rerank", "/v2/rerank", "/rerank"} {
		engine := &fakeEngine{snapshot: testSnapshot(t), invokeResult: contract.Response{Rerank: &contract.RerankResponse{
			ID: "rerank_1", Results: []contract.RerankResult{{Index: 1, RelevanceScore: 0.95, Document: &contract.RerankDocument{Object: json.RawMessage(`{"title":"two","text":"second"}`)}}},
			Meta: contract.RerankMeta{SearchUnits: 1, InputTokens: 8},
		}}}
		response := perform(testHandler(t, engine, fakeAuthenticator{}), http.MethodPost, path,
			`{"model":"allowed","query":"best","documents":["first",{"title":"two","text":"second"}],"top_n":1,"rank_fields":["title","text"],"return_documents":true,"instruction":"rank precisely"}`, "key")
		if response.Code != http.StatusOK {
			t.Fatalf("path=%s status=%d body=%s", path, response.Code, response.Body.String())
		}
		request := engine.invokeRequest
		if request.Operation != contract.OperationRerank || request.Rerank == nil || len(request.Rerank.Documents) != 2 ||
			len(request.Rerank.Documents[1].Object) == 0 || request.Rerank.TopN != 1 || request.Rerank.ReturnDocuments == nil || !*request.Rerank.ReturnDocuments {
			t.Fatalf("path=%s canonical request=%#v", path, request)
		}
		assertJSONEqual(t, response.Body.String(), `{"id":"rerank_1","results":[{"index":1,"relevance_score":0.95,"document":{"title":"two","text":"second"}}],"meta":{"billed_units":{"search_units":1},"tokens":{"input_tokens":8,"output_tokens":0}}}`)
	}
}

func TestModerationAndRerankRejectMalformedInput(t *testing.T) {
	for _, test := range []struct{ path, body string }{
		{path: "/v1/moderations", body: `{"model":"allowed","input":[]}`},
		{path: "/v1/moderations", body: `{"model":"allowed","input":[{"type":"image_url","image_url":{}}]}`},
		{path: "/v1/rerank", body: `{"model":"allowed","query":"q","documents":[]}`},
		{path: "/v1/rerank", body: `{"model":"allowed","query":"q","documents":["one"],"top_n":2}`},
		{path: "/v1/rerank", body: `{"model":"allowed","query":"q","documents":[{"field":1,"field":2}]}`},
	} {
		response := perform(testHandler(t, &fakeEngine{snapshot: testSnapshot(t)}, fakeAuthenticator{}), http.MethodPost, test.path, test.body, "key")
		if response.Code != http.StatusBadRequest {
			t.Fatalf("invalid request accepted: path=%s status=%d body=%s", test.path, response.Code, response.Body.String())
		}
	}
}
