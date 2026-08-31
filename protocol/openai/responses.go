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

type responsesRequest struct {
	Model              string          `json:"model"`
	Input              json.RawMessage `json:"input"`
	Instructions       string          `json:"instructions,omitempty"`
	Stream             bool            `json:"stream,omitempty"`
	Tools              []responseTool  `json:"tools,omitempty"`
	ToolChoice         json.RawMessage `json:"tool_choice,omitempty"`
	Text               *responseText   `json:"text,omitempty"`
	MaxOutputTokens    *int64          `json:"max_output_tokens,omitempty"`
	Store              *bool           `json:"store,omitempty"`
	PreviousResponseID *string         `json:"previous_response_id,omitempty"`
}

type responseText struct {
	Format *responseTextFormat `json:"format,omitempty"`
}

type responseTextFormat struct {
	Type        string          `json:"type"`
	Name        string          `json:"name,omitempty"`
	Description string          `json:"description,omitempty"`
	Schema      json.RawMessage `json:"schema,omitempty"`
	Strict      *bool           `json:"strict,omitempty"`
}

type responseInputItem struct {
	Type      string          `json:"type,omitempty"`
	Role      string          `json:"role,omitempty"`
	Content   json.RawMessage `json:"content,omitempty"`
	CallID    string          `json:"call_id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Arguments string          `json:"arguments,omitempty"`
	Output    string          `json:"output,omitempty"`
}

type responseTool struct {
	Type        string          `json:"type"`
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters"`
	Strict      *bool           `json:"strict,omitempty"`
}

type responseContentPart struct {
	Type     string `json:"type"`
	Text     string `json:"text,omitempty"`
	ImageURL string `json:"image_url,omitempty"`
	Detail   string `json:"detail,omitempty"`
}

func (h *Handler) responses(response http.ResponseWriter, request *http.Request) {
	id, err := h.requestID(request)
	if err != nil {
		writeError(response, err, "")
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
	var wire responsesRequest
	if err := decodeStrict(body, &wire); err != nil {
		writeError(response, gatewayError(contract.ErrorInvalidRequest, "invalid responses request", http.StatusBadRequest, false, err), id)
		return
	}
	canonical, err := h.canonicalResponse(id, wire, int64(len(body)))
	if err != nil {
		writeError(response, err, id)
		return
	}
	if canonical.Stream {
		h.streamResponse(response, request, principal, canonical)
		return
	}
	result, err := h.engine.Invoke(request.Context(), principal, canonical)
	if err != nil {
		writeError(response, err, id)
		return
	}
	encoded, err := encodeResponse(result)
	if err != nil {
		writeError(response, err, id)
		return
	}
	writeJSON(response, http.StatusOK, id, encoded)
}

func (h *Handler) canonicalResponse(id contract.ID, wire responsesRequest, requestBytes int64) (contract.Request, error) {
	if strings.TrimSpace(wire.Model) == "" || len(bytes.TrimSpace(wire.Input)) == 0 {
		return contract.Request{}, gatewayError(contract.ErrorInvalidRequest, "model and input are required", http.StatusBadRequest, false, nil)
	}
	if wire.Store != nil || wire.PreviousResponseID != nil {
		return contract.Request{}, gatewayError(contract.ErrorInvalidRequest, "stateful responses are not supported", http.StatusBadRequest, false, nil)
	}
	var maximum int64
	if wire.MaxOutputTokens != nil {
		maximum = *wire.MaxOutputTokens
	}
	if wire.MaxOutputTokens != nil && maximum < 1 {
		return contract.Request{}, gatewayError(contract.ErrorInvalidRequest, "max_output_tokens must be positive", http.StatusBadRequest, false, nil)
	}
	input, err := convertResponseInput(wire.Input)
	if err != nil {
		return contract.Request{}, gatewayError(contract.ErrorInvalidRequest, "invalid response input", http.StatusBadRequest, false, err)
	}
	canonical := &contract.ResponsesRequest{Instructions: wire.Instructions, Input: input}
	for _, tool := range wire.Tools {
		if tool.Type != "function" || tool.Name == "" || len(tool.Parameters) == 0 {
			return contract.Request{}, gatewayError(contract.ErrorInvalidRequest, "invalid function tool", http.StatusBadRequest, false, nil)
		}
		canonical.Tools = append(canonical.Tools, contract.Tool{Type: tool.Type, Function: contract.FunctionDefinition{Name: tool.Name, Description: tool.Description, Parameters: append([]byte(nil), tool.Parameters...), Strict: tool.Strict}})
	}
	canonical.ToolChoice, err = convertResponseToolChoice(wire.ToolChoice)
	if err != nil {
		return contract.Request{}, gatewayError(contract.ErrorInvalidRequest, "invalid tool_choice", http.StatusBadRequest, false, err)
	}
	if wire.Text != nil && wire.Text.Format != nil {
		canonical.TextFormat, err = convertResponseTextFormat(*wire.Text.Format)
		if err != nil {
			return contract.Request{}, gatewayError(contract.ErrorInvalidRequest, "invalid text format", http.StatusBadRequest, false, err)
		}
	}
	return contract.Request{
		ID: id, Operation: contract.OperationResponses, PublicModel: wire.Model, Stream: wire.Stream, MaxOutputTokens: maximum,
		EstimatedUsage: contract.Usage{InputTokens: requestBytes, OutputTokens: maximum, TotalTokens: requestBytes + maximum, Estimated: true},
		Responses:      canonical,
	}, nil
}

func convertResponseToolChoice(raw json.RawMessage) (*contract.ToolChoice, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return nil, nil
	}
	if trimmed[0] == '"' {
		return convertToolChoice(raw)
	}
	var value struct {
		Type string `json:"type"`
		Name string `json:"name"`
	}
	if err := decodeStrict(raw, &value); err != nil {
		return nil, err
	}
	if value.Type != "function" || value.Name == "" {
		return nil, errors.New("named response tool choice requires a function name")
	}
	return &contract.ToolChoice{Mode: "function", FunctionName: value.Name}, nil
}

