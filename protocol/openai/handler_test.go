package openai

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/daptin/llmgateway/catalog"
	"github.com/daptin/llmgateway/contract"
)

type fakeAuthenticator struct {
	principal contract.Principal
	err       error
}

func (f fakeAuthenticator) Authenticate(context.Context, string) (contract.Principal, error) {
	return f.principal, f.err
}

type fakeEngine struct {
	snapshot      *catalog.Snapshot
	invokeRequest contract.Request
	streamRequest contract.Request
	invokeResult  contract.Response
	invokeErr     error
	stream        contract.EventStream
	streamErr     error
	denied        map[string]bool
}

func (f *fakeEngine) Invoke(_ context.Context, _ contract.Principal, request contract.Request) (contract.Response, error) {
	f.invokeRequest = request
	return f.invokeResult, f.invokeErr
}

func (f *fakeEngine) Stream(_ context.Context, _ contract.Principal, request contract.Request) (contract.EventStream, error) {
	f.streamRequest = request
	return f.stream, f.streamErr
}

func (f *fakeEngine) Snapshot() (*catalog.Snapshot, error) { return f.snapshot, nil }

func (f *fakeEngine) Authorize(_ context.Context, _ contract.Principal, model string) error {
	if f.denied[model] {
		return &contract.Error{Code: contract.ErrorPermission, Message: "permission denied", HTTPStatus: http.StatusForbidden}
	}
	return nil
}

type eventStream struct {
	events []contract.StreamEvent
	index  int
	closed bool
	err    error
}

func (s *eventStream) Next(context.Context) (contract.StreamEvent, error) {
	if s.index == len(s.events) {
		if s.err != nil {
			return contract.StreamEvent{}, s.err
		}
		return contract.StreamEvent{}, io.EOF
	}
	event := s.events[s.index]
	s.index++
	return event, nil
}

func (s *eventStream) Close(context.Context) error {
	s.closed = true
	return nil
}

