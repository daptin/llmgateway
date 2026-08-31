package openaicompat

import (
	"bytes"
	"encoding/json"
	"fmt"

	"github.com/daptin/llmgateway/catalog"
	"github.com/daptin/llmgateway/contract"
	"github.com/daptin/llmgateway/internal/jsonx"
)

type deploymentParameters struct {
	Chat            map[string]json.RawMessage `json:"chat,omitempty"`
	Responses       map[string]json.RawMessage `json:"responses,omitempty"`
	Embeddings      map[string]json.RawMessage `json:"embeddings,omitempty"`
	ImageGeneration map[string]json.RawMessage `json:"image_generation,omitempty"`
}

func encodeRequest(deployment catalog.Deployment, request contract.Request) ([]byte, string, error) {
	var value map[string]any
	var path string
	switch request.Operation {
	case contract.OperationChat:
		value = encodeChatRequest(deployment.UpstreamModel, request)
		path = "/chat/completions"
	case contract.OperationResponses:
		value = encodeResponsesRequest(deployment.UpstreamModel, request)
		path = "/responses"
	case contract.OperationEmbeddings:
		value = encodeEmbeddingsRequest(deployment.UpstreamModel, request)
		path = "/embeddings"
	case contract.OperationImageGeneration:
		value = encodeImageRequest(deployment.UpstreamModel, request)
		path = "/images/generations"
	default:
		return nil, "", invalidRequest("unsupported operation", nil)
	}
	parameters, err := parseDeploymentParameters(deployment.Parameters)
	if err != nil {
		return nil, "", invalidRequest("invalid deployment parameters", err)
	}
	for key, raw := range parameters.forOperation(request.Operation) {
		if _, exists := value[key]; exists {
			return nil, "", invalidRequest(fmt.Sprintf("deployment parameter %q conflicts with a canonical request field", key), nil)
		}
		value[key] = raw
	}
	payload, err := json.Marshal(value)
	if err != nil {
		return nil, "", invalidRequest("failed to encode upstream request", err)
	}
	return payload, path, nil
}

func parseDeploymentParameters(raw json.RawMessage) (deploymentParameters, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("{}")) {
		return deploymentParameters{}, nil
	}
	var parameters deploymentParameters
	if err := jsonx.DecodeOne(bytes.NewReader(trimmed), &parameters); err != nil {
		return deploymentParameters{}, fmt.Errorf("invalid OpenAI-compatible deployment parameters: %w", err)
	}
	for operation, values := range map[contract.Operation]map[string]json.RawMessage{
		contract.OperationChat: parameters.Chat, contract.OperationResponses: parameters.Responses,
		contract.OperationEmbeddings: parameters.Embeddings, contract.OperationImageGeneration: parameters.ImageGeneration,
	} {
		for key := range values {
			if key == "" {
				return deploymentParameters{}, fmt.Errorf("%s deployment parameters contain an empty field", operation)
			}
			if _, canonical := canonicalRequestFields[operation][key]; canonical {
				return deploymentParameters{}, fmt.Errorf("%s deployment parameter %q conflicts with a canonical request field", operation, key)
			}
		}
	}
	return parameters, nil
}

func (parameters deploymentParameters) forOperation(operation contract.Operation) map[string]json.RawMessage {
	switch operation {
	case contract.OperationChat:
		return parameters.Chat
	case contract.OperationResponses:
		return parameters.Responses
	case contract.OperationEmbeddings:
		return parameters.Embeddings
	case contract.OperationImageGeneration:
		return parameters.ImageGeneration
	default:
		return nil
	}
}

var canonicalRequestFields = map[contract.Operation]map[string]struct{}{
	contract.OperationChat:            fieldSet("model", "messages", "stream", "stream_options", "tools", "tool_choice", "response_format", "n", "temperature", "top_p", "frequency_penalty", "presence_penalty", "max_completion_tokens", "stop", "user", "seed", "logprobs", "top_logprobs", "parallel_tool_calls", "reasoning_effort"),
	contract.OperationResponses:       fieldSet("model", "input", "stream", "store", "instructions", "tools", "tool_choice", "text", "max_output_tokens", "temperature", "top_p", "parallel_tool_calls", "reasoning"),
	contract.OperationEmbeddings:      fieldSet("model", "input", "encoding_format", "dimensions", "user"),
	contract.OperationImageGeneration: fieldSet("model", "prompt", "n", "response_format", "size", "quality"),
}

func fieldSet(fields ...string) map[string]struct{} {
	result := make(map[string]struct{}, len(fields))
	for _, field := range fields {
		result[field] = struct{}{}
	}
	return result
}

