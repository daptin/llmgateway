package openaicompat

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/daptin/llmgateway/contract"
)

func TestToolContinuationSurvivesAdapterAndStoredHistory(t *testing.T) {
	extra := json.RawMessage(`{"google":{"thought_signature":"opaque+/=\n\"state"},"provider_extension":{"value":7}}`)
	var expected interface{}
	if err := json.Unmarshal(extra, &expected); err != nil {
		t.Fatal(err)
	}
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		w.Header().Set("Content-Type", "application/json")
		if requests == 1 {
			fmt.Fprintf(w, `{"id":"first","choices":[{"index":0,"message":{"role":"assistant","content":null,"tool_calls":[{"id":"signed","type":"function","function":{"name":"inspect","arguments":"{}"},"extra_content":%s},{"id":"unsigned","type":"function","function":{"name":"inspect","arguments":"{}"}}]},"finish_reason":"tool_calls"}]}`, extra)
			return
		}
		var body map[string]interface{}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
			w.WriteHeader(400)
			return
		}
		calls := body["messages"].([]interface{})[1].(map[string]interface{})["tool_calls"].([]interface{})
		if got := calls[0].(map[string]interface{})["extra_content"]; !reflect.DeepEqual(got, expected) {
			t.Errorf("continuation changed: got %v want %v", got, expected)
		}
		if _, ok := calls[1].(map[string]interface{})["extra_content"]; ok {
			t.Error("metadata leaked onto an unsigned parallel call")
		}
		io.WriteString(w, `{"id":"second","choices":[{"index":0,"message":{"role":"assistant","content":"done"},"finish_reason":"stop"}]}`)
	}))
	defer server.Close()
	adapter := buildAdapter(t, server, `{}`, Factory{})
	initial := contract.Message{Role: "user", Content: []contract.ContentPart{{Type: "text", Text: "inspect"}}}
	request := contract.Request{Operation: contract.OperationChat, Chat: &contract.ChatRequest{Messages: []contract.Message{initial}, N: 1, MaxCompletionTokens: 64}}
	result, err := adapter.Invoke(context.Background(), deployment(), request)
	if err != nil {
		t.Fatal(err)
	}
	// Use the same canonical JSON boundary as hosts persisting a conversation.
	saved, err := json.Marshal(result.Chat.Choices[0].Message)
	if err != nil {
		t.Fatal(err)
	}
	var restored contract.Message
	if err = json.Unmarshal(saved, &restored); err != nil {
		t.Fatal(err)
	}
	request.Chat.Messages = append(request.Chat.Messages, restored,
		contract.Message{Role: "tool", ToolCallID: "signed", Content: []contract.ContentPart{{Type: "text", Text: "{}"}}},
		contract.Message{Role: "tool", ToolCallID: "unsigned", Content: []contract.ContentPart{{Type: "text", Text: "{}"}}})
	if _, err = adapter.Invoke(context.Background(), deployment(), request); err != nil {
		t.Fatal(err)
	}
	if requests != 2 {
		t.Fatalf("requests = %d", requests)
	}
}

func TestStreamPreservesContinuationOnLateToolDelta(t *testing.T) {
	extra := json.RawMessage(`{"google":{"thought_signature":"signed+/=state"}}`)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"signed\",\"type\":\"function\",\"function\":{\"name\":\"inspect\",\"arguments\":\"{}\"}}]}}]}\n\n")
		fmt.Fprintf(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"extra_content\":%s}]},\"finish_reason\":\"tool_calls\"}]}\n\ndata: [DONE]\n\n", extra)
	}))
	defer server.Close()
	adapter := buildAdapter(t, server, `{}`, Factory{})
	stream, err := adapter.Stream(context.Background(), deployment(), contract.Request{Operation: contract.OperationChat, Stream: true, Chat: &contract.ChatRequest{Messages: []contract.Message{{Role: "user", Content: []contract.ContentPart{{Type: "text", Text: "inspect"}}}}}})
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	first, err := stream.Next(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Chat.ToolCalls[0].ExtraContent) != 0 {
		t.Fatal("unexpected metadata before signature delta")
	}
	last, err := stream.Next(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if last.Chat.ToolCalls[0].Index != 0 || string(last.Chat.ToolCalls[0].ExtraContent) != string(extra) {
		t.Fatalf("signature lost on late delta: %#v", last)
	}
}

func TestProviderContinuationMustBeAnObject(t *testing.T) {
	for _, extra := range []string{`null`, `[]`, `"text"`, `42`} {
		t.Run(extra, func(t *testing.T) {
			payload := []byte(fmt.Sprintf(`{"choices":[{"index":0,"message":{"role":"assistant","tool_calls":[{"id":"call","type":"function","function":{"name":"inspect","arguments":"{}"},"extra_content":%s}]}}]}`, extra))
			if _, err := decodeResponse(contract.OperationChat, payload); err == nil {
				t.Fatal("accepted malformed provider continuation")
			}
			chunk := strings.Replace(string(payload), `"message":`, `"delta":`, 1)
			if _, err := decodeChatEvents([]byte(chunk)); err == nil {
				t.Fatal("accepted malformed streaming provider continuation")
			}
		})
	}
}
