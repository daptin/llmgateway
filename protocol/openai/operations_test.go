package openai

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
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
	response := perform(handler, http.MethodPost, "/v1/embeddings", `{"model":"allowed","input":[[1,2,3]],"dimensions":2,"encoding_format":"base64","user":"user-1"}`, "key")
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	if engine.invokeRequest.Embeddings == nil || engine.invokeRequest.Embeddings.Dimensions != 2 || engine.invokeRequest.Embeddings.EncodingFormat != "base64" ||
		engine.invokeRequest.Embeddings.User != "user-1" || len(engine.invokeRequest.Embeddings.Input.Tokens) != 1 {
		t.Fatalf("canonical request = %#v", engine.invokeRequest)
	}
	want := `{"data":[{"embedding":"AAAA","index":0,"object":"embedding"}],"model":"allowed","object":"list","usage":{"prompt_tokens":3,"completion_tokens":0,"total_tokens":3}}`
	assertJSONEqual(t, response.Body.String(), want)
}

func TestImageGenerationProtocol(t *testing.T) {
	engine := &fakeEngine{snapshot: testSnapshot(t), invokeResult: contract.Response{
		Images: &contract.ImageResponse{Created: 100, Data: []contract.GeneratedImage{{Base64: "image", RevisedPrompt: "clean prompt"}}},
	}}
	handler := testHandler(t, engine, fakeAuthenticator{})
	response := perform(handler, http.MethodPost, "/v1/images/generations", `{"model":"allowed","prompt":"a cat","n":1,"size":"1024x1024","quality":"hd","response_format":"b64_json"}`, "key")
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	if engine.invokeRequest.ImageGeneration == nil || engine.invokeRequest.PublicModel != "allowed" || engine.invokeRequest.ImageGeneration.Prompt != "a cat" ||
		engine.invokeRequest.ImageGeneration.N != 1 || engine.invokeRequest.ImageGeneration.Size != "1024x1024" ||
		engine.invokeRequest.ImageGeneration.Quality != "hd" || engine.invokeRequest.ImageGeneration.ResponseFormat != "b64_json" {
		t.Fatalf("canonical request = %#v", engine.invokeRequest)
	}
	if engine.invokeRequest.EstimatedUsage.InputTokens != 5 || engine.invokeRequest.EstimatedUsage.Measures["request_bytes"] == 0 {
		t.Fatalf("image generation usage estimate=%#v", engine.invokeRequest.EstimatedUsage)
	}
	assertJSONEqual(t, response.Body.String(), `{"created":100,"data":[{"b64_json":"image","revised_prompt":"clean prompt"}]}`)
}

func TestImageEditProtocolPreservesFilesAndControls(t *testing.T) {
	engine := &fakeEngine{snapshot: testSnapshot(t), invokeResult: contract.Response{Images: &contract.ImageResponse{
		Created: 101, Data: []contract.GeneratedImage{{Base64: "edited"}},
	}}}
	request := multipartFilesRequest(t, "/v1/images/edits", map[string][]string{
		"model": {"allowed"}, "prompt": {"remove background"}, "n": {"1"}, "response_format": {"b64_json"},
		"quality": {"high"}, "input_fidelity": {"high"}, "output_format": {"png"}, "output_compression": {"80"},
	}, map[string][]contract.MediaFile{
		"image[]": {{Name: "one.png", Data: []byte("one")}, {Name: "two.png", Data: []byte("two")}},
		"mask":    {{Name: "mask.png", Data: []byte("mask")}},
	})
	response := httptest.NewRecorder()
	testHandler(t, engine, fakeAuthenticator{}).ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	edit := engine.invokeRequest.ImageEdit
	if engine.invokeRequest.Operation != contract.OperationImageEdit || edit == nil || len(edit.Images) != 2 || edit.Mask == nil || edit.Mask.Name != "mask.png" ||
		edit.Prompt != "remove background" || edit.N != 1 || edit.OutputCompression == nil || *edit.OutputCompression != 80 || edit.InputFidelity != "high" {
		t.Fatalf("canonical image edit=%#v", engine.invokeRequest)
	}
	if engine.invokeRequest.EstimatedUsage.Measures["image_bytes"] != 10 {
		t.Fatalf("image edit usage estimate=%#v", engine.invokeRequest.EstimatedUsage)
	}
	assertJSONEqual(t, response.Body.String(), `{"created":101,"data":[{"b64_json":"edited"}]}`)
}

