package openai

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"

	"github.com/daptin/llmgateway/contract"
	"github.com/daptin/llmgateway/internal/jsonx"
)

type messagesRequest struct {
	Model         string              `json:"model"`
	Messages      []messagesMessage   `json:"messages"`
	System        json.RawMessage     `json:"system,omitempty"`
	MaxTokens     int64               `json:"max_tokens"`
	StopSequences []string            `json:"stop_sequences,omitempty"`
	Stream        bool                `json:"stream,omitempty"`
	Temperature   *float64            `json:"temperature,omitempty"`
	TopP          *float64            `json:"top_p,omitempty"`
	Tools         []messagesTool      `json:"tools,omitempty"`
	ToolChoice    *messagesToolChoice `json:"tool_choice,omitempty"`
	Metadata      *struct {
		UserID string `json:"user_id,omitempty"`
	} `json:"metadata,omitempty"`
}

type messagesMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

type messagesBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text,omitempty"`
	ID        string          `json:"id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Input     json.RawMessage `json:"input,omitempty"`
	ToolUseID string          `json:"tool_use_id,omitempty"`
	Content   json.RawMessage `json:"content,omitempty"`
	IsError   bool            `json:"is_error,omitempty"`
	Source    *struct {
		Type      string `json:"type"`
		MediaType string `json:"media_type,omitempty"`
		Data      string `json:"data,omitempty"`
		URL       string `json:"url,omitempty"`
	} `json:"source,omitempty"`
}

type messagesTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema"`
}

type messagesToolChoice struct {
	Type                   string `json:"type"`
	Name                   string `json:"name,omitempty"`
	DisableParallelToolUse *bool  `json:"disable_parallel_tool_use,omitempty"`
}

func (h *Handler) messages(response http.ResponseWriter, request *http.Request) {
	id, err := h.requestID(request)
	if err != nil {
		writeMessagesError(response, err, "")
		return
	}
	principal, err := h.authenticate(request)
	if err != nil {
		writeMessagesError(response, err, id)
		return
	}
	body, err := readJSONBody(response, request, h.maxBody)
	if err != nil {
		writeMessagesError(response, err, id)
		return
	}
	var wire messagesRequest
	if err := decodeStrict(body, &wire); err != nil {
		writeMessagesError(response, gatewayError(contract.ErrorInvalidRequest, "invalid messages request", http.StatusBadRequest, false, err), id)
		return
	}
	canonical, err := canonicalMessages(id, wire, int64(len(body)))
	if err != nil {
		writeMessagesError(response, err, id)
		return
	}
	if canonical.Stream {
		h.streamMessages(response, request, principal, canonical)
		return
	}
	result, err := h.engine.Invoke(request.Context(), principal, canonical)
	if err != nil {
		writeMessagesError(response, err, id)
		return
	}
	encoded, err := encodeMessagesResponse(result)
	if err != nil {
		writeMessagesError(response, err, id)
		return
	}
	writeJSON(response, http.StatusOK, id, encoded)
}

