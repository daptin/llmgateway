package openai

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/daptin/llmgateway/contract"
)

func TestEmbeddingInputForms(t *testing.T) {
	tests := []struct {
		name       string
		input      string
		texts      int
		tokenLists int
	}{
		{name: "string", input: `"hello"`, texts: 1},
		{name: "strings", input: `["hello","world"]`, texts: 2},
		{name: "tokens", input: `[1,2,3]`, tokenLists: 1},
		{name: "token lists", input: `[[1,2],[3,4]]`, tokenLists: 2},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			input, err := decodeEmbeddingInput(json.RawMessage(test.input))
			if err != nil {
				t.Fatal(err)
			}
			if len(input.Texts) != test.texts || len(input.Tokens) != test.tokenLists {
				t.Fatalf("decoded input = %#v", input)
			}
		})
	}
	for _, invalid := range []string{`[]`, `["text",1]`, `[-1]`, `[[1],[]]`, `{}`} {
		if _, err := decodeEmbeddingInput(json.RawMessage(invalid)); err == nil {
			t.Fatalf("accepted invalid input %s", invalid)
		}
	}
}

func TestEmbeddingsProtocol(t *testing.T) {
	engine := &fakeEngine{snapshot: testSnapshot(t), invokeResult: contract.Response{
		Model: "allowed", Embeddings: &contract.EmbeddingsResponse{Data: []contract.Embedding{{Index: 0, Base64: "AAAA"}}},
		Usage: contract.Usage{InputTokens: 3, TotalTokens: 3},
	}}
	handler := testHandler(t, engine, fakeAuthenticator{})
	response := perform(handler, http.MethodPost, "/v1/embeddings", `{"model":"allowed","input":[[1,2,3]],"dimensions":2,"encoding_format":"base64"}`, "key")
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	if engine.invokeRequest.Embeddings == nil || engine.invokeRequest.Embeddings.Dimensions != 2 || len(engine.invokeRequest.Embeddings.Input.Tokens) != 1 {
		t.Fatalf("canonical request = %#v", engine.invokeRequest)
	}
	want := `{"data":[{"embedding":"AAAA","index":0,"object":"embedding"}],"model":"allowed","object":"list","usage":{"prompt_tokens":3,"completion_tokens":0,"total_tokens":3}}`
	assertJSONEqual(t, response.Body.String(), want)
}

func TestImageGenerationProtocol(t *testing.T) {
	engine := &fakeEngine{snapshot: testSnapshot(t), invokeResult: contract.Response{
		ImageGeneration: &contract.ImageGenerationResponse{Created: 100, Data: []contract.GeneratedImage{{Base64: "image", RevisedPrompt: "clean prompt"}}},
	}}
	handler := testHandler(t, engine, fakeAuthenticator{})
	response := perform(handler, http.MethodPost, "/v1/images/generations", `{"model":"allowed","prompt":"a cat","n":1,"size":"1024x1024","quality":"hd","response_format":"b64_json"}`, "key")
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	if engine.invokeRequest.ImageGeneration == nil || engine.invokeRequest.ImageGeneration.ResponseFormat != "b64_json" {
		t.Fatalf("canonical request = %#v", engine.invokeRequest)
	}
	assertJSONEqual(t, response.Body.String(), `{"created":100,"data":[{"b64_json":"image","revised_prompt":"clean prompt"}]}`)
}

