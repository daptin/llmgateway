package openai

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/daptin/llmgateway/contract"
)

type chatRequest struct {
	Model               string          `json:"model"`
	Messages            []chatMessage   `json:"messages"`
	Stream              bool            `json:"stream,omitempty"`
	StreamOptions       *streamOptions  `json:"stream_options,omitempty"`
	Tools               []chatTool      `json:"tools,omitempty"`
	ToolChoice          json.RawMessage `json:"tool_choice,omitempty"`
	ResponseFormat      *responseFormat `json:"response_format,omitempty"`
	N                   *int            `json:"n,omitempty"`
	Temperature         *float64        `json:"temperature,omitempty"`
	TopP                *float64        `json:"top_p,omitempty"`
	MaxTokens           *int64          `json:"max_tokens,omitempty"`
	MaxCompletionTokens *int64          `json:"max_completion_tokens,omitempty"`
	Stop                json.RawMessage `json:"stop,omitempty"`
	User                string          `json:"user,omitempty"`
	Seed                *int64          `json:"seed,omitempty"`
	Logprobs            bool            `json:"logprobs,omitempty"`
	TopLogprobs         *int            `json:"top_logprobs,omitempty"`
}

type streamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

type chatMessage struct {
	Role       string          `json:"role"`
	Name       string          `json:"name,omitempty"`
	Content    json.RawMessage `json:"content"`
	ToolCalls  []toolCall      `json:"tool_calls,omitempty"`
	ToolCallID string          `json:"tool_call_id,omitempty"`
}

type contentPart struct {
	Type       string      `json:"type"`
	Text       string      `json:"text,omitempty"`
	ImageURL   *imageURL   `json:"image_url,omitempty"`
	InputAudio *inputAudio `json:"input_audio,omitempty"`
}

type imageURL struct {
	URL    string `json:"url"`
	Detail string `json:"detail,omitempty"`
}

type inputAudio struct {
	Data   string `json:"data"`
	Format string `json:"format"`
}

type chatTool struct {
	Type     string             `json:"type"`
	Function functionDefinition `json:"function"`
}

type functionDefinition struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters"`
	Strict      *bool           `json:"strict,omitempty"`
}

type toolCall struct {
	ID       string       `json:"id"`
	Type     string       `json:"type"`
	Function functionCall `json:"function"`
}

type functionCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type responseFormat struct {
	Type       string      `json:"type"`
	JSONSchema *jsonSchema `json:"json_schema,omitempty"`
}

type jsonSchema struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Schema      json.RawMessage `json:"schema"`
	Strict      *bool           `json:"strict,omitempty"`
}

func (h *Handler) chatCompletions(response http.ResponseWriter, request *http.Request) {
	id, err := h.requestID(request)
	if err != nil {
		writeError(response, gatewayError(contract.ErrorInternal, "failed to create request ID", http.StatusInternalServerError, false, err), "")
		return
	}
	principal, err := h.authenticate(request)
	if err != nil {
		writeError(response, err, id)
		return
	}
	body, err := readJSONBody(response, request, h.maxBody)
	if err != nil {
		writeError(response, err, id)
		return
	}
	var wire chatRequest
	if err := decodeStrict(body, &wire); err != nil {
		writeError(response, gatewayError(contract.ErrorInvalidRequest, "invalid chat completion request", http.StatusBadRequest, false, err), id)
		return
	}
	canonical, includeUsage, err := h.canonicalChat(id, wire, int64(len(body)))
	if err != nil {
		writeError(response, err, id)
		return
	}
	if canonical.Stream {
		h.streamChat(response, request, principal, canonical, includeUsage)
		return
	}
	result, err := h.engine.Invoke(request.Context(), principal, canonical)
	if err != nil {
		writeError(response, err, id)
		return
	}
	encoded, err := encodeChatResponse(result)
	if err != nil {
		writeError(response, err, id)
		return
	}
	writeJSON(response, http.StatusOK, id, encoded)
}

