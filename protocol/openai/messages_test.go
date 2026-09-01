package openai

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/daptin/llmgateway/contract"
)

func messagesRequestWithKey(body string) *http.Request {
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewBufferString(body))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-API-Key", "key")
	request.Header.Set("X-Request-ID", "req_test")
	return request
}

func TestMessagesProtocolTranslatesToolsImagesAndResponse(t *testing.T) {
	engine := &fakeEngine{snapshot: testSnapshot(t), invokeResult: contract.Response{Model: "actual-model", Usage: contract.Usage{InputTokens: 10, OutputTokens: 4, CacheReadTokens: 2},
		Chat: &contract.ChatResponse{ID: "msg_1", Choices: []contract.ChatChoice{{Index: 0, Message: contract.Message{Role: "assistant",
			Content: []contract.ContentPart{{Type: "text", Text: "calling"}}, ToolCalls: []contract.ToolCall{{ID: "tool_1", Type: "function", Function: contract.FunctionCall{Name: "weather", Arguments: `{"city":"Pune"}`}}}}, FinishReason: "tool_calls"}}}}}
	body := `{"model":"allowed","max_tokens":64,"system":[{"type":"text","text":"be concise"}],"messages":[{"role":"user","content":[{"type":"text","text":"weather"},{"type":"image","source":{"type":"base64","media_type":"image/png","data":"aW1hZ2U="}}]}],"tools":[{"name":"weather","description":"lookup","input_schema":{"type":"object","properties":{"city":{"type":"string"}}}}],"tool_choice":{"type":"tool","name":"weather","disable_parallel_tool_use":true},"metadata":{"user_id":"tenant-user"}}`
	response := httptest.NewRecorder()
	testHandler(t, engine, fakeAuthenticator{}).ServeHTTP(response, messagesRequestWithKey(body))
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	chat := engine.invokeRequest.Chat
	if engine.invokeRequest.Operation != contract.OperationChat || chat == nil || len(chat.Messages) != 2 || chat.Messages[0].Role != "system" ||
		chat.Messages[1].Content[1].ImageURL == nil || !strings.HasPrefix(chat.Messages[1].Content[1].ImageURL.URL, "data:image/png;base64,") ||
		len(chat.Tools) != 1 || chat.ToolChoice == nil || chat.ToolChoice.FunctionName != "weather" || chat.ParallelToolCalls == nil || *chat.ParallelToolCalls || chat.User != "tenant-user" {
		t.Fatalf("canonical messages request=%#v", engine.invokeRequest)
	}
	assertJSONEqual(t, response.Body.String(), `{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"text","text":"calling"},{"type":"tool_use","id":"tool_1","name":"weather","input":{"city":"Pune"}}],"model":"actual-model","stop_reason":"tool_use","stop_sequence":null,"usage":{"input_tokens":10,"output_tokens":4,"cache_read_input_tokens":2,"cache_creation_input_tokens":0}}`)
}

func TestMessagesProtocolTranslatesToolResults(t *testing.T) {
	engine := &fakeEngine{snapshot: testSnapshot(t), invokeResult: contract.Response{Model: "model", Chat: &contract.ChatResponse{ID: "msg", Choices: []contract.ChatChoice{{Message: contract.Message{Role: "assistant", Content: []contract.ContentPart{{Type: "text", Text: "done"}}}, FinishReason: "stop"}}}}}
	body := `{"model":"allowed","max_tokens":32,"messages":[{"role":"assistant","content":[{"type":"tool_use","id":"call_1","name":"lookup","input":{"q":"x"}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"call_1","content":"result"},{"type":"text","text":"continue"}]}]}`
	response := httptest.NewRecorder()
	testHandler(t, engine, fakeAuthenticator{}).ServeHTTP(response, messagesRequestWithKey(body))
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	messages := engine.invokeRequest.Chat.Messages
	if len(messages) != 3 || len(messages[0].ToolCalls) != 1 || messages[1].Role != "tool" || messages[1].ToolCallID != "call_1" || messages[2].Role != "user" {
		t.Fatalf("translated tool history=%#v", messages)
	}
}

func TestMessagesStreamingUsesNamedAnthropicEvents(t *testing.T) {
	stream := &eventStream{events: []contract.StreamEvent{
		{Chat: &contract.ChatDelta{ID: "msg_stream", Role: "assistant", Content: "hel"}},
		{Chat: &contract.ChatDelta{ID: "msg_stream", Content: "lo"}},
		{Chat: &contract.ChatDelta{ID: "msg_stream", FinishReason: "stop"}, Usage: &contract.Usage{InputTokens: 3, OutputTokens: 2}, Terminal: true},
	}}
	engine := &fakeEngine{snapshot: testSnapshot(t), stream: stream}
	response := httptest.NewRecorder()
	testHandler(t, engine, fakeAuthenticator{}).ServeHTTP(response, messagesRequestWithKey(`{"model":"allowed","max_tokens":8,"stream":true,"messages":[{"role":"user","content":"hello"}]}`))
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	output := response.Body.String()
	for _, expected := range []string{"event: message_start", "event: content_block_start", `"text":"hel","type":"text_delta"`,
		`"text":"lo","type":"text_delta"`, "event: content_block_stop", `"stop_reason":"end_turn"`, "event: message_stop"} {
		if !strings.Contains(output, expected) {
			t.Fatalf("stream missing %q: %s", expected, output)
		}
	}
	if !stream.closed {
		t.Fatal("messages stream was not closed")
	}
}

func TestMessagesErrorsUseAnthropicShapeAndDualCredentialsAreRejected(t *testing.T) {
	engine := &fakeEngine{snapshot: testSnapshot(t)}
	request := messagesRequestWithKey(`{"model":"allowed","max_tokens":0,"messages":[]}`)
	request.Header.Set("Authorization", "Bearer second")
	response := httptest.NewRecorder()
	testHandler(t, engine, fakeAuthenticator{}).ServeHTTP(response, request)
	if response.Code != http.StatusUnauthorized || !strings.Contains(response.Body.String(), `"type":"authentication_error"`) || strings.Contains(response.Body.String(), "request_id") {
		t.Fatalf("dual credential response status=%d body=%s", response.Code, response.Body.String())
	}
}
