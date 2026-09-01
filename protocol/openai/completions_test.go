package openai

import (
	"net/http"
	"strings"
	"testing"

	"github.com/daptin/llmgateway/contract"
)

func TestTextCompletionProtocolUsesNativeContract(t *testing.T) {
	engine := &fakeEngine{snapshot: testSnapshot(t), invokeResult: contract.Response{
		Model: "allowed", TextCompletion: &contract.TextCompletionResponse{ID: "cmpl_1", Created: 123,
			Choices: []contract.TextCompletionChoice{{Index: 0, Text: " world", FinishReason: "stop"}}},
		Usage: contract.Usage{InputTokens: 1, OutputTokens: 1, TotalTokens: 2},
	}}
	response := perform(testHandler(t, engine, fakeAuthenticator{}), http.MethodPost, "/v1/completions",
		`{"model":"allowed","prompt":"hello","max_tokens":8,"n":1,"best_of":1,"echo":false,"temperature":0,"logprobs":2,"stop":"done"}`, "key")
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	request := engine.invokeRequest
	if request.Operation != contract.OperationTextCompletion || request.Chat != nil || request.TextCompletion == nil ||
		len(request.TextCompletion.Prompt.Texts) != 1 || request.TextCompletion.Prompt.Texts[0] != "hello" ||
		request.TextCompletion.MaxTokens != 8 || request.TextCompletion.N != 1 || request.TextCompletion.BestOf != 1 ||
		request.TextCompletion.Echo == nil || *request.TextCompletion.Echo || request.TextCompletion.Logprobs == nil || *request.TextCompletion.Logprobs != 2 {
		t.Fatalf("canonical request=%#v", request)
	}
	assertJSONEqual(t, response.Body.String(), `{"id":"cmpl_1","object":"text_completion","created":123,"model":"allowed","choices":[{"text":" world","index":0,"logprobs":null,"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
}

func TestTextCompletionPromptFormsAndInvalidCombinations(t *testing.T) {
	for _, body := range []string{
		`{"model":"allowed","prompt":[1,2,3],"max_tokens":1}`,
		`{"model":"allowed","prompt":[[1,2],[3]],"max_tokens":1}`,
		`{"model":"allowed","prompt":["one","two"],"max_tokens":1}`,
	} {
		response := perform(testHandler(t, &fakeEngine{snapshot: testSnapshot(t), invokeResult: contract.Response{
			Model: "allowed", TextCompletion: &contract.TextCompletionResponse{ID: "cmpl", Choices: []contract.TextCompletionChoice{{Index: 0}}},
		}}, fakeAuthenticator{}), http.MethodPost, "/v1/completions", body, "key")
		if response.Code != http.StatusOK {
			t.Fatalf("valid prompt rejected: status=%d body=%s", response.Code, response.Body.String())
		}
	}
	for _, body := range []string{
		`{"model":"allowed","prompt":[],"max_tokens":1}`,
		`{"model":"allowed","prompt":["text",1],"max_tokens":1}`,
		`{"model":"allowed","prompt":[-1],"max_tokens":1}`,
		`{"model":"allowed","prompt":"hello","n":2,"best_of":1,"max_tokens":1}`,
		`{"model":"allowed","prompt":"hello","stream":true,"n":1,"best_of":2,"max_tokens":1}`,
	} {
		response := perform(testHandler(t, &fakeEngine{snapshot: testSnapshot(t)}, fakeAuthenticator{}), http.MethodPost, "/v1/completions", body, "key")
		if response.Code != http.StatusBadRequest {
			t.Fatalf("invalid completion accepted: status=%d body=%s request=%s", response.Code, response.Body.String(), body)
		}
	}
}

func TestTextCompletionStreamingPreservesChunksUsageAndTermination(t *testing.T) {
	stream := &eventStream{events: []contract.StreamEvent{
		{TextCompletion: &contract.TextCompletionDelta{ID: "cmpl_s", Created: 5, Index: 0, Text: "hi"}},
		{TextCompletion: &contract.TextCompletionDelta{ID: "cmpl_s", Created: 5, Index: 0, FinishReason: "stop"},
			Usage: &contract.Usage{InputTokens: 1, OutputTokens: 1, TotalTokens: 2}, Terminal: true},
	}}
	engine := &fakeEngine{snapshot: testSnapshot(t), stream: stream}
	response := perform(testHandler(t, engine, fakeAuthenticator{}), http.MethodPost, "/v1/completions",
		`{"model":"allowed","prompt":"hello","max_tokens":2,"stream":true,"stream_options":{"include_usage":true}}`, "key")
	if response.Code != http.StatusOK || !stream.closed || !strings.Contains(response.Body.String(), `"object":"text_completion"`) ||
		!strings.Contains(response.Body.String(), `"completion_tokens":1`) || !strings.HasSuffix(response.Body.String(), "data: [DONE]\n\n") {
		t.Fatalf("invalid text completion stream: status=%d closed=%v body=%s", response.Code, stream.closed, response.Body.String())
	}
}