func TestImageVariationProtocolPreservesFileAndControls(t *testing.T) {
	engine := &fakeEngine{snapshot: testSnapshot(t), invokeResult: contract.Response{Images: &contract.ImageResponse{
		Created: 102, Data: []contract.GeneratedImage{{Base64: "variation"}},
	}}}
	request := multipartFilesRequest(t, "/v1/images/variations", map[string][]string{
		"model": {"allowed"}, "n": {"1"}, "size": {"512x512"}, "response_format": {"b64_json"}, "user": {"user-1"},
	}, map[string][]contract.MediaFile{"image": {{Name: "source.png", Data: []byte("source")}}})
	response := httptest.NewRecorder()
	testHandler(t, engine, fakeAuthenticator{}).ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	variation := engine.invokeRequest.ImageVariation
	if engine.invokeRequest.Operation != contract.OperationImageVariation || variation == nil || variation.Image.Name != "source.png" ||
		variation.N != 1 || variation.Size != "512x512" || variation.ResponseFormat != "b64_json" || variation.User != "user-1" {
		t.Fatalf("canonical image variation=%#v", engine.invokeRequest)
	}
	if engine.invokeRequest.EstimatedUsage.Measures["image_bytes"] != 6 {
		t.Fatalf("image variation usage estimate=%#v", engine.invokeRequest.EstimatedUsage)
	}
	assertJSONEqual(t, response.Body.String(), `{"created":102,"data":[{"b64_json":"variation"}]}`)
}

func TestImageVariationRejectsInvalidMultipartInput(t *testing.T) {
	for _, test := range []struct {
		fields map[string][]string
		files  map[string][]contract.MediaFile
	}{
		{fields: map[string][]string{"model": {"allowed"}}, files: map[string][]contract.MediaFile{}},
		{fields: map[string][]string{"model": {"allowed"}, "n": {"11"}}, files: map[string][]contract.MediaFile{"image": {{Name: "source.png", Data: []byte("source")}}}},
		{fields: map[string][]string{"model": {"allowed"}, "response_format": {"binary"}}, files: map[string][]contract.MediaFile{"image": {{Name: "source.png", Data: []byte("source")}}}},
		{fields: map[string][]string{"model": {"allowed"}}, files: map[string][]contract.MediaFile{"image": {{Name: "one.png", Data: []byte("one")}, {Name: "two.png", Data: []byte("two")}}}},
	} {
		response := httptest.NewRecorder()
		testHandler(t, &fakeEngine{}, fakeAuthenticator{}).ServeHTTP(response,
			multipartFilesRequest(t, "/v1/images/variations", test.fields, test.files))
		if response.Code != http.StatusBadRequest {
			t.Fatalf("invalid image variation accepted: status=%d body=%s", response.Code, response.Body.String())
		}
	}
}