func canonicalMessages(id contract.ID, wire messagesRequest, requestBytes int64) (contract.Request, error) {
	if strings.TrimSpace(wire.Model) == "" || len(wire.Messages) == 0 || wire.MaxTokens < 1 {
		return contract.Request{}, gatewayError(contract.ErrorInvalidRequest, "model, messages, and positive max_tokens are required", http.StatusBadRequest, false, nil)
	}
	chat := &contract.ChatRequest{N: 1, MaxCompletionTokens: wire.MaxTokens, Temperature: wire.Temperature, TopP: wire.TopP, Stop: append([]string(nil), wire.StopSequences...)}
	if wire.Metadata != nil {
		chat.User = wire.Metadata.UserID
	}
	if len(bytes.TrimSpace(wire.System)) != 0 {
		parts, err := messagesContent(wire.System)
		if err != nil {
			return contract.Request{}, gatewayError(contract.ErrorInvalidRequest, "invalid system content", http.StatusBadRequest, false, err)
		}
		chat.Messages = append(chat.Messages, contract.Message{Role: "system", Content: parts})
	}
	for index, message := range wire.Messages {
		converted, err := convertMessagesMessage(message)
		if err != nil {
			return contract.Request{}, gatewayError(contract.ErrorInvalidRequest, fmt.Sprintf("invalid message at index %d", index), http.StatusBadRequest, false, err)
		}
		chat.Messages = append(chat.Messages, converted...)
	}
	for _, tool := range wire.Tools {
		if tool.Name == "" || !contract.ValidJSONObject(tool.InputSchema) {
			return contract.Request{}, gatewayError(contract.ErrorInvalidRequest, "invalid tool", http.StatusBadRequest, false, nil)
		}
		chat.Tools = append(chat.Tools, contract.Tool{Type: "function", Function: contract.FunctionDefinition{Name: tool.Name, Description: tool.Description, Parameters: append([]byte(nil), tool.InputSchema...)}})
	}
	if wire.ToolChoice != nil {
		choice := &contract.ToolChoice{}
		switch wire.ToolChoice.Type {
		case "auto":
			choice.Mode = "auto"
		case "any":
			choice.Mode = "required"
		case "none":
			choice.Mode = "none"
		case "tool":
			choice.Mode, choice.FunctionName = "function", wire.ToolChoice.Name
		default:
			return contract.Request{}, gatewayError(contract.ErrorInvalidRequest, "invalid tool_choice", http.StatusBadRequest, false, nil)
		}
		chat.ToolChoice = choice
		if wire.ToolChoice.DisableParallelToolUse != nil {
			parallel := !*wire.ToolChoice.DisableParallelToolUse
			chat.ParallelToolCalls = &parallel
		}
	}
	canonical := contract.Request{ID: id, Operation: contract.OperationChat, PublicModel: strings.TrimSpace(wire.Model), Stream: wire.Stream,
		MaxOutputTokens: wire.MaxTokens, EstimatedUsage: contract.Usage{InputTokens: requestBytes, OutputTokens: wire.MaxTokens,
			TotalTokens: requestBytes + wire.MaxTokens, Estimated: true}, Chat: chat}
	if err := canonical.Validate(); err != nil {
		return contract.Request{}, gatewayError(contract.ErrorInvalidRequest, "invalid messages request", http.StatusBadRequest, false, err)
	}
	return canonical, nil
}

func convertMessagesMessage(message messagesMessage) ([]contract.Message, error) {
	if message.Role != "user" && message.Role != "assistant" {
		return nil, errors.New("role must be user or assistant")
	}
	trimmed := bytes.TrimSpace(message.Content)
	if len(trimmed) == 0 {
		return nil, errors.New("content is required")
	}
	if trimmed[0] == '"' {
		parts, err := messagesContent(trimmed)
		return []contract.Message{{Role: message.Role, Content: parts}}, err
	}
	var blocks []messagesBlock
	if err := jsonx.DecodeOne(bytes.NewReader(trimmed), &blocks); err != nil || len(blocks) == 0 {
		return nil, errors.New("content blocks are invalid")
	}
	result := make([]contract.Message, 0, len(blocks))
	current := contract.Message{Role: message.Role}
	flush := func() {
		if len(current.Content) != 0 || len(current.ToolCalls) != 0 {
			result = append(result, current)
			current = contract.Message{Role: message.Role}
		}
	}
	for _, block := range blocks {
		switch block.Type {
		case "text":
			if block.Text == "" {
				return nil, errors.New("text block is empty")
			}
			current.Content = append(current.Content, contract.ContentPart{Type: "text", Text: block.Text})
		case "image":
			if message.Role != "user" || block.Source == nil {
				return nil, errors.New("image block is invalid")
			}
			url := block.Source.URL
			if block.Source.Type == "base64" {
				if block.Source.MediaType == "" || block.Source.Data == "" {
					return nil, errors.New("base64 image source is invalid")
				}
				if _, err := base64.StdEncoding.DecodeString(block.Source.Data); err != nil {
					return nil, errors.New("base64 image data is invalid")
				}
				url = "data:" + block.Source.MediaType + ";base64," + block.Source.Data
			}
			if url == "" {
				return nil, errors.New("image source is invalid")
			}
			current.Content = append(current.Content, contract.ContentPart{Type: "image_url", ImageURL: &contract.ImageURL{URL: url}})
		case "tool_use":
			if message.Role != "assistant" || block.ID == "" || block.Name == "" || !contract.ValidJSONObject(block.Input) {
				return nil, errors.New("tool_use block is invalid")
			}
			current.ToolCalls = append(current.ToolCalls, contract.ToolCall{ID: block.ID, Type: "function", Function: contract.FunctionCall{Name: block.Name, Arguments: string(block.Input)}})
		case "tool_result":
			if message.Role != "user" || block.ToolUseID == "" {
				return nil, errors.New("tool_result block is invalid")
			}
			flush()
			parts, err := messagesContent(block.Content)
			if err != nil {
				return nil, err
			}
			result = append(result, contract.Message{Role: "tool", ToolCallID: block.ToolUseID, Content: parts})
		default:
			return nil, fmt.Errorf("unsupported content block %q", block.Type)
		}
	}
	flush()
	if len(result) == 0 {
		return nil, errors.New("message content is empty")
	}
	return result, nil
}

