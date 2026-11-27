package contract

import (
	"encoding/json"
	"testing"
)

func TestToolContinuationIsBoundToItsCallAndRequiresAnObject(t *testing.T) {
	for _, extra := range []string{``, `{"google":{"thought_signature":"opaque"}}`, `null`, `[]`, `"opaque"`, `{`} {
		request := Request{ID: "test", PublicModel: "model", Operation: OperationChat, Chat: &ChatRequest{N: 1, MaxCompletionTokens: 64, Messages: []Message{{Role: "assistant", ToolCalls: []ToolCall{{ID: "call", Type: "function", Function: FunctionCall{Name: "inspect", Arguments: "{}"}, ExtraContent: json.RawMessage(extra)}}}}}}
		err := request.Validate()
		valid := extra == "" || extra == `{"google":{"thought_signature":"opaque"}}`
		if (err == nil) != valid {
			t.Errorf("extra %q valid=%v got %v", extra, valid, err)
		}
	}
}