func (h *Handler) canonicalChat(id contract.ID, wire chatRequest, requestBytes int64) (contract.Request, bool, error) {
	if strings.TrimSpace(wire.Model) == "" || len(wire.Messages) == 0 {
		return contract.Request{}, false, gatewayError(contract.ErrorInvalidRequest, "model and messages are required", http.StatusBadRequest, false, nil)
	}
	if wire.MaxTokens != nil && wire.MaxCompletionTokens != nil {
		return contract.Request{}, false, gatewayError(contract.ErrorInvalidRequest, "max_tokens and max_completion_tokens cannot both be set", http.StatusBadRequest, false, nil)
	}
	maximum := h.maxOutput
	if wire.MaxCompletionTokens != nil {
		maximum = *wire.MaxCompletionTokens
	} else if wire.MaxTokens != nil {
		maximum = *wire.MaxTokens
	}
	if maximum < 1 {
		return contract.Request{}, false, gatewayError(contract.ErrorInvalidRequest, "maximum output tokens must be positive", http.StatusBadRequest, false, nil)
	}
	n := 1
	if wire.N != nil {
		n = *wire.N
	}
	if n < 1 || n > 128 {
		return contract.Request{}, false, gatewayError(contract.ErrorInvalidRequest, "n must be between 1 and 128", http.StatusBadRequest, false, nil)
	}
	if wire.Temperature != nil && (*wire.Temperature < 0 || *wire.Temperature > 2) {
		return contract.Request{}, false, gatewayError(contract.ErrorInvalidRequest, "temperature must be between 0 and 2", http.StatusBadRequest, false, nil)
	}
	if wire.TopP != nil && (*wire.TopP < 0 || *wire.TopP > 1) {
		return contract.Request{}, false, gatewayError(contract.ErrorInvalidRequest, "top_p must be between 0 and 1", http.StatusBadRequest, false, nil)
	}
	chat := &contract.ChatRequest{N: n, Temperature: wire.Temperature, TopP: wire.TopP, MaxCompletionTokens: maximum, User: wire.User, Seed: wire.Seed, Logprobs: wire.Logprobs}
	if wire.TopLogprobs != nil {
		if *wire.TopLogprobs < 0 || *wire.TopLogprobs > 20 || !wire.Logprobs {
			return contract.Request{}, false, gatewayError(contract.ErrorInvalidRequest, "top_logprobs requires logprobs and must be between 0 and 20", http.StatusBadRequest, false, nil)
		}
		chat.TopLogprobs = *wire.TopLogprobs
	}
	for index, message := range wire.Messages {
		converted, err := convertMessage(message)
		if err != nil {
			return contract.Request{}, false, gatewayError(contract.ErrorInvalidRequest, fmt.Sprintf("invalid message at index %d", index), http.StatusBadRequest, false, err)
		}
		chat.Messages = append(chat.Messages, converted)
	}
	for _, tool := range wire.Tools {
		if tool.Type != "function" || tool.Function.Name == "" || len(tool.Function.Parameters) == 0 {
			return contract.Request{}, false, gatewayError(contract.ErrorInvalidRequest, "invalid function tool", http.StatusBadRequest, false, nil)
		}
		chat.Tools = append(chat.Tools, contract.Tool{Type: tool.Type, Function: contract.FunctionDefinition{Name: tool.Function.Name, Description: tool.Function.Description, Parameters: append([]byte(nil), tool.Function.Parameters...), Strict: tool.Function.Strict}})
	}
	choice, err := convertToolChoice(wire.ToolChoice)
	if err != nil {
		return contract.Request{}, false, gatewayError(contract.ErrorInvalidRequest, "invalid tool_choice", http.StatusBadRequest, false, err)
	}
	chat.ToolChoice = choice
	if wire.ResponseFormat != nil {
		converted, err := convertResponseFormat(*wire.ResponseFormat)
		if err != nil {
			return contract.Request{}, false, gatewayError(contract.ErrorInvalidRequest, "invalid response_format", http.StatusBadRequest, false, err)
		}
		chat.ResponseFormat = converted
	}
	stop, err := convertStop(wire.Stop)
	if err != nil {
		return contract.Request{}, false, gatewayError(contract.ErrorInvalidRequest, "invalid stop value", http.StatusBadRequest, false, err)
	}
	chat.Stop = stop
	includeUsage := wire.StreamOptions != nil && wire.StreamOptions.IncludeUsage
	if !wire.Stream && wire.StreamOptions != nil {
		return contract.Request{}, false, gatewayError(contract.ErrorInvalidRequest, "stream_options requires stream=true", http.StatusBadRequest, false, nil)
	}
	estimatedInput := requestBytes
	return contract.Request{
		ID: id, Operation: contract.OperationChat, PublicModel: wire.Model, Stream: wire.Stream,
		MaxOutputTokens: maximum, EstimatedUsage: contract.Usage{InputTokens: estimatedInput, OutputTokens: maximum * int64(n), TotalTokens: estimatedInput + maximum*int64(n), Estimated: true},
		Chat: chat,
	}, includeUsage, nil
}