func TestResponsesRejectStateAndPreserveTypedInput(t *testing.T) {
	engine := &fakeEngine{snapshot: testSnapshot(t), invokeResult: contract.Response{
		Model: "allowed", Responses: &contract.ResponsesResponse{ID: "resp_1", Status: "completed", Output: []contract.ResponseOutputItem{{
			Type: "message", ID: "msg_1", Role: "assistant", Status: "completed", Content: []contract.ContentPart{{Type: "output_text", Text: "sunny"}},
		}}}, Usage: contract.Usage{InputTokens: 5, OutputTokens: 1, CacheReadTokens: 2, ReasoningTokens: 1, TotalTokens: 6},
	}}
	handler := testHandler(t, engine, fakeAuthenticator{})
	for _, body := range []string{
		`{"model":"allowed","input":"hello","store":true}`,
		`{"model":"allowed","input":"hello","background":true}`,
		`{"model":"allowed","input":"hello","previous_response_id":""}`,
	} {
		stateful := perform(handler, http.MethodPost, "/v1/responses", body, "key")
		if stateful.Code != http.StatusBadRequest || !strings.Contains(stateful.Body.String(), "stateful responses") {
			t.Fatalf("stateful request was not explicitly rejected: %d %s", stateful.Code, stateful.Body.String())
		}
	}
	body := `{"model":"allowed","instructions":"be concise","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"weather"},{"type":"input_image","image_url":"https://example.test/sky.png","detail":"low"},{"type":"input_file","file_data":"data:application/pdf;base64,cGRm","filename":"brief.pdf"}]},{"type":"function_call_output","call_id":"call_1","output":"sunny"}],"tools":[{"type":"function","name":"weather","parameters":{"type":"object"}}],"tool_choice":{"type":"function","name":"weather"},"text":{"format":{"type":"json_schema","name":"forecast","schema":{"type":"object"}}},"temperature":0.4,"top_p":0.8,"parallel_tool_calls":false,"reasoning":{"effort":"low","summary":"concise"},"max_output_tokens":20,"store":false,"background":false,"prompt_cache_key":"tenant-thread","safety_identifier":"safe-user","service_tier":"priority","truncation":"disabled","user":"legacy-user","top_logprobs":3}`
	response := perform(handler, http.MethodPost, "/v1/responses", body, "key")
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	request := engine.invokeRequest
	if request.Responses == nil || len(request.Responses.Input) != 2 || request.Responses.TextFormat.JSONSchema.Name != "forecast" || request.MaxOutputTokens != 20 ||
		request.Responses.Temperature == nil || *request.Responses.Temperature != 0.4 || request.Responses.TopP == nil || *request.Responses.TopP != 0.8 ||
		request.Responses.ParallelToolCalls == nil || *request.Responses.ParallelToolCalls || request.Responses.ReasoningEffort != "low" ||
		request.Responses.ReasoningSummary != "concise" || request.Responses.PromptCacheKey != "tenant-thread" || request.Responses.SafetyIdentifier != "safe-user" ||
		request.Responses.ServiceTier != "priority" || request.Responses.Truncation != "disabled" || request.Responses.User != "legacy-user" ||
		request.Responses.TopLogprobs == nil || *request.Responses.TopLogprobs != 3 ||
		request.Responses.Input[0].Content[2].File == nil || request.Responses.Input[0].Content[2].File.Filename != "brief.pdf" ||
		request.Responses.ToolChoice == nil || request.Responses.ToolChoice.FunctionName != "weather" {
		t.Fatalf("canonical request = %#v", request)
	}
	assertJSONEqual(t, response.Body.String(), `{"id":"resp_1","model":"allowed","object":"response","output":[{"content":[{"annotations":[],"text":"sunny","type":"output_text"}],"id":"msg_1","role":"assistant","status":"completed","type":"message"}],"status":"completed","usage":{"input_tokens":5,"input_tokens_details":{"cached_tokens":2},"output_tokens":1,"output_tokens_details":{"reasoning_tokens":1},"total_tokens":6}}`)
}

func TestResponsesAcceptCodex01532Request(t *testing.T) {
	body, err := os.ReadFile("testdata/codex-0.153.2-responses-request.json")
	if err != nil {
		t.Fatal(err)
	}
	stream := &eventStream{events: []contract.StreamEvent{{
		Type: "response.completed",
		Response: &contract.ResponseDelta{ResponseID: "resp_codex", Sequence: 1, Snapshot: &contract.Response{
			Responses: &contract.ResponsesResponse{ID: "resp_codex", Status: "completed", Output: []contract.ResponseOutputItem{}},
		}},
		Terminal: true,
	}}}
	engine := &fakeEngine{snapshot: testSnapshot(t), stream: stream}
	response := perform(testHandler(t, engine, fakeAuthenticator{}), http.MethodPost, "/v1/responses", string(body), "key")
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	request := engine.streamRequest
	if request.Responses == nil || len(request.Responses.Include) != 1 || request.Responses.Include[0] != "reasoning.encrypted_content" ||
		len(request.Responses.Input) != 1 || request.Responses.Input[0].ID != "msg_codex_1" || request.Responses.PromptCacheKey != "codex-thread" {
		t.Fatalf("Codex request was not preserved: %#v", request.Responses)
	}
}

