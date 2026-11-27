package openai

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/daptin/llmgateway/contract"
)

func TestChatContinuationRoundTripsThroughHTTPProtocol(t *testing.T) {
	extra := json.RawMessage(`{"google":{"thought_signature":"opaque+/=state"}}`)
	result := contract.Response{Model: "public", Chat: &contract.ChatResponse{ID: "response", Choices: []contract.ChatChoice{{Index: 0, FinishReason: "tool_calls", Message: contract.Message{Role: "assistant", ToolCalls: []contract.ToolCall{{ID: "signed", Type: "function", Function: contract.FunctionCall{Name: "inspect", Arguments: "{}"}, ExtraContent: extra}}}}}}}
	encoded, err := encodeChatResponse(result)
	if err != nil {
		t.Fatal(err)
	}
	choice := encoded["choices"].([]map[string]interface{})[0]
	body, err := json.Marshal(map[string]interface{}{"model": "public", "messages": []interface{}{choice["message"], map[string]interface{}{"role": "tool", "tool_call_id": "signed", "content": "{}"}}, "n": 1, "max_completion_tokens": 64})
	if err != nil {
		t.Fatal(err)
	}
	request, err := DecodeChatRequest("continuation", body)
	if err != nil {
		t.Fatal(err)
	}
	if err = request.Validate(); err != nil {
		t.Fatal(err)
	}
	var want, got interface{}
	json.Unmarshal(extra, &want)
	json.Unmarshal(request.Chat.Messages[0].ToolCalls[0].ExtraContent, &got)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("HTTP round trip lost provider metadata: %v", got)
	}
	chunk, err := encodeChatChunk(request, contract.StreamEvent{Type: "tool_call_delta", Chat: &contract.ChatDelta{Index: 0, ToolCalls: []contract.ToolCallDelta{{Index: 0, ExtraContent: extra}}}}, false)
	if err != nil {
		t.Fatal(err)
	}
	var wire struct {
		Choices []struct {
			Delta struct {
				ToolCalls []struct {
					ExtraContent json.RawMessage `json:"extra_content"`
				} `json:"tool_calls"`
			} `json:"delta"`
		} `json:"choices"`
	}
	if err = json.Unmarshal(chunk, &wire); err != nil {
		t.Fatal(err)
	}
	json.Unmarshal(wire.Choices[0].Delta.ToolCalls[0].ExtraContent, &got)
	if !reflect.DeepEqual(got, want) {
		t.Fatal("HTTP SSE lost provider metadata")
	}
}