func convertMessage(message chatMessage) (contract.Message, error) {
	if message.Role != "system" && message.Role != "developer" && message.Role != "user" && message.Role != "assistant" && message.Role != "tool" {
		return contract.Message{}, fmt.Errorf("unsupported role %q", message.Role)
	}
	converted := contract.Message{Role: message.Role, Name: message.Name, ToolCallID: message.ToolCallID}
	content := bytes.TrimSpace(message.Content)
	if len(content) != 0 && !bytes.Equal(content, []byte("null")) {
		if content[0] == '"' {
			var text string
			if err := json.Unmarshal(content, &text); err != nil {
				return contract.Message{}, err
			}
			converted.Content = []contract.ContentPart{{Type: "text", Text: text}}
		} else {
			var parts []contentPart
			if err := decodeStrict(content, &parts); err != nil {
				return contract.Message{}, err
			}
			for _, part := range parts {
				canonical, err := convertContentPart(part)
				if err != nil {
					return contract.Message{}, err
				}
				converted.Content = append(converted.Content, canonical)
			}
		}
	}
	for _, call := range message.ToolCalls {
		if call.ID == "" || call.Type != "function" || call.Function.Name == "" {
			return contract.Message{}, errors.New("invalid tool call")
		}
		converted.ToolCalls = append(converted.ToolCalls, contract.ToolCall{ID: call.ID, Type: call.Type, Function: contract.FunctionCall{Name: call.Function.Name, Arguments: call.Function.Arguments}})
	}
	if message.Role == "tool" && message.ToolCallID == "" {
		return contract.Message{}, errors.New("tool message requires tool_call_id")
	}
	if len(converted.Content) == 0 && len(converted.ToolCalls) == 0 {
		return contract.Message{}, errors.New("message requires content or tool calls")
	}
	return converted, nil
}

func convertContentPart(part contentPart) (contract.ContentPart, error) {
	switch part.Type {
	case "text":
		if part.Text == "" {
			return contract.ContentPart{}, errors.New("text content cannot be empty")
		}
		return contract.ContentPart{Type: "text", Text: part.Text}, nil
	case "image_url":
		if part.ImageURL == nil || part.ImageURL.URL == "" {
			return contract.ContentPart{}, errors.New("image_url content requires a URL")
		}
		return contract.ContentPart{Type: "image_url", ImageURL: &contract.ImageURL{URL: part.ImageURL.URL, Detail: part.ImageURL.Detail}}, nil
	case "input_audio":
		if part.InputAudio == nil || part.InputAudio.Data == "" || part.InputAudio.Format == "" {
			return contract.ContentPart{}, errors.New("input_audio requires data and format")
		}
		return contract.ContentPart{Type: "input_audio", Audio: &contract.InputAudio{Data: part.InputAudio.Data, Format: part.InputAudio.Format}}, nil
	default:
		return contract.ContentPart{}, fmt.Errorf("unsupported content type %q", part.Type)
	}
}

func convertToolChoice(raw json.RawMessage) (*contract.ToolChoice, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil, nil
	}
	if bytes.TrimSpace(raw)[0] == '"' {
		var mode string
		if err := json.Unmarshal(raw, &mode); err != nil {
			return nil, err
		}
		if mode != "none" && mode != "auto" && mode != "required" {
			return nil, fmt.Errorf("unsupported mode %q", mode)
		}
		return &contract.ToolChoice{Mode: mode}, nil
	}
	var value struct {
		Type     string `json:"type"`
		Function struct {
			Name string `json:"name"`
		} `json:"function"`
	}
	if err := decodeStrict(raw, &value); err != nil {
		return nil, err
	}
	if value.Type != "function" || value.Function.Name == "" {
		return nil, errors.New("named tool choice requires a function name")
	}
	return &contract.ToolChoice{Mode: "function", FunctionName: value.Function.Name}, nil
}