func testSnapshot(t *testing.T) *catalog.Snapshot {
	t.Helper()
	snapshot, err := catalog.Compile(catalog.Document{
		Revision: 1,
		Models: []catalog.Model{
			{ID: "model-allowed", Name: "allowed", Operations: []contract.Operation{contract.OperationChat}, RoutingStrategy: "priority_weighted", UnsupportedParameterPolicy: "reject", Enabled: true},
			{ID: "model-denied", Name: "denied", Operations: []contract.Operation{contract.OperationChat}, RoutingStrategy: "priority_weighted", UnsupportedParameterPolicy: "reject", Enabled: true},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return snapshot
}

func testHandler(t *testing.T, engine *fakeEngine, authenticator Authenticator) http.Handler {
	t.Helper()
	handler, err := NewHandler(engine, authenticator, Options{NewRequestID: func() (contract.ID, error) { return "req_generated", nil }})
	if err != nil {
		t.Fatal(err)
	}
	return handler
}

func perform(handler http.Handler, method, path, body, token string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(method, path, strings.NewReader(body))
	if body != "" {
		request.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	request.Header.Set("X-Request-ID", "req_test")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

func TestRetryAfterRoundingCannotOverflow(t *testing.T) {
	engine := &fakeEngine{snapshot: testSnapshot(t), invokeErr: &contract.Error{
		Code: contract.ErrorRateLimit, Message: "rate limited", HTTPStatus: http.StatusTooManyRequests,
		Retryable: true, RetryAfter: time.Duration(1<<63 - 1),
	}}
	response := perform(testHandler(t, engine, fakeAuthenticator{}), http.MethodPost, "/v1/chat/completions",
		`{"model":"allowed","messages":[{"role":"user","content":"hello"}]}`, "key")
	if response.Code != http.StatusTooManyRequests || response.Header().Get("Retry-After") != "9223372037" {
		t.Fatalf("status=%d retry-after=%q", response.Code, response.Header().Get("Retry-After"))
	}
}

func decodeObject(t *testing.T, response *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var value map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &value); err != nil {
		t.Fatalf("decode response %q: %v", response.Body.String(), err)
	}
	return value
}

func TestAuthenticationAndRoutingErrorsUseOpenAIShape(t *testing.T) {
	engine := &fakeEngine{snapshot: testSnapshot(t)}
	handler := testHandler(t, engine, fakeAuthenticator{})
	for _, test := range []struct {
		name   string
		method string
		path   string
		status int
	}{
		{name: "missing bearer", method: http.MethodGet, path: "/v1/models", status: http.StatusUnauthorized},
		{name: "unknown route", method: http.MethodGet, path: "/v1/unknown", status: http.StatusNotFound},
		{name: "wrong method", method: http.MethodPost, path: "/v1/models", status: http.StatusNotFound},
	} {
		t.Run(test.name, func(t *testing.T) {
			response := perform(handler, test.method, test.path, "", "")
			if response.Code != test.status {
				t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
			}
			value := decodeObject(t, response)
			if value["error"] == nil || response.Header().Get("X-Request-ID") != "req_test" {
				t.Fatalf("not an OpenAI error response: %#v", value)
			}
		})
	}
}

func TestMalformedTypedAuthenticationErrorFallsBackToInternalError(t *testing.T) {
	engine := &fakeEngine{snapshot: testSnapshot(t)}
	handler := testHandler(t, engine, fakeAuthenticator{err: &contract.Error{Code: "unsafe", Message: "secret", HTTPStatus: 0}})
	response := perform(handler, http.MethodGet, "/v1/models", "", "token")
	if response.Code != http.StatusInternalServerError {
		t.Fatalf("malformed authentication status=%d body=%s", response.Code, response.Body.String())
	}
	errorObject := decodeObject(t, response)["error"].(map[string]any)
	if errorObject["code"] != string(contract.ErrorInternal) || strings.Contains(response.Body.String(), "secret") {
		t.Fatalf("malformed authentication error leaked: %s", response.Body.String())
	}
}

func TestInvalidRequestIDIsRejectedInsteadOfSilentlyReplaced(t *testing.T) {
	handler := testHandler(t, &fakeEngine{snapshot: testSnapshot(t)}, fakeAuthenticator{})
	request := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	request.Header.Set("X-Request-ID", "contains whitespace")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	value := decodeObject(t, response)
	errorObject := value["error"].(map[string]any)
	if errorObject["code"] != string(contract.ErrorInvalidRequest) {
		t.Fatalf("error = %#v", errorObject)
	}
}

func TestChatRejectsUnknownAndDuplicateFields(t *testing.T) {
	handler := testHandler(t, &fakeEngine{snapshot: testSnapshot(t)}, fakeAuthenticator{})
	for _, body := range []string{
		`{"model":"allowed","messages":[{"role":"user","content":"hello"}],"mystery":true}`,
		`{"model":"allowed","model":"denied","messages":[{"role":"user","content":"hello"}]}`,
		`{"model":"allowed","messages":[{"role":"user","content":"hello","role":"assistant"}]}`,
	} {
		response := perform(handler, http.MethodPost, "/v1/chat/completions", body, "key")
		if response.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
		}
	}
}

func TestJSONTransportBoundary(t *testing.T) {
	engine := &fakeEngine{snapshot: testSnapshot(t), invokeResult: contract.Response{
		Model: "allowed", Chat: &contract.ChatResponse{},
	}}
	handler, err := NewHandler(engine, fakeAuthenticator{}, Options{
		MaxBodyBytes: 128,
		NewRequestID: func() (contract.ID, error) { return "req_generated", nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	validBody := `{"model":"allowed","messages":[{"role":"user","content":"hello"}]}`
	for _, test := range []struct {
		name        string
		contentType string
		encoding    string
		body        string
		status      int
	}{
		{name: "JSON with parameters", contentType: "application/json; charset=utf-8", body: validBody, status: http.StatusOK},
		{name: "case-insensitive identity encoding", contentType: "application/json", encoding: "Identity", body: validBody, status: http.StatusOK},
		{name: "JSON prefix is not JSON", contentType: "application/jsonp", body: validBody, status: http.StatusUnsupportedMediaType},
		{name: "malformed media type", contentType: "application/json; charset", body: validBody, status: http.StatusUnsupportedMediaType},
		{name: "compressed body", contentType: "application/json", encoding: "gzip", body: validBody, status: http.StatusUnsupportedMediaType},
		{name: "trailing document", contentType: "application/json", body: validBody + `{}`, status: http.StatusBadRequest},
		{name: "bounded body", contentType: "application/json", body: validBody + strings.Repeat(" ", 128), status: http.StatusRequestEntityTooLarge},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(test.body))
			request.Header.Set("Authorization", "Bearer key")
			request.Header.Set("X-Request-ID", "req_test")
			if test.contentType != "" {
				request.Header.Set("Content-Type", test.contentType)
			}
			if test.encoding != "" {
				request.Header.Set("Content-Encoding", test.encoding)
			}
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != test.status {
				t.Fatalf("status=%d want=%d body=%s", response.Code, test.status, response.Body.String())
			}
			if test.status != http.StatusOK {
				value := decodeObject(t, response)
				if value["error"] == nil || response.Header().Get("X-Request-ID") != "req_test" {
					t.Fatalf("not a normalized error: headers=%v body=%#v", response.Header(), value)
				}
			}
		})
	}
}

func TestChatCanonicalizesTypedMultimodalToolsAndReturnsGoldenResponse(t *testing.T) {
	engine := &fakeEngine{snapshot: testSnapshot(t), invokeResult: contract.Response{
		Model: "allowed",
		Chat: &contract.ChatResponse{ID: "chatcmpl_1", Created: 123, Choices: []contract.ChatChoice{{
			Index: 0, Message: contract.Message{Role: "assistant", ToolCalls: []contract.ToolCall{{ID: "call_1", Type: "function", Function: contract.FunctionCall{Name: "weather", Arguments: `{"city":"Pune"}`}}}}, FinishReason: "tool_calls",
		}}},
		Usage: contract.Usage{InputTokens: 9, OutputTokens: 4, TotalTokens: 13},
	}}
	handler := testHandler(t, engine, fakeAuthenticator{principal: contract.Principal{KeyID: "key-1"}})
	body := `{"model":"allowed","messages":[{"role":"user","content":[{"type":"text","text":"weather"},{"type":"image_url","image_url":{"url":"https://example.test/map.png","detail":"low"}},{"type":"input_audio","input_audio":{"data":"AAAA","format":"wav"}}]}],"tools":[{"type":"function","function":{"name":"weather","parameters":{"type":"object"},"strict":true}}],"tool_choice":{"type":"function","function":{"name":"weather"}},"response_format":{"type":"json_schema","json_schema":{"name":"forecast","schema":{"type":"object"},"strict":true}},"n":2,"temperature":0.2,"top_p":0.9,"frequency_penalty":-0.5,"presence_penalty":1.25,"stop":["done"],"user":"user-1","seed":7,"logprobs":true,"top_logprobs":3,"parallel_tool_calls":false,"reasoning_effort":"low","max_completion_tokens":32}`
	response := perform(handler, http.MethodPost, "/v1/chat/completions", body, "key")
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	request := engine.invokeRequest
	if request.Chat == nil || request.Operation != contract.OperationChat || request.Stream || request.MaxOutputTokens != 32 {
		t.Fatalf("invalid canonical request: %#v", request)
	}
	if len(request.Chat.Messages[0].Content) != 3 || request.Chat.Messages[0].Content[2].Audio == nil ||
		request.Chat.Messages[0].Content[2].Audio.Format != "wav" || request.Chat.ToolChoice.FunctionName != "weather" || !*request.Chat.Tools[0].Function.Strict {
		t.Fatalf("typed fields were not preserved: %#v", request.Chat)
	}
	if request.Chat.FrequencyPenalty == nil || *request.Chat.FrequencyPenalty != -0.5 || request.Chat.PresencePenalty == nil ||
		*request.Chat.PresencePenalty != 1.25 || request.Chat.ParallelToolCalls == nil || *request.Chat.ParallelToolCalls || request.Chat.ReasoningEffort != "low" {
		t.Fatalf("sampling controls were not preserved: %#v", request.Chat)
	}
	if request.Chat.N != 2 || request.Chat.Temperature == nil || *request.Chat.Temperature != 0.2 || request.Chat.TopP == nil || *request.Chat.TopP != 0.9 ||
		request.Chat.ResponseFormat == nil || request.Chat.ResponseFormat.JSONSchema == nil || request.Chat.ResponseFormat.JSONSchema.Name != "forecast" ||
		len(request.Chat.Stop) != 1 || request.Chat.Stop[0] != "done" || request.Chat.User != "user-1" || request.Chat.Seed == nil || *request.Chat.Seed != 7 ||
		!request.Chat.Logprobs || request.Chat.TopLogprobs != 3 {
		t.Fatalf("manifested chat controls were not preserved: %#v", request.Chat)
	}
	want := `{"choices":[{"finish_reason":"tool_calls","index":0,"message":{"content":null,"role":"assistant","tool_calls":[{"function":{"arguments":"{\"city\":\"Pune\"}","name":"weather"},"id":"call_1","type":"function"}]}}],"created":123,"id":"chatcmpl_1","model":"allowed","object":"chat.completion","usage":{"completion_tokens":4,"prompt_tokens":9,"total_tokens":13}}`
	var gotObject, wantObject any
	if err := json.Unmarshal(response.Body.Bytes(), &gotObject); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(want), &wantObject); err != nil {
		t.Fatal(err)
	}
	got, _ := json.Marshal(gotObject)
	expected, _ := json.Marshal(wantObject)
	if string(got) != string(expected) {
		t.Fatalf("response mismatch\n got: %s\nwant: %s", got, expected)
	}
}

func TestChatRejectsInvalidSamplingControls(t *testing.T) {
	handler := testHandler(t, &fakeEngine{snapshot: testSnapshot(t)}, fakeAuthenticator{})
	for _, body := range []string{
		`{"model":"allowed","messages":[{"role":"user","content":"hello"}],"frequency_penalty":2.1}`,
		`{"model":"allowed","messages":[{"role":"user","content":"hello"}],"presence_penalty":-2.1}`,
		`{"model":"allowed","messages":[{"role":"user","content":"hello"}],"reasoning_effort":"maximum"}`,
	} {
		response := perform(handler, http.MethodPost, "/v1/chat/completions", body, "key")
		if response.Code != http.StatusBadRequest {
			t.Fatalf("accepted invalid controls: status=%d body=%s", response.Code, response.Body.String())
		}
	}
}

func TestChatStreamingSSEIsDeterministicAndCloses(t *testing.T) {
	stream := &eventStream{events: []contract.StreamEvent{
		{Chat: &contract.ChatDelta{ID: "chatcmpl_s", Created: 456, Index: 0, Role: "assistant", Content: "hi"}},
		{Chat: &contract.ChatDelta{ID: "chatcmpl_s", Created: 456, Index: 0, FinishReason: "stop"}, Usage: &contract.Usage{InputTokens: 2, OutputTokens: 1, TotalTokens: 3}, Terminal: true},
	}}
	engine := &fakeEngine{snapshot: testSnapshot(t), stream: stream}
	handler := testHandler(t, engine, fakeAuthenticator{})
	body := `{"model":"allowed","messages":[{"role":"user","content":"hello"}],"stream":true,"stream_options":{"include_usage":true}}`
	response := perform(handler, http.MethodPost, "/v1/chat/completions", body, "key")
	if response.Code != http.StatusOK || response.Header().Get("Content-Type") != "text/event-stream" {
		t.Fatalf("status = %d, headers = %#v, body = %s", response.Code, response.Header(), response.Body.String())
	}
	if !engine.streamRequest.Stream || !stream.closed {
		t.Fatalf("stream lifecycle mismatch: request=%#v closed=%v", engine.streamRequest, stream.closed)
	}
	want := "data: {\"choices\":[{\"delta\":{\"content\":\"hi\",\"role\":\"assistant\"},\"finish_reason\":null,\"index\":0}],\"created\":456,\"id\":\"chatcmpl_s\",\"model\":\"allowed\",\"object\":\"chat.completion.chunk\",\"usage\":null}\n\n" +
		"data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\",\"index\":0}],\"created\":456,\"id\":\"chatcmpl_s\",\"model\":\"allowed\",\"object\":\"chat.completion.chunk\",\"usage\":{\"prompt_tokens\":2,\"completion_tokens\":1,\"total_tokens\":3}}\n\n" +
		"data: [DONE]\n\n"
	if response.Body.String() != want {
		t.Fatalf("SSE mismatch\n got: %s\nwant: %s", response.Body.String(), want)
	}
}

func TestChatStreamingFailureIsFramedTerminatesAndCloses(t *testing.T) {
	stream := &eventStream{err: &contract.Error{Code: contract.ErrorProvider, Message: "provider failed", HTTPStatus: http.StatusBadGateway}}
	engine := &fakeEngine{snapshot: testSnapshot(t), stream: stream}
	response := perform(testHandler(t, engine, fakeAuthenticator{}), http.MethodPost, "/v1/chat/completions",
		`{"model":"allowed","messages":[{"role":"user","content":"hello"}],"stream":true}`, "key")
	if response.Code != http.StatusOK || !stream.closed {
		t.Fatalf("status=%d closed=%v body=%s", response.Code, stream.closed, response.Body.String())
	}
	if !strings.Contains(response.Body.String(), `"code":"provider_error"`) || !strings.HasSuffix(response.Body.String(), "data: [DONE]\n\n") {
		t.Fatalf("invalid streaming error framing: %s", response.Body.String())
	}
}

func TestModelsUseEngineAuthorization(t *testing.T) {
	engine := &fakeEngine{snapshot: testSnapshot(t), denied: map[string]bool{"denied": true}}
	handler := testHandler(t, engine, fakeAuthenticator{})
	response := perform(handler, http.MethodGet, "/v1/models", "", "key")
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	value := decodeObject(t, response)
	models := value["data"].([]any)
	if len(models) != 1 || models[0].(map[string]any)["id"] != "allowed" {
		t.Fatalf("authorization was not applied: %#v", models)
	}
	visible := perform(handler, http.MethodGet, "/v1/models/allowed", "", "key")
	visibleModel := decodeObject(t, visible)
	if visible.Code != http.StatusOK || visibleModel["owned_by"] != "llmgateway" || visibleModel["llmgateway"] == nil || visibleModel["daptin"] != nil {
		t.Fatalf("model metadata is not host-neutral: status=%d body=%#v", visible.Code, visibleModel)
	}
	denied := perform(handler, http.MethodGet, "/v1/models/denied", "", "key")
	if denied.Code != http.StatusNotFound {
		t.Fatalf("denied model leaked with status %d: %s", denied.Code, denied.Body.String())
	}
}

func TestRequestIDGeneratorFailureIsSafe(t *testing.T) {
	engine := &fakeEngine{snapshot: testSnapshot(t)}
	handler, err := NewHandler(engine, fakeAuthenticator{}, Options{NewRequestID: func() (contract.ID, error) { return "", errors.New("entropy failed") }})
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
}