func encodeChatRequest(model string, request contract.Request) map[string]any {
	chat := request.Chat
	value := map[string]any{"model": model, "messages": encodeMessages(chat.Messages), "stream": request.Stream}
	if request.Stream {
		value["stream_options"] = map[string]any{"include_usage": true}
	}
	if len(chat.Tools) > 0 {
		value["tools"] = encodeTools(chat.Tools)
	}
	if chat.ToolChoice != nil {
		value["tool_choice"] = encodeToolChoice(*chat.ToolChoice)
	}
	if chat.ResponseFormat != nil {
		value["response_format"] = encodeChatResponseFormat(*chat.ResponseFormat)
	}
	if chat.N != 0 {
		value["n"] = chat.N
	}
	if chat.Temperature != nil {
		value["temperature"] = *chat.Temperature
	}
	if chat.TopP != nil {
		value["top_p"] = *chat.TopP
	}
	if chat.FrequencyPenalty != nil {
		value["frequency_penalty"] = *chat.FrequencyPenalty
	}
	if chat.PresencePenalty != nil {
		value["presence_penalty"] = *chat.PresencePenalty
	}
	if chat.MaxCompletionTokens > 0 {
		value["max_completion_tokens"] = chat.MaxCompletionTokens
	}
	if len(chat.Stop) > 0 {
		value["stop"] = chat.Stop
	}
	if chat.User != "" {
		value["user"] = chat.User
	}
	if chat.Seed != nil {
		value["seed"] = *chat.Seed
	}
	if chat.Logprobs {
		value["logprobs"] = true
		value["top_logprobs"] = chat.TopLogprobs
	}
	if chat.ParallelToolCalls != nil {
		value["parallel_tool_calls"] = *chat.ParallelToolCalls
	}
	if chat.ReasoningEffort != "" {
		value["reasoning_effort"] = chat.ReasoningEffort
	}
	return value
}

func encodeMessages(messages []contract.Message) []map[string]any {
	result := make([]map[string]any, 0, len(messages))
	for _, message := range messages {
		value := map[string]any{"role": message.Role}
		if message.Name != "" {
			value["name"] = message.Name
		}
		if message.ToolCallID != "" {
			value["tool_call_id"] = message.ToolCallID
		}
		if len(message.Content) == 1 && message.Content[0].Type == "text" {
			value["content"] = message.Content[0].Text
		} else if len(message.Content) > 0 {
			parts := make([]map[string]any, 0, len(message.Content))
			for _, part := range message.Content {
				encoded := map[string]any{"type": part.Type}
				switch part.Type {
				case "text":
					encoded["text"] = part.Text
				case "image_url":
					encoded["image_url"] = map[string]any{"url": part.ImageURL.URL, "detail": part.ImageURL.Detail}
				case "input_audio":
					encoded["input_audio"] = map[string]any{"data": part.Audio.Data, "format": part.Audio.Format}
				}
				parts = append(parts, encoded)
			}
			value["content"] = parts
		} else {
			value["content"] = nil
		}
		if len(message.ToolCalls) > 0 {
			calls := make([]map[string]any, 0, len(message.ToolCalls))
			for _, call := range message.ToolCalls {
				calls = append(calls, map[string]any{"id": call.ID, "type": call.Type, "function": map[string]any{"name": call.Function.Name, "arguments": call.Function.Arguments}})
			}
			value["tool_calls"] = calls
		}
		result = append(result, value)
	}
	return result
}

func encodeTools(tools []contract.Tool) []map[string]any {
	result := make([]map[string]any, 0, len(tools))
	for _, tool := range tools {
		function := map[string]any{"name": tool.Function.Name, "parameters": json.RawMessage(tool.Function.Parameters)}
		if tool.Function.Description != "" {
			function["description"] = tool.Function.Description
		}
		if tool.Function.Strict != nil {
			function["strict"] = *tool.Function.Strict
		}
		result = append(result, map[string]any{"type": tool.Type, "function": function})
	}
	return result
}

func encodeToolChoice(choice contract.ToolChoice) any {
	if choice.Mode != "function" {
		return choice.Mode
	}
	return map[string]any{"type": "function", "function": map[string]any{"name": choice.FunctionName}}
}

func encodeChatResponseFormat(format contract.ResponseFormat) map[string]any {
	value := map[string]any{"type": format.Type}
	if format.JSONSchema != nil {
		schema := map[string]any{"name": format.JSONSchema.Name, "schema": json.RawMessage(format.JSONSchema.Schema)}
		if format.JSONSchema.Description != "" {
			schema["description"] = format.JSONSchema.Description
		}
		if format.JSONSchema.Strict != nil {
			schema["strict"] = *format.JSONSchema.Strict
		}
		value["json_schema"] = schema
	}
	return value
}