func convertResponseFormat(value responseFormat) (*contract.ResponseFormat, error) {
	switch value.Type {
	case "text", "json_object":
		return &contract.ResponseFormat{Type: value.Type}, nil
	case "json_schema":
		if value.JSONSchema == nil || value.JSONSchema.Name == "" || len(value.JSONSchema.Schema) == 0 {
			return nil, errors.New("json_schema response requires name and schema")
		}
		return &contract.ResponseFormat{Type: value.Type, JSONSchema: &contract.JSONSchema{Name: value.JSONSchema.Name, Description: value.JSONSchema.Description, Schema: append([]byte(nil), value.JSONSchema.Schema...), Strict: value.JSONSchema.Strict}}, nil
	default:
		return nil, fmt.Errorf("unsupported response format %q", value.Type)
	}
}

func convertStop(raw json.RawMessage) ([]string, error) {
	if len(bytes.TrimSpace(raw)) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil, nil
	}
	if bytes.TrimSpace(raw)[0] == '"' {
		var value string
		if err := json.Unmarshal(raw, &value); err != nil {
			return nil, err
		}
		if value == "" {
			return nil, errors.New("stop value cannot be empty")
		}
		return []string{value}, nil
	}
	var values []string
	if err := json.Unmarshal(raw, &values); err != nil {
		return nil, err
	}
	if len(values) > 4 {
		return nil, errors.New("at most four stop values are supported")
	}
	for _, value := range values {
		if value == "" {
			return nil, errors.New("stop value cannot be empty")
		}
	}
	return values, nil
}

func (h *Handler) streamChat(response http.ResponseWriter, request *http.Request, principal contract.Principal, canonical contract.Request, includeUsage bool) {
	flusher, ok := response.(http.Flusher)
	if !ok {
		writeError(response, gatewayError(contract.ErrorInternal, "streaming is unavailable", http.StatusInternalServerError, false, nil), canonical.ID)
		return
	}
	stream, err := h.engine.Stream(request.Context(), principal, canonical)
	if err != nil {
		writeError(response, err, canonical.ID)
		return
	}
	defer stream.Close(request.Context())
	response.Header().Set("Content-Type", "text/event-stream")
	response.Header().Set("Cache-Control", "no-cache")
	response.Header().Set("X-Accel-Buffering", "no")
	response.Header().Set("X-Request-ID", string(canonical.ID))
	for {
		event, nextErr := stream.Next(request.Context())
		if nextErr != nil {
			if errors.Is(nextErr, io.EOF) {
				if _, writeErr := io.WriteString(response, "data: [DONE]\n\n"); writeErr != nil {
					return
				}
				flusher.Flush()
				return
			}
			encoded := encodeErrorEvent(nextErr, canonical.ID)
			if _, writeErr := fmt.Fprintf(response, "data: %s\n\n", encoded); writeErr != nil {
				return
			}
			if _, writeErr := io.WriteString(response, "data: [DONE]\n\n"); writeErr != nil {
				return
			}
			flusher.Flush()
			return
		}
		if event.Usage != nil && !includeUsage && event.Chat == nil {
			if event.Terminal {
				if _, writeErr := io.WriteString(response, "data: [DONE]\n\n"); writeErr != nil {
					return
				}
				flusher.Flush()
				return
			}
			continue
		}
		encoded, encodeErr := encodeChatChunk(canonical, event, includeUsage)
		if encodeErr != nil {
			if _, writeErr := fmt.Fprintf(response, "data: %s\n\n", encodeErrorEvent(encodeErr, canonical.ID)); writeErr != nil {
				return
			}
			if _, writeErr := io.WriteString(response, "data: [DONE]\n\n"); writeErr != nil {
				return
			}
			flusher.Flush()
			return
		}
		if _, writeErr := fmt.Fprintf(response, "data: %s\n\n", encoded); writeErr != nil {
			return
		}
		flusher.Flush()
		if event.Terminal {
			if _, writeErr := io.WriteString(response, "data: [DONE]\n\n"); writeErr != nil {
				return
			}
			flusher.Flush()
			return
		}
	}
}