func TestResponsesStrictErrorsIdentifyUnsupportedFields(t *testing.T) {
	handler := testHandler(t, &fakeEngine{snapshot: testSnapshot(t)}, fakeAuthenticator{})
	for _, test := range []struct {
		body  string
		field string
	}{
		{body: `{"model":"allowed","input":"hello","future_option":true}`, field: "future_option"},
		{body: `{"model":"allowed","input":[{"type":"message","role":"user","content":"hello","future_item":true}]}`, field: "future_item"},
	} {
		response := perform(handler, http.MethodPost, "/v1/responses", test.body, "key")
		if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), `unsupported field \"`+test.field+`\"`) {
			t.Fatalf("field-specific error missing: status=%d body=%s", response.Code, response.Body.String())
		}
	}
}

func TestResponsesRejectInvalidSamplingControls(t *testing.T) {
	handler := testHandler(t, &fakeEngine{snapshot: testSnapshot(t)}, fakeAuthenticator{})
	for _, body := range []string{
		`{"model":"allowed","input":"hello","temperature":2.1}`,
		`{"model":"allowed","input":"hello","top_p":-0.1}`,
		`{"model":"allowed","input":"hello","reasoning":{"effort":"maximum"}}`,
		`{"model":"allowed","input":"hello","reasoning":{"effort":"low","summary":"tiny"}}`,
		`{"model":"allowed","input":"hello","service_tier":"slow"}`,
		`{"model":"allowed","input":"hello","truncation":"oldest"}`,
		`{"model":"allowed","input":"hello","top_logprobs":21}`,
		`{"model":"allowed","input":[{"type":"message","role":"user","content":[{"type":"input_file","file_id":"file_provider"}]}]}`,
	} {
		response := perform(handler, http.MethodPost, "/v1/responses", body, "key")
		if response.Code != http.StatusBadRequest {
			t.Fatalf("accepted invalid controls: status=%d body=%s", response.Code, response.Body.String())
		}
	}
}

