package contract

import (
	"encoding/json"
	"errors"
	"fmt"
)

func (r Request) Validate() error {
	if r.ID == "" || r.PublicModel == "" || !r.Operation.Valid() {
		return errors.New("request id, model, and operation are required")
	}
	if r.MaxOutputTokens < 0 || !r.EstimatedUsage.Valid() {
		return errors.New("request token bounds cannot be negative")
	}
	count := 0
	if r.Chat != nil {
		count++
	}
	if r.Responses != nil {
		count++
	}
	if r.Embeddings != nil {
		count++
	}
	if r.ImageGeneration != nil {
		count++
	}
	if count != 1 {
		return errors.New("request must contain exactly one operation payload")
	}
	if r.Stream && r.Operation != OperationChat && r.Operation != OperationResponses {
		return errors.New("operation does not support streaming")
	}
	switch r.Operation {
	case OperationChat:
		if r.Chat == nil || len(r.Chat.Messages) == 0 {
			return errors.New("chat request requires messages")
		}
		if err := validateMessages(r.Chat.Messages); err != nil {
			return err
		}
		if err := validateTools(r.Chat.Tools, r.Chat.ToolChoice, r.Chat.ResponseFormat); err != nil {
			return err
		}
		if r.Chat.N < 1 || r.Chat.MaxCompletionTokens < 1 {
			return errors.New("chat n and maximum completion tokens must be positive")
		}
	case OperationResponses:
		if r.Responses == nil || len(r.Responses.Input) == 0 {
			return errors.New("responses request requires input")
		}
		for _, item := range r.Responses.Input {
			if err := validateResponseInput(item); err != nil {
				return err
			}
		}
		if err := validateTools(r.Responses.Tools, r.Responses.ToolChoice, r.Responses.TextFormat); err != nil {
			return err
		}
		if r.MaxOutputTokens < 1 {
			return errors.New("responses maximum output tokens must be positive")
		}
	case OperationEmbeddings:
		if r.Embeddings == nil || (len(r.Embeddings.Input.Texts) == 0 && len(r.Embeddings.Input.Tokens) == 0) {
			return errors.New("embeddings request requires input")
		}
		if len(r.Embeddings.Input.Texts) > 0 && len(r.Embeddings.Input.Tokens) > 0 {
			return errors.New("embeddings input must use text or tokens, not both")
		}
		for _, text := range r.Embeddings.Input.Texts {
			if text == "" {
				return errors.New("embedding text cannot be empty")
			}
		}
		for _, tokens := range r.Embeddings.Input.Tokens {
			if len(tokens) == 0 {
				return errors.New("embedding token input cannot be empty")
			}
			for _, token := range tokens {
				if token < 0 {
					return errors.New("embedding token IDs cannot be negative")
				}
			}
		}
		if r.Embeddings.Dimensions < 0 || (r.Embeddings.EncodingFormat != "float" && r.Embeddings.EncodingFormat != "base64") {
			return errors.New("invalid embeddings dimensions or encoding format")
		}
	case OperationImageGeneration:
		if r.ImageGeneration == nil || r.ImageGeneration.Prompt == "" {
			return errors.New("image generation request requires a prompt")
		}
		if r.ImageGeneration.N < 1 || r.ImageGeneration.N > 10 || (r.ImageGeneration.ResponseFormat != "url" && r.ImageGeneration.ResponseFormat != "b64_json") {
			return errors.New("invalid image count or response format")
		}
	}
	return nil
}

func validateMessages(messages []Message) error {
	for _, message := range messages {
		switch message.Role {
		case "system", "developer", "user", "assistant", "tool":
		default:
			return fmt.Errorf("invalid message role %q", message.Role)
		}
		if len(message.Content) == 0 && len(message.ToolCalls) == 0 {
			return errors.New("message requires content or tool calls")
		}
		for _, part := range message.Content {
			switch part.Type {
			case "text":
				if part.Text == "" {
					return errors.New("text content cannot be empty")
				}
			case "image_url":
				if part.ImageURL == nil || part.ImageURL.URL == "" {
					return errors.New("image content requires a URL")
				}
			case "input_audio":
				if part.Audio == nil || part.Audio.Data == "" || part.Audio.Format == "" {
					return errors.New("audio content requires data and format")
				}
			default:
				return fmt.Errorf("invalid message content type %q", part.Type)
			}
		}
		if message.Role == "tool" && message.ToolCallID == "" {
			return errors.New("tool message requires a tool call ID")
		}
		for _, call := range message.ToolCalls {
			if call.ID == "" || call.Type != "function" || call.Function.Name == "" {
				return errors.New("invalid message tool call")
			}
		}
	}
	return nil
}

func validateTools(tools []Tool, choice *ToolChoice, format *ResponseFormat) error {
	for _, tool := range tools {
		if tool.Type != "function" || tool.Function.Name == "" || !validJSONObject(tool.Function.Parameters) {
			return errors.New("invalid function tool")
		}
	}
	if choice != nil {
		switch choice.Mode {
		case "none", "auto", "required":
		case "function":
			if choice.FunctionName == "" {
				return errors.New("named tool choice requires a function")
			}
		default:
			return errors.New("invalid tool choice")
		}
	}
	if format != nil {
		switch format.Type {
		case "text", "json_object":
		case "json_schema":
			if format.JSONSchema == nil || format.JSONSchema.Name == "" || !validJSONObject(format.JSONSchema.Schema) {
				return errors.New("invalid JSON schema response format")
			}
		default:
			return errors.New("invalid response format")
		}
	}
	return nil
}

func validateResponseInput(item ResponseInputItem) error {
	switch item.Type {
	case "message":
		if item.Role == "" || len(item.Content) == 0 {
			return errors.New("response message requires role and content")
		}
		for _, part := range item.Content {
			switch part.Type {
			case "input_text":
				if part.Text == "" {
					return errors.New("response input text cannot be empty")
				}
			case "input_image":
				if part.ImageURL == nil || part.ImageURL.URL == "" {
					return errors.New("response input image requires a URL")
				}
			default:
				return errors.New("unsupported response input content")
			}
		}
	case "function_call":
		if item.CallID == "" || item.Name == "" || item.Arguments == "" {
			return errors.New("response function call is incomplete")
		}
	case "function_call_output":
		if item.CallID == "" || item.Output == "" {
			return errors.New("response function output is incomplete")
		}
	default:
		return errors.New("unsupported response input item")
	}
	return nil
}

func validJSONObject(raw json.RawMessage) bool {
	var value map[string]any
	return len(raw) > 0 && json.Unmarshal(raw, &value) == nil && value != nil
}