func TestResponsesRejectStateAndPreserveTypedInput(t *testing.T) {
	engine := &fakeEngine{snapshot: testSnapshot(t), invokeResult: contract.Response{
		Model: "allowed", Responses: &contract.ResponsesResponse{ID: "resp_1", Status: "completed", Output: []contract.ResponseOutputItem{{
			Type: "message", ID: "msg_1", Role: "assistant", Status: "completed", Content: []contract.ContentPart{{Type: "output_text", Text: "sunny"}},
		}}}, Usage: contract.Usage{InputTokens: 5, OutputTokens: 1, TotalTokens: 6},
	}}
	handler := testHandler(t, engine, fakeAuthenticator{})
	stateful := perform(handler, http.MethodPost, "/v1/responses", `{"model":"allowed","input":"hello","store":true}`, "key")
	if stateful.Code != http.StatusBadRequest || !strings.Contains(stateful.Body.String(), "stateful responses") {
		t.Fatalf("stateful request was not explicitly rejected: %d %s", stateful.Code, stateful.Body.String())
	}
	body := `{"model":"allowed","instructions":"be concise","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"weather"},{"type":"input_image","image_url":"https://example.test/sky.png","detail":"low"}]},{"type":"function_call_output","call_id":"call_1","output":"sunny"}],"tools":[{"type":"function","function":{"name":"weather","parameters":{"type":"object"}}}],"text":{"format":{"type":"json_schema","name":"forecast","schema":{"type":"object"}}},"max_output_tokens":20}`
	response := perform(handler, http.MethodPost, "/v1/responses", body, "key")
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	request := engine.invokeRequest
	if request.Responses == nil || len(request.Responses.Input) != 2 || request.Responses.TextFormat.JSONSchema.Name != "forecast" || request.MaxOutputTokens != 20 {
		t.Fatalf("canonical request = %#v", request)
	}
	assertJSONEqual(t, response.Body.String(), `{"id":"resp_1","model":"allowed","object":"response","output":[{"content":[{"annotations":[],"text":"sunny","type":"output_text"}],"id":"msg_1","role":"assistant","status":"completed","type":"message"}],"status":"completed","usage":{"input_tokens":5,"output_tokens":1,"total_tokens":6}}`)
}

func TestResponsesStreamingUsesNamedSSEEvents(t *testing.T) {
	stream := &eventStream{events: []contract.StreamEvent{
		{Type: "response.output_text.delta", Response: &contract.ResponseDelta{ResponseID: "resp_s", Sequence: 1, Delta: "hi"}},
		{Type: "response.completed", Response: &contract.ResponseDelta{ResponseID: "resp_s", Sequence: 2}, Usage: &contract.Usage{InputTokens: 2, OutputTokens: 1, TotalTokens: 3}, Terminal: true},
	}}
	engine := &fakeEngine{snapshot: testSnapshot(t), stream: stream}
	handler := testHandler(t, engine, fakeAuthenticator{})
	response := perform(handler, http.MethodPost, "/v1/responses", `{"model":"allowed","input":"hello","stream":true}`, "key")
	if response.Code != http.StatusOK || !stream.closed {
		t.Fatalf("status=%d closed=%v body=%s", response.Code, stream.closed, response.Body.String())
	}
	want := "event: response.output_text.delta\ndata: {\"delta\":\"hi\",\"response_id\":\"resp_s\",\"sequence_number\":1,\"type\":\"response.output_text.delta\"}\n\n" +
		"event: response.completed\ndata: {\"response\":{\"id\":\"resp_s\",\"model\":\"allowed\",\"object\":\"response\",\"status\":\"completed\",\"usage\":{\"input_tokens\":2,\"output_tokens\":1,\"total_tokens\":3}},\"response_id\":\"resp_s\",\"sequence_number\":2,\"type\":\"response.completed\"}\n\n"
	if response.Body.String() != want {
		t.Fatalf("SSE mismatch\n got: %s\nwant: %s", response.Body.String(), want)
	}
}

func assertJSONEqual(t *testing.T, got, want string) {
	t.Helper()
	var gotValue, wantValue any
	if err := json.Unmarshal([]byte(got), &gotValue); err != nil {
		t.Fatalf("decode got: %v: %s", err, got)
	}
	if err := json.Unmarshal([]byte(want), &wantValue); err != nil {
		t.Fatalf("decode want: %v: %s", err, want)
	}
	gotJSON, _ := json.Marshal(gotValue)
	wantJSON, _ := json.Marshal(wantValue)
	if string(gotJSON) != string(wantJSON) {
		t.Fatalf("JSON mismatch\n got: %s\nwant: %s", gotJSON, wantJSON)
	}
}
