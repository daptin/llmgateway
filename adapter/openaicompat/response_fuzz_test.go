package openaicompat

import (
	"encoding/json"
	"testing"
	"unicode/utf8"

	"github.com/daptin/llmgateway/contract"
)

func FuzzDecodeProviderResponseDoesNotPanic(f *testing.F) {
	for _, seed := range []string{
		`{"id":"chat","choices":[{"message":{"role":"assistant","content":"ok"}}]}`,
		`{"data":[{"index":0,"embedding":[0.1,0.2]}]}`,
		`{"data":[{}]}`,
		`{`,
	} {
		f.Add([]byte(seed))
	}
	f.Fuzz(func(t *testing.T, payload []byte) {
		for _, operation := range []contract.Operation{
			contract.OperationChat,
			contract.OperationResponses,
			contract.OperationEmbeddings,
			contract.OperationImageGeneration,
		} {
			_, _ = decodeResponse(operation, payload)
		}
	})
}

func FuzzFinalToolArgumentsArePreserved(f *testing.F) {
	for _, seed := range []string{`{"city":"Pune"}`, `{`, `null`, "", "plain text"} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, arguments string) {
		if !utf8.ValidString(arguments) || len(arguments) > 1<<20 {
			return
		}
		payload, err := json.Marshal(map[string]any{
			"id": "chat", "choices": []any{map[string]any{
				"message": map[string]any{"role": "assistant", "tool_calls": []any{map[string]any{
					"id": "call", "type": "function", "function": map[string]any{"name": "tool", "arguments": arguments},
				}}},
			}},
		})
		if err != nil {
			t.Fatal(err)
		}
		response, err := decodeResponse(contract.OperationChat, payload)
		if err != nil {
			t.Fatal(err)
		}
		got := response.Chat.Choices[0].Message.ToolCalls[0].Function.Arguments
		if got != arguments {
			t.Fatalf("tool arguments changed: got %q want %q", got, arguments)
		}
	})
}