func TestResponseCompactionProtocolIsStatelessAndTyped(t *testing.T) {
	engine := &fakeEngine{snapshot: testSnapshot(t), invokeResult: contract.Response{Responses: &contract.ResponsesResponse{
		ID: "resp_compact_1", Object: "response.compaction", CreatedAt: 123,
		Output: []contract.ResponseOutputItem{
			{Type: "message", ID: "msg_1", Role: "user", Content: []contract.ContentPart{{Type: "input_text", Text: "hello"}, {Type: "input_image", ImageURL: &contract.ImageURL{URL: "https://example.test/image.png", Detail: "low"}}}},
			{Type: "compaction", ID: "cmp_1", EncryptedContent: "opaque", CreatedBy: "service"},
		},
	}, Usage: contract.Usage{InputTokens: 9, OutputTokens: 2, CacheReadTokens: 3, ReasoningTokens: 1, TotalTokens: 11}}}
	handler := testHandler(t, engine, fakeAuthenticator{})
	body := `{"model":"allowed","input":[{"type":"message","role":"user","content":"hello"},{"type":"compaction","encrypted_content":"previous"}],"instructions":"short","prompt_cache_key":"thread","prompt_cache_options":{"mode":"explicit","ttl":"30m"},"prompt_cache_retention":"24h","service_tier":"priority"}`
	response := perform(handler, http.MethodPost, "/v1/responses/compact", body, "key")
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	request := engine.invokeRequest
	if request.Operation != contract.OperationResponseCompact || request.Responses == nil || len(request.Responses.Input) != 2 ||
		request.Responses.Input[1].EncryptedContent != "previous" || request.Responses.PromptCacheMode != "explicit" ||
		request.Responses.PromptCacheTTL != "30m" || request.Responses.PromptCacheRetention != "24h" || request.Responses.ServiceTier != "priority" {
		t.Fatalf("canonical request = %#v", request)
	}
	assertJSONEqual(t, response.Body.String(), `{"created_at":123,"id":"resp_compact_1","object":"response.compaction","output":[{"content":[{"text":"hello","type":"input_text"},{"detail":"low","image_url":"https://example.test/image.png","type":"input_image"}],"id":"msg_1","role":"user","type":"message"},{"created_by":"service","encrypted_content":"opaque","id":"cmp_1","type":"compaction"}],"usage":{"input_tokens":9,"input_tokens_details":{"cached_tokens":3},"output_tokens":2,"output_tokens_details":{"reasoning_tokens":1},"total_tokens":11}}`)

	stateful := perform(handler, http.MethodPost, "/v1/responses/compact", `{"model":"allowed","input":"hello","previous_response_id":"resp_1"}`, "key")
	if stateful.Code != http.StatusBadRequest || !strings.Contains(stateful.Body.String(), "stateful response compaction") {
		t.Fatalf("stateful compaction was not rejected: %d %s", stateful.Code, stateful.Body.String())
	}
	for _, invalid := range []string{
		`{"model":"allowed","input":"hello","prompt_cache_options":{"mode":"automatic"}}`,
		`{"model":"allowed","input":"hello","prompt_cache_options":{"ttl":"1h"}}`,
		`{"model":"allowed","input":"hello","prompt_cache_retention":"forever"}`,
		`{"model":"allowed","input":"hello","service_tier":"slow"}`,
		`{"model":"allowed","input":[{"type":"compaction","encrypted_content":""}]}`,
		`{"model":"allowed","input":"hello","temperature":0}`,
	} {
		result := perform(handler, http.MethodPost, "/v1/responses/compact", invalid, "key")
		if result.Code != http.StatusBadRequest {
			t.Fatalf("invalid compaction accepted: status=%d body=%s request=%s", result.Code, result.Body.String(), invalid)
		}
	}
}

func TestResponsesStreamingUsesNamedSSEEvents(t *testing.T) {
	stream := &eventStream{events: []contract.StreamEvent{
		{Type: "response.output_text.delta", Response: &contract.ResponseDelta{ResponseID: "resp_s", Sequence: 1, ItemID: "msg_s", OutputIndex: 0, ContentIndex: 0, Delta: "hi"}},
		{Type: "response.completed", Response: &contract.ResponseDelta{ResponseID: "resp_s", Sequence: 2, Snapshot: &contract.Response{
			Responses: &contract.ResponsesResponse{ID: "resp_s", Status: "completed", Output: []contract.ResponseOutputItem{}},
		}}, Usage: &contract.Usage{InputTokens: 2, OutputTokens: 1, CacheReadTokens: 1, ReasoningTokens: 1, TotalTokens: 3}, Terminal: true},
	}}
	engine := &fakeEngine{snapshot: testSnapshot(t), stream: stream}
	handler := testHandler(t, engine, fakeAuthenticator{})
	response := perform(handler, http.MethodPost, "/v1/responses", `{"model":"allowed","input":"hello","stream":true}`, "key")
	if response.Code != http.StatusOK || !stream.closed {
		t.Fatalf("status=%d closed=%v body=%s", response.Code, stream.closed, response.Body.String())
	}
	want := "event: response.output_text.delta\ndata: {\"content_index\":0,\"delta\":\"hi\",\"item_id\":\"msg_s\",\"output_index\":0,\"response_id\":\"resp_s\",\"sequence_number\":1,\"type\":\"response.output_text.delta\"}\n\n" +
		"event: response.completed\ndata: {\"response\":{\"id\":\"resp_s\",\"model\":\"allowed\",\"object\":\"response\",\"output\":[],\"status\":\"completed\",\"usage\":{\"input_tokens\":2,\"input_tokens_details\":{\"cached_tokens\":1},\"output_tokens\":1,\"output_tokens_details\":{\"reasoning_tokens\":1},\"total_tokens\":3}},\"response_id\":\"resp_s\",\"sequence_number\":2,\"type\":\"response.completed\"}\n\n"
	if response.Body.String() != want {
		t.Fatalf("SSE mismatch\n got: %s\nwant: %s", response.Body.String(), want)
	}
}