func convertResponseInput(raw json.RawMessage) ([]contract.ResponseInputItem, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return nil, errors.New("input is required")
	}
	if trimmed[0] == '"' {
		var text string
		if err := json.Unmarshal(trimmed, &text); err != nil || text == "" {
			return nil, errors.New("input text cannot be empty")
		}
		return []contract.ResponseInputItem{{Type: "message", Role: "user", Content: []contract.ContentPart{{Type: "input_text", Text: text}}}}, nil
	}
	var items []responseInputItem
	if err := decodeStrict(trimmed, &items); err != nil || len(items) == 0 {
		return nil, errors.New("input must be a string or non-empty item array")
	}
	result := make([]contract.ResponseInputItem, 0, len(items))
	for _, item := range items {
		converted, err := convertResponseInputItem(item)
		if err != nil {
			return nil, err
		}
		result = append(result, converted)
	}
	return result, nil
}

func convertResponseInputItem(item responseInputItem) (contract.ResponseInputItem, error) {
	switch item.Type {
	case "", "message":
		if item.Role == "" {
			item.Role = "user"
		}
		if item.Role != "user" && item.Role != "assistant" && item.Role != "system" && item.Role != "developer" {
			return contract.ResponseInputItem{}, fmt.Errorf("invalid message role %q", item.Role)
		}
		content, err := convertResponseContent(item.Content)
		if err != nil {
			return contract.ResponseInputItem{}, err
		}
		return contract.ResponseInputItem{Type: "message", Role: item.Role, Content: content}, nil
	case "function_call":
		if item.CallID == "" || item.Name == "" || item.Arguments == "" {
			return contract.ResponseInputItem{}, errors.New("function_call requires call_id, name, and arguments")
		}
		return contract.ResponseInputItem{Type: item.Type, CallID: item.CallID, Name: item.Name, Arguments: item.Arguments}, nil
	case "function_call_output":
		if item.CallID == "" || item.Output == "" {
			return contract.ResponseInputItem{}, errors.New("function_call_output requires call_id and output")
		}
		return contract.ResponseInputItem{Type: item.Type, CallID: item.CallID, Output: item.Output}, nil
	default:
		return contract.ResponseInputItem{}, fmt.Errorf("unsupported input item type %q", item.Type)
	}
}

func convertResponseContent(raw json.RawMessage) ([]contract.ContentPart, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return nil, errors.New("message content is required")
	}
	if trimmed[0] == '"' {
		var text string
		if err := json.Unmarshal(trimmed, &text); err != nil || text == "" {
			return nil, errors.New("message content cannot be empty")
		}
		return []contract.ContentPart{{Type: "input_text", Text: text}}, nil
	}
	var parts []responseContentPart
	if err := decodeStrict(trimmed, &parts); err != nil || len(parts) == 0 {
		return nil, errors.New("message content must be text or a non-empty content array")
	}
	result := make([]contract.ContentPart, 0, len(parts))
	for _, part := range parts {
		switch part.Type {
		case "input_text":
			if part.Text == "" {
				return nil, errors.New("input_text cannot be empty")
			}
			result = append(result, contract.ContentPart{Type: part.Type, Text: part.Text})
		case "input_image":
			if part.ImageURL == "" {
				return nil, errors.New("input_image requires image_url")
			}
			result = append(result, contract.ContentPart{Type: part.Type, ImageURL: &contract.ImageURL{URL: part.ImageURL, Detail: part.Detail}})
		default:
			return nil, fmt.Errorf("unsupported response content type %q", part.Type)
		}
	}
	return result, nil
}

