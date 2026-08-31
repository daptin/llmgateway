package openai

import (
	"encoding/json"
	"errors"

	"github.com/daptin/llmgateway/contract"
)

type usageResponse struct {
	PromptTokens     int64 `json:"prompt_tokens"`
	CompletionTokens int64 `json:"completion_tokens"`
	TotalTokens      int64 `json:"total_tokens"`
}

func encodeChatResponse(response contract.Response) (map[string]any, error) {
	if response.Chat == nil {
		return nil, gatewayError(contract.ErrorProvider, "provider returned no chat response", 502, false, nil)
	}
	choices := make([]map[string]any, 0, len(response.Chat.Choices))
	for _, choice := range response.Chat.Choices {
		message, err := encodeMessage(choice.Message)
		if err != nil {
			return nil, err
		}
		encoded := map[string]any{"index": choice.Index, "message": message, "finish_reason": choice.FinishReason}
		if len(choice.Logprobs) > 0 {
			var logprobs any
			if err := json.Unmarshal(choice.Logprobs, &logprobs); err != nil {
				return nil, gatewayError(contract.ErrorProvider, "provider returned invalid logprobs", 502, false, err)
			}
			encoded["logprobs"] = logprobs
		}
		choices = append(choices, encoded)
	}
	return map[string]any{
		"id": response.Chat.ID, "object": "chat.completion", "created": response.Chat.Created,
		"model": response.Model, "choices": choices, "usage": encodeUsage(response.Usage),
	}, nil
}

func encodeMessage(message contract.Message) (map[string]any, error) {
	encoded := map[string]any{"role": message.Role}
	if message.Name != "" {
		encoded["name"] = message.Name
	}
	if message.ToolCallID != "" {
		encoded["tool_call_id"] = message.ToolCallID
	}
	if len(message.Content) > 0 {
		if len(message.Content) == 1 && message.Content[0].Type == "text" {
			encoded["content"] = message.Content[0].Text
		} else {
			parts := make([]map[string]any, 0, len(message.Content))
			for _, part := range message.Content {
				switch part.Type {
				case "text":
					parts = append(parts, map[string]any{"type": "text", "text": part.Text})
				case "image_url":
					if part.ImageURL == nil {
						return nil, errors.New("image content is missing image data")
					}
					parts = append(parts, map[string]any{"type": "image_url", "image_url": map[string]any{"url": part.ImageURL.URL, "detail": part.ImageURL.Detail}})
				default:
					return nil, gatewayError(contract.ErrorProvider, "provider returned unsupported message content", 502, false, nil)
				}
			}
			encoded["content"] = parts
		}
	} else {
		encoded["content"] = nil
	}
	if len(message.ToolCalls) > 0 {
		calls := make([]map[string]any, 0, len(message.ToolCalls))
		for _, call := range message.ToolCalls {
			calls = append(calls, map[string]any{"id": call.ID, "type": call.Type, "function": map[string]any{"name": call.Function.Name, "arguments": call.Function.Arguments}})
		}
		encoded["tool_calls"] = calls
	}
	return encoded, nil
}

func encodeChatChunk(request contract.Request, event contract.StreamEvent, includeUsage bool) ([]byte, error) {
	chunk := map[string]any{
		"id": "", "object": "chat.completion.chunk", "created": int64(0),
		"model": request.PublicModel, "choices": []any{},
	}
	if event.Chat != nil {
		delta := map[string]any{}
		if event.Chat.Role != "" {
			delta["role"] = event.Chat.Role
		}
		if event.Chat.Content != "" {
			delta["content"] = event.Chat.Content
		}
		if len(event.Chat.ToolCalls) > 0 {
			calls := make([]map[string]any, 0, len(event.Chat.ToolCalls))
			for _, call := range event.Chat.ToolCalls {
				calls = append(calls, map[string]any{"index": call.Index, "id": call.ID, "type": call.Type, "function": map[string]any{"name": call.Function.Name, "arguments": call.Function.Arguments}})
			}
			delta["tool_calls"] = calls
		}
		chunk["id"] = event.Chat.ID
		chunk["created"] = event.Chat.Created
		choice := map[string]any{"index": event.Chat.Index, "delta": delta, "finish_reason": nil}
		if event.Chat.FinishReason != "" {
			choice["finish_reason"] = event.Chat.FinishReason
		}
		if len(event.Chat.Logprobs) > 0 {
			var logprobs any
			if err := json.Unmarshal(event.Chat.Logprobs, &logprobs); err != nil {
				return nil, err
			}
			choice["logprobs"] = logprobs
		}
		chunk["choices"] = []any{choice}
	}
	if includeUsage && event.Usage != nil {
		chunk["usage"] = encodeUsage(*event.Usage)
	} else {
		chunk["usage"] = nil
	}
	return json.Marshal(chunk)
}

func encodeUsage(usage contract.Usage) usageResponse {
	return usageResponse{PromptTokens: usage.InputTokens, CompletionTokens: usage.OutputTokens, TotalTokens: usage.TotalTokens}
}

func encodeErrorEvent(err error, requestID contract.ID) []byte {
	public := gatewayError(contract.ErrorInternal, "internal server error", 500, false, err)
	var typed *contract.Error
	if errors.As(err, &typed) {
		public = typed.Safe()
	}
	encoded, marshalErr := json.Marshal(map[string]any{"error": map[string]any{
		"message": public.Message, "type": string(public.Code), "code": string(public.Code), "request_id": requestID,
	}})
	if marshalErr != nil {
		return []byte(`{"error":{"message":"internal server error","type":"internal_error","code":"internal_error"}}`)
	}
	return encoded
}