func messagesContent(raw json.RawMessage) ([]contract.ContentPart, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return nil, errors.New("content is required")
	}
	if trimmed[0] == '"' {
		var text string
		if err := json.Unmarshal(trimmed, &text); err != nil || text == "" {
			return nil, errors.New("content text is invalid")
		}
		return []contract.ContentPart{{Type: "text", Text: text}}, nil
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := jsonx.DecodeOne(bytes.NewReader(trimmed), &blocks); err != nil || len(blocks) == 0 {
		return nil, errors.New("content blocks are invalid")
	}
	parts := make([]contract.ContentPart, 0, len(blocks))
	for _, block := range blocks {
		if block.Type != "text" || block.Text == "" {
			return nil, errors.New("only non-empty text content is supported here")
		}
		parts = append(parts, contract.ContentPart{Type: "text", Text: block.Text})
	}
	return parts, nil
}

func encodeMessagesResponse(response contract.Response) (map[string]any, error) {
	if response.Chat == nil || len(response.Chat.Choices) != 1 {
		return nil, gatewayError(contract.ErrorProvider, "provider returned an invalid messages response", http.StatusBadGateway, false, nil)
	}
	choice := response.Chat.Choices[0]
	content := make([]map[string]any, 0, len(choice.Message.Content)+len(choice.Message.ToolCalls))
	for _, part := range choice.Message.Content {
		if part.Type != "text" {
			return nil, gatewayError(contract.ErrorProvider, "provider returned unsupported messages content", http.StatusBadGateway, false, nil)
		}
		content = append(content, map[string]any{"type": "text", "text": part.Text})
	}
	for _, call := range choice.Message.ToolCalls {
		var input any
		if err := json.Unmarshal([]byte(call.Function.Arguments), &input); err != nil {
			return nil, gatewayError(contract.ErrorProvider, "provider returned invalid tool input", http.StatusBadGateway, false, err)
		}
		content = append(content, map[string]any{"type": "tool_use", "id": call.ID, "name": call.Function.Name, "input": input})
	}
	return map[string]any{"id": response.Chat.ID, "type": "message", "role": "assistant", "content": content, "model": response.Model,
		"stop_reason": messagesStopReason(choice.FinishReason), "stop_sequence": nil,
		"usage": map[string]any{"input_tokens": response.Usage.InputTokens, "output_tokens": response.Usage.OutputTokens,
			"cache_read_input_tokens": response.Usage.CacheReadTokens, "cache_creation_input_tokens": response.Usage.CacheWriteTokens}}, nil
}

func messagesStopReason(reason string) any {
	switch reason {
	case "stop":
		return "end_turn"
	case "length":
		return "max_tokens"
	case "tool_calls":
		return "tool_use"
	case "content_filter":
		return "refusal"
	case "":
		return nil
	default:
		return reason
	}
}

func writeMessagesError(response http.ResponseWriter, err error, requestID contract.ID) {
	public := publicError(err)
	body := map[string]any{"type": "error", "error": map[string]any{"type": messagesErrorType(public.Code), "message": public.Message}}
	writeJSON(response, public.HTTPStatus, requestID, body)
}

func messagesErrorType(code contract.ErrorCode) string {
	switch code {
	case contract.ErrorAuthentication:
		return "authentication_error"
	case contract.ErrorPermission:
		return "permission_error"
	case contract.ErrorInvalidRequest, contract.ErrorModelNotFound:
		return "invalid_request_error"
	case contract.ErrorRateLimit, contract.ErrorInsufficientQuota:
		return "rate_limit_error"
	case contract.ErrorUnavailable, contract.ErrorTimeout, contract.ErrorProvider:
		return "api_error"
	default:
		return "internal_server_error"
	}
}