func convertResponseTextFormat(format responseTextFormat) (*contract.ResponseFormat, error) {
	switch format.Type {
	case "text", "json_object":
		return &contract.ResponseFormat{Type: format.Type}, nil
	case "json_schema":
		if format.Name == "" || len(format.Schema) == 0 {
			return nil, errors.New("json_schema requires name and schema")
		}
		return &contract.ResponseFormat{Type: format.Type, JSONSchema: &contract.JSONSchema{Name: format.Name, Description: format.Description, Schema: append([]byte(nil), format.Schema...), Strict: format.Strict}}, nil
	default:
		return nil, fmt.Errorf("unsupported text format %q", format.Type)
	}
}

func encodeResponse(response contract.Response) (map[string]any, error) {
	if response.Responses == nil {
		return nil, gatewayError(contract.ErrorProvider, "provider returned no responses response", http.StatusBadGateway, false, nil)
	}
	output := make([]map[string]any, 0, len(response.Responses.Output))
	for _, item := range response.Responses.Output {
		encoded, err := encodeResponseOutput(item)
		if err != nil {
			return nil, err
		}
		output = append(output, encoded)
	}
	return map[string]any{
		"id": response.Responses.ID, "object": "response", "status": response.Responses.Status,
		"model": response.Model, "output": output,
		"usage": map[string]any{"input_tokens": response.Usage.InputTokens, "output_tokens": response.Usage.OutputTokens, "total_tokens": response.Usage.TotalTokens},
	}, nil
}

func encodeResponseOutput(item contract.ResponseOutputItem) (map[string]any, error) {
	encoded := map[string]any{"type": item.Type, "id": item.ID, "status": item.Status}
	switch item.Type {
	case "message":
		encoded["role"] = item.Role
		content := make([]map[string]any, 0, len(item.Content))
		for _, part := range item.Content {
			if part.Type != "output_text" {
				return nil, gatewayError(contract.ErrorProvider, "provider returned unsupported response content", http.StatusBadGateway, false, nil)
			}
			content = append(content, map[string]any{"type": part.Type, "text": part.Text, "annotations": []any{}})
		}
		encoded["content"] = content
	case "function_call":
		encoded["call_id"] = item.CallID
		encoded["name"] = item.Name
		encoded["arguments"] = item.Arguments
	case "reasoning":
		summary := make([]map[string]any, 0, len(item.Summary))
		for _, part := range item.Summary {
			if part.Type != "summary_text" {
				return nil, gatewayError(contract.ErrorProvider, "provider returned unsupported reasoning content", http.StatusBadGateway, false, nil)
			}
			summary = append(summary, map[string]any{"type": part.Type, "text": part.Text})
		}
		encoded["summary"] = summary
	default:
		return nil, gatewayError(contract.ErrorProvider, "provider returned unsupported response item", http.StatusBadGateway, false, nil)
	}
	return encoded, nil
}

func (h *Handler) streamResponse(response http.ResponseWriter, request *http.Request, principal contract.Principal, canonical contract.Request) {
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
				return
			}
			if writeResponseSSE(response, "error", encodeErrorEvent(nextErr, canonical.ID)) != nil {
				return
			}
			flusher.Flush()
			return
		}
		name, data, encodeErr := encodeResponseEvent(canonical, event)
		if encodeErr != nil {
			_ = writeResponseSSE(response, "error", encodeErrorEvent(encodeErr, canonical.ID))
			flusher.Flush()
			return
		}
		if err := writeResponseSSE(response, name, data); err != nil {
			return
		}
		flusher.Flush()
		if event.Terminal {
			return
		}
	}
}

func encodeResponseEvent(request contract.Request, event contract.StreamEvent) (string, []byte, error) {
	if event.Response == nil {
		return "", nil, gatewayError(contract.ErrorProvider, "provider returned an invalid response event", http.StatusBadGateway, false, nil)
	}
	delta := event.Response
	name := event.Type
	if name == "" {
		name = "response.output_text.delta"
	}
	payload := map[string]any{"type": name, "sequence_number": delta.Sequence}
	if delta.ResponseID != "" {
		payload["response_id"] = delta.ResponseID
	}
	if delta.Delta != "" {
		payload["delta"] = delta.Delta
	}
	if delta.Item != nil {
		item, err := encodeResponseOutput(*delta.Item)
		if err != nil {
			return "", nil, err
		}
		payload["item"] = item
	}
	if event.Terminal {
		usage := contract.Usage{}
		if event.Usage != nil {
			usage = *event.Usage
		}
		payload["response"] = map[string]any{
			"id": delta.ResponseID, "object": "response", "status": "completed", "model": request.PublicModel,
			"usage": map[string]any{"input_tokens": usage.InputTokens, "output_tokens": usage.OutputTokens, "total_tokens": usage.TotalTokens},
		}
	}
	encoded, err := json.Marshal(payload)
	return name, encoded, err
}

func writeResponseSSE(response io.Writer, event string, data []byte) error {
	_, err := fmt.Fprintf(response, "event: %s\ndata: %s\n\n", event, data)
	return err
}