func encodeResponsesRequest(model string, request contract.Request) map[string]any {
	responses := request.Responses
	value := map[string]any{"model": model, "input": encodeResponseInput(responses.Input), "stream": request.Stream, "store": false}
	if responses.Instructions != "" {
		value["instructions"] = responses.Instructions
	}
	if len(responses.Tools) > 0 {
		value["tools"] = encodeResponseTools(responses.Tools)
	}
	if responses.ToolChoice != nil {
		value["tool_choice"] = encodeResponseToolChoice(*responses.ToolChoice)
	}
	if responses.TextFormat != nil {
		value["text"] = map[string]any{"format": encodeResponseTextFormat(*responses.TextFormat)}
	}
	if request.MaxOutputTokens > 0 {
		value["max_output_tokens"] = request.MaxOutputTokens
	}
	if responses.Temperature != nil {
		value["temperature"] = *responses.Temperature
	}
	if responses.TopP != nil {
		value["top_p"] = *responses.TopP
	}
	if responses.ParallelToolCalls != nil {
		value["parallel_tool_calls"] = *responses.ParallelToolCalls
	}
	if responses.ReasoningEffort != "" {
		value["reasoning"] = map[string]any{"effort": responses.ReasoningEffort}
	}
	return value
}

func encodeResponseTools(tools []contract.Tool) []map[string]any {
	result := make([]map[string]any, 0, len(tools))
	for _, tool := range tools {
		value := map[string]any{"type": tool.Type, "name": tool.Function.Name, "parameters": json.RawMessage(tool.Function.Parameters)}
		if tool.Function.Description != "" {
			value["description"] = tool.Function.Description
		}
		if tool.Function.Strict != nil {
			value["strict"] = *tool.Function.Strict
		}
		result = append(result, value)
	}
	return result
}

func encodeResponseToolChoice(choice contract.ToolChoice) any {
	if choice.Mode != "function" {
		return choice.Mode
	}
	return map[string]any{"type": "function", "name": choice.FunctionName}
}

func encodeResponseInput(items []contract.ResponseInputItem) []map[string]any {
	result := make([]map[string]any, 0, len(items))
	for _, item := range items {
		value := map[string]any{"type": item.Type}
		switch item.Type {
		case "message":
			value["role"] = item.Role
			parts := make([]map[string]any, 0, len(item.Content))
			for _, part := range item.Content {
				encoded := map[string]any{"type": part.Type}
				if part.Type == "input_text" {
					encoded["text"] = part.Text
				} else if part.Type == "input_image" {
					encoded["image_url"] = part.ImageURL.URL
					if part.ImageURL.Detail != "" {
						encoded["detail"] = part.ImageURL.Detail
					}
				}
				parts = append(parts, encoded)
			}
			value["content"] = parts
		case "function_call":
			value["call_id"], value["name"], value["arguments"] = item.CallID, item.Name, item.Arguments
		case "function_call_output":
			value["call_id"], value["output"] = item.CallID, item.Output
		}
		result = append(result, value)
	}
	return result
}

func encodeResponseTextFormat(format contract.ResponseFormat) map[string]any {
	value := map[string]any{"type": format.Type}
	if format.JSONSchema != nil {
		value["name"] = format.JSONSchema.Name
		value["schema"] = json.RawMessage(format.JSONSchema.Schema)
		if format.JSONSchema.Description != "" {
			value["description"] = format.JSONSchema.Description
		}
		if format.JSONSchema.Strict != nil {
			value["strict"] = *format.JSONSchema.Strict
		}
	}
	return value
}

func encodeEmbeddingsRequest(model string, request contract.Request) map[string]any {
	embeddings := request.Embeddings
	value := map[string]any{"model": model, "encoding_format": embeddings.EncodingFormat}
	if len(embeddings.Input.Texts) == 1 {
		value["input"] = embeddings.Input.Texts[0]
	} else if len(embeddings.Input.Texts) > 0 {
		value["input"] = embeddings.Input.Texts
	} else if len(embeddings.Input.Tokens) == 1 {
		value["input"] = embeddings.Input.Tokens[0]
	} else {
		value["input"] = embeddings.Input.Tokens
	}
	if embeddings.Dimensions > 0 {
		value["dimensions"] = embeddings.Dimensions
	}
	if embeddings.User != "" {
		value["user"] = embeddings.User
	}
	return value
}

func encodeImageRequest(model string, request contract.Request) map[string]any {
	image := request.ImageGeneration
	value := map[string]any{"model": model, "prompt": image.Prompt, "n": image.N, "response_format": image.ResponseFormat}
	if image.Size != "" {
		value["size"] = image.Size
	}
	if image.Quality != "" {
		value["quality"] = image.Quality
	}
	return value
}