func (h *Handler) streamMessages(response http.ResponseWriter, request *http.Request, principal contract.Principal, canonical contract.Request) {
	session, err := h.startSSE(response, request, principal, canonical)
	if err != nil {
		if session == nil {
			writeMessagesError(response, err, canonical.ID)
		} else {
			defer session.Close()
			writeMessagesStreamError(response, session.flusher, err)
		}
		return
	}
	defer session.Close()
	started, textOpen, nextBlock, finish := false, false, 0, ""
	toolBlocks := make(map[int]int)
	for {
		event, nextErr := session.Next()
		if nextErr != nil {
			if errors.Is(nextErr, io.EOF) {
				return
			}
			writeMessagesStreamError(response, session.flusher, nextErr)
			return
		}
		if !started {
			started = true
			messageID := string(canonical.ID)
			inputTokens := int64(0)
			if event.Chat != nil && event.Chat.ID != "" {
				messageID = event.Chat.ID
			}
			if event.Usage != nil {
				inputTokens = event.Usage.InputTokens
			}
			if !writeMessagesEvent(response, session.flusher, "message_start", map[string]any{"type": "message_start", "message": map[string]any{
				"id": messageID, "type": "message", "role": "assistant", "content": []any{}, "model": canonical.PublicModel,
				"stop_reason": nil, "stop_sequence": nil, "usage": map[string]any{"input_tokens": inputTokens, "output_tokens": int64(0)}}}) {
				return
			}
		}
		if event.Chat != nil {
			if event.Chat.FinishReason != "" {
				finish = event.Chat.FinishReason
			}
			if event.Chat.Content != "" {
				if !textOpen {
					if !writeMessagesEvent(response, session.flusher, "content_block_start", map[string]any{"type": "content_block_start", "index": nextBlock, "content_block": map[string]any{"type": "text", "text": ""}}) {
						return
					}
					textOpen = true
				}
				if !writeMessagesEvent(response, session.flusher, "content_block_delta", map[string]any{"type": "content_block_delta", "index": nextBlock, "delta": map[string]any{"type": "text_delta", "text": event.Chat.Content}}) {
					return
				}
			}
			for _, call := range event.Chat.ToolCalls {
				block, exists := toolBlocks[call.Index]
				if !exists {
					if textOpen {
						if !writeMessagesEvent(response, session.flusher, "content_block_stop", map[string]any{"type": "content_block_stop", "index": nextBlock}) {
							return
						}
						textOpen = false
						nextBlock++
					}
					block = nextBlock
					nextBlock++
					toolBlocks[call.Index] = block
					if !writeMessagesEvent(response, session.flusher, "content_block_start", map[string]any{"type": "content_block_start", "index": block, "content_block": map[string]any{"type": "tool_use", "id": call.ID, "name": call.Function.Name, "input": map[string]any{}}}) {
						return
					}
				}
				if call.Function.Arguments != "" {
					if !writeMessagesEvent(response, session.flusher, "content_block_delta", map[string]any{"type": "content_block_delta", "index": block, "delta": map[string]any{"type": "input_json_delta", "partial_json": call.Function.Arguments}}) {
						return
					}
				}
			}
		}
		if event.Terminal {
			if textOpen {
				if !writeMessagesEvent(response, session.flusher, "content_block_stop", map[string]any{"type": "content_block_stop", "index": nextBlock}) {
					return
				}
			}
			blocks := make([]int, 0, len(toolBlocks))
			for _, block := range toolBlocks {
				blocks = append(blocks, block)
			}
			sort.Ints(blocks)
			for _, block := range blocks {
				if !writeMessagesEvent(response, session.flusher, "content_block_stop", map[string]any{"type": "content_block_stop", "index": block}) {
					return
				}
			}
			usage := contract.Usage{}
			if event.Usage != nil {
				usage = *event.Usage
			}
			if !writeMessagesEvent(response, session.flusher, "message_delta", map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": messagesStopReason(finish), "stop_sequence": nil}, "usage": map[string]any{"output_tokens": usage.OutputTokens}}) {
				return
			}
			_ = writeMessagesEvent(response, session.flusher, "message_stop", map[string]any{"type": "message_stop"})
			return
		}
	}
}

func writeMessagesEvent(response io.Writer, flusher http.Flusher, event string, value any) bool {
	payload, err := json.Marshal(value)
	if err != nil {
		return false
	}
	if _, err = fmt.Fprintf(response, "event: %s\ndata: %s\n\n", event, payload); err != nil {
		return false
	}
	flusher.Flush()
	return true
}

func writeMessagesStreamError(response io.Writer, flusher http.Flusher, err error) {
	public := publicError(err)
	writeMessagesEvent(response, flusher, "error", map[string]any{"type": "error", "error": map[string]any{"type": messagesErrorType(public.Code), "message": public.Message}})
}