func TestResponsesEventEncodingPreservesTypedFields(t *testing.T) {
	request := contract.Request{PublicModel: "public-model"}
	tests := []struct {
		name  string
		event contract.StreamEvent
		want  string
	}{
		{name: "content part", event: contract.StreamEvent{Type: "response.content_part.done", Response: &contract.ResponseDelta{
			Sequence: 4, ItemID: "msg_1", OutputIndex: 1, ContentIndex: 2,
			Part: &contract.ContentPart{Type: "refusal", Refusal: "cannot comply"},
		}}, want: `{"type":"response.content_part.done","sequence_number":4,"item_id":"msg_1","output_index":1,"content_index":2,"part":{"type":"refusal","refusal":"cannot comply"}}`},
		{name: "function arguments", event: contract.StreamEvent{Type: "response.function_call_arguments.done", Response: &contract.ResponseDelta{
			Sequence: 5, ItemID: "call_1", OutputIndex: 0, Name: "weather", Arguments: `{"city":"Pune"}`,
		}}, want: `{"type":"response.function_call_arguments.done","sequence_number":5,"item_id":"call_1","output_index":0,"name":"weather","arguments":"{\"city\":\"Pune\"}"}`},
		{name: "reasoning summary", event: contract.StreamEvent{Type: "response.reasoning_summary_text.done", Response: &contract.ResponseDelta{
			Sequence: 6, ItemID: "reason_1", OutputIndex: 0, SummaryIndex: 1, Text: "checked",
		}}, want: `{"type":"response.reasoning_summary_text.done","sequence_number":6,"item_id":"reason_1","output_index":0,"summary_index":1,"text":"checked"}`},
		{name: "reasoning part", event: contract.StreamEvent{Type: "response.reasoning_part.done", Response: &contract.ResponseDelta{
			Sequence: 7, ItemID: "reason_1", OutputIndex: 0, ContentIndex: 1,
			Part: &contract.ContentPart{Type: "reasoning_text", Text: "checked"},
		}}, want: `{"type":"response.reasoning_part.done","sequence_number":7,"item_id":"reason_1","output_index":0,"content_index":1,"part":{"type":"reasoning_text","text":"checked"}}`},
		{name: "reasoning item", event: contract.StreamEvent{Type: "response.output_item.done", Response: &contract.ResponseDelta{
			Sequence: 8, OutputIndex: 0, Item: &contract.ResponseOutputItem{Type: "reasoning", ID: "reason_1",
				Summary: []contract.ContentPart{}, Content: []contract.ContentPart{{Type: "reasoning_text", Text: "checked"}}},
		}}, want: `{"type":"response.output_item.done","sequence_number":8,"output_index":0,"item":{"type":"reasoning","id":"reason_1","summary":[],"content":[{"type":"reasoning_text","text":"checked"}]}}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			name, data, err := encodeResponseEvent(request, test.event)
			if err != nil || name != test.event.Type {
				t.Fatalf("name=%q data=%s err=%v", name, data, err)
			}
			assertJSONEqual(t, string(data), test.want)
		})
	}
}

func TestResponsesStreamingFailureUsesNamedErrorEventAndCloses(t *testing.T) {
	stream := &eventStream{err: &contract.Error{Code: contract.ErrorProvider, Message: "provider failed", HTTPStatus: http.StatusBadGateway}}
	engine := &fakeEngine{snapshot: testSnapshot(t), stream: stream}
	response := perform(testHandler(t, engine, fakeAuthenticator{}), http.MethodPost, "/v1/responses",
		`{"model":"allowed","input":"hello","stream":true}`, "key")
	if response.Code != http.StatusOK || !stream.closed {
		t.Fatalf("status=%d closed=%v body=%s", response.Code, stream.closed, response.Body.String())
	}
	if !strings.HasPrefix(response.Body.String(), "event: error\ndata: ") || !strings.Contains(response.Body.String(), `"code":"provider_error"`) {
		t.Fatalf("invalid streaming error event: %s", response.Body.String())
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
