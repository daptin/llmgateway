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
	Model              string             `json:"model"`
	Input              json.RawMessage    `json:"input"`
	Include            []string           `json:"include,omitempty"`
	ClientMetadata     json.RawMessage    `json:"client_metadata,omitempty"`
	Instructions       string             `json:"instructions,omitempty"`
	Stream             bool               `json:"stream,omitempty"`
	Tools              []responseTool     `json:"tools,omitempty"`
	ToolChoice         json.RawMessage    `json:"tool_choice,omitempty"`
	Text               *responseText      `json:"text,omitempty"`
	MaxOutputTokens    *int64             `json:"max_output_tokens,omitempty"`
	Store              *bool              `json:"store,omitempty"`
	Background         *bool              `json:"background,omitempty"`
	PreviousResponseID *string            `json:"previous_response_id,omitempty"`
	Temperature        *float64           `json:"temperature,omitempty"`
	TopP               *float64           `json:"top_p,omitempty"`
	ParallelToolCalls  *bool              `json:"parallel_tool_calls,omitempty"`
	Reasoning          *responseReasoning `json:"reasoning,omitempty"`
	PromptCacheKey     string             `json:"prompt_cache_key,omitempty"`
	SafetyIdentifier   string             `json:"safety_identifier,omitempty"`
	ServiceTier        string             `json:"service_tier,omitempty"`
	Truncation         string             `json:"truncation,omitempty"`
	User               string             `json:"user,omitempty"`
	TopLogprobs        *int               `json:"top_logprobs,omitempty"`
}

type compactResponsesRequest struct {
	Model              string          `json:"model"`
	Input              json.RawMessage `json:"input"`
	Instructions       string          `json:"instructions,omitempty"`
	PreviousResponseID *string         `json:"previous_response_id,omitempty"`
	PromptCacheKey     string          `json:"prompt_cache_key,omitempty"`
	PromptCacheOptions *struct {
		Mode string `json:"mode,omitempty"`
		TTL  string `json:"ttl,omitempty"`
	} `json:"prompt_cache_options,omitempty"`
	PromptCacheRetention string `json:"prompt_cache_retention,omitempty"`
	ServiceTier          string `json:"service_tier,omitempty"`
}

type responseReasoning struct {
	Effort  string `json:"effort"`
	Summary string `json:"summary,omitempty"`
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
	ID               string          `json:"id,omitempty"`
	Type             string          `json:"type,omitempty"`
	Role             string          `json:"role,omitempty"`
	Content          json.RawMessage `json:"content,omitempty"`
	CallID           string          `json:"call_id,omitempty"`
	Name             string          `json:"name,omitempty"`
	Arguments        string          `json:"arguments,omitempty"`
	Output           string          `json:"output,omitempty"`
	EncryptedContent string          `json:"encrypted_content,omitempty"`
}

type responseTool struct {
	Type              string          `json:"type"`
	Name              string          `json:"name,omitempty"`
	Description       string          `json:"description,omitempty"`
	Parameters        json.RawMessage `json:"parameters,omitempty"`
	Strict            *bool           `json:"strict,omitempty"`
	Tools             []responseTool  `json:"tools,omitempty"`
	ExternalWebAccess *bool           `json:"external_web_access,omitempty"`
}

type responseContentPart struct {
	Type     string `json:"type"`
	Text     string `json:"text,omitempty"`
	ImageURL string `json:"image_url,omitempty"`
	Detail   string `json:"detail,omitempty"`
	FileID   string `json:"file_id,omitempty"`
	FileData string `json:"file_data,omitempty"`
	Filename string `json:"filename,omitempty"`
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
		writeError(response, gatewayError(contract.ErrorInvalidRequest, strictJSONErrorMessage("invalid responses request", err), http.StatusBadRequest, false, err), id)
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

func (h *Handler) compactResponses(response http.ResponseWriter, request *http.Request) {
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
	var wire compactResponsesRequest
	if err := decodeStrict(body, &wire); err != nil {
		writeError(response, gatewayError(contract.ErrorInvalidRequest, "invalid response compaction request", http.StatusBadRequest, false, err), id)
		return
	}
	canonical, err := canonicalResponseCompaction(id, wire, int64(len(body)))
	if err != nil {
		writeError(response, err, id)
		return
	}
	result, err := h.engine.Invoke(request.Context(), principal, canonical)
	if err != nil {
		writeError(response, err, id)
		return
	}
	encoded, err := encodeCompactedResponse(result)
	if err != nil {
		writeError(response, err, id)
		return
	}
	writeJSON(response, http.StatusOK, id, encoded)
}

func canonicalResponseCompaction(id contract.ID, wire compactResponsesRequest, requestBytes int64) (contract.Request, error) {
	if strings.TrimSpace(wire.Model) == "" || len(bytes.TrimSpace(wire.Input)) == 0 {
		return contract.Request{}, gatewayError(contract.ErrorInvalidRequest, "model and input are required", http.StatusBadRequest, false, nil)
	}
	if wire.PreviousResponseID != nil {
		return contract.Request{}, gatewayError(contract.ErrorInvalidRequest, "stateful response compaction is not supported", http.StatusBadRequest, false, nil)
	}
	input, err := convertResponseInput(wire.Input)
	if err != nil {
		return contract.Request{}, gatewayError(contract.ErrorInvalidRequest, "invalid response compaction input", http.StatusBadRequest, false, err)
	}
	canonical := &contract.ResponsesRequest{Instructions: wire.Instructions, Input: input, PromptCacheKey: wire.PromptCacheKey,
		PromptCacheRetention: wire.PromptCacheRetention, ServiceTier: wire.ServiceTier}
	if wire.PromptCacheOptions != nil {
		canonical.PromptCacheMode = wire.PromptCacheOptions.Mode
		canonical.PromptCacheTTL = wire.PromptCacheOptions.TTL
	}
	request := contract.Request{ID: id, Operation: contract.OperationResponseCompact, PublicModel: wire.Model,
		EstimatedUsage: contract.Usage{InputTokens: requestBytes, TotalTokens: requestBytes, Estimated: true}, Responses: canonical}
	if err := request.Validate(); err != nil {
		return contract.Request{}, gatewayError(contract.ErrorInvalidRequest, "invalid response compaction request", http.StatusBadRequest, false, err)
	}
	return request, nil
}

func (h *Handler) canonicalResponse(id contract.ID, wire responsesRequest, requestBytes int64) (contract.Request, error) {
	if strings.TrimSpace(wire.Model) == "" || len(bytes.TrimSpace(wire.Input)) == 0 {
		return contract.Request{}, gatewayError(contract.ErrorInvalidRequest, "model and input are required", http.StatusBadRequest, false, nil)
	}
	if (wire.Store != nil && *wire.Store) || (wire.Background != nil && *wire.Background) || wire.PreviousResponseID != nil {
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
		return contract.Request{}, gatewayError(contract.ErrorInvalidRequest, strictJSONErrorMessage("invalid response input", err), http.StatusBadRequest, false, err)
	}
	canonical := &contract.ResponsesRequest{Instructions: wire.Instructions, Input: input, Include: append([]string(nil), wire.Include...)}
	for _, tool := range wire.Tools {
		converted, err := convertResponseTool(tool)
		if err != nil {
			return contract.Request{}, gatewayError(contract.ErrorInvalidRequest, "invalid response tool", http.StatusBadRequest, false, err)
		}
		canonical.Tools = append(canonical.Tools, converted)
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
	if wire.Temperature != nil && (*wire.Temperature < 0 || *wire.Temperature > 2) {
		return contract.Request{}, gatewayError(contract.ErrorInvalidRequest, "temperature must be between 0 and 2", http.StatusBadRequest, false, nil)
	}
	if wire.TopP != nil && (*wire.TopP < 0 || *wire.TopP > 1) {
		return contract.Request{}, gatewayError(contract.ErrorInvalidRequest, "top_p must be between 0 and 1", http.StatusBadRequest, false, nil)
	}
	canonical.Temperature = wire.Temperature
	canonical.TopP = wire.TopP
	canonical.ParallelToolCalls = wire.ParallelToolCalls
	canonical.PromptCacheKey = wire.PromptCacheKey
	canonical.SafetyIdentifier = wire.SafetyIdentifier
	canonical.ServiceTier = wire.ServiceTier
	canonical.Truncation = wire.Truncation
	canonical.User = wire.User
	canonical.TopLogprobs = wire.TopLogprobs
	if len(wire.SafetyIdentifier) > 64 {
		return contract.Request{}, gatewayError(contract.ErrorInvalidRequest, "safety_identifier exceeds 64 characters", http.StatusBadRequest, false, nil)
	}
	if wire.ServiceTier != "" && wire.ServiceTier != "auto" && wire.ServiceTier != "default" && wire.ServiceTier != "flex" && wire.ServiceTier != "scale" && wire.ServiceTier != "priority" && wire.ServiceTier != "fast" && wire.ServiceTier != "ultrafast" {
		return contract.Request{}, gatewayError(contract.ErrorInvalidRequest, "invalid service_tier", http.StatusBadRequest, false, nil)
	}
	if wire.Truncation != "" && wire.Truncation != "auto" && wire.Truncation != "disabled" {
		return contract.Request{}, gatewayError(contract.ErrorInvalidRequest, "invalid truncation", http.StatusBadRequest, false, nil)
	}
	if wire.TopLogprobs != nil && (*wire.TopLogprobs < 0 || *wire.TopLogprobs > 20) {
		return contract.Request{}, gatewayError(contract.ErrorInvalidRequest, "top_logprobs must be between 0 and 20", http.StatusBadRequest, false, nil)
	}
	if wire.Reasoning != nil {
		if wire.Reasoning.Effort != "none" && wire.Reasoning.Effort != "minimal" && wire.Reasoning.Effort != "low" && wire.Reasoning.Effort != "medium" &&
			wire.Reasoning.Effort != "high" && wire.Reasoning.Effort != "xhigh" {
			return contract.Request{}, gatewayError(contract.ErrorInvalidRequest, "invalid reasoning effort", http.StatusBadRequest, false, nil)
		}
		canonical.ReasoningEffort = wire.Reasoning.Effort
		if wire.Reasoning.Summary != "" && wire.Reasoning.Summary != "auto" && wire.Reasoning.Summary != "concise" && wire.Reasoning.Summary != "detailed" {
			return contract.Request{}, gatewayError(contract.ErrorInvalidRequest, "invalid reasoning summary", http.StatusBadRequest, false, nil)
		}
		canonical.ReasoningSummary = wire.Reasoning.Summary
	}
	return contract.Request{
		ID: id, Operation: contract.OperationResponses, PublicModel: wire.Model, Stream: wire.Stream, MaxOutputTokens: maximum,
		EstimatedUsage: contract.Usage{InputTokens: requestBytes, OutputTokens: maximum, TotalTokens: requestBytes + maximum, Estimated: true},
		Responses:      canonical,
	}, nil
}

func convertResponseTool(tool responseTool) (contract.Tool, error) {
	switch tool.Type {
	case "function":
		if tool.Name == "" || len(tool.Parameters) == 0 {
			return contract.Tool{}, errors.New("function tool requires name and parameters")
		}
		return contract.Tool{Type: tool.Type, Function: contract.FunctionDefinition{
			Name: tool.Name, Description: tool.Description, Parameters: append([]byte(nil), tool.Parameters...), Strict: tool.Strict,
		}}, nil
	case "namespace":
		if tool.Name == "" || len(tool.Tools) == 0 {
			return contract.Tool{}, errors.New("tool namespace requires name and tools")
		}
		converted := contract.Tool{Type: tool.Type, Name: tool.Name, Description: tool.Description}
		for _, nested := range tool.Tools {
			if nested.Type != "function" {
				return contract.Tool{}, errors.New("tool namespace may contain only function tools")
			}
			child, err := convertResponseTool(nested)
			if err != nil {
				return contract.Tool{}, err
			}
			converted.Tools = append(converted.Tools, child)
		}
		return converted, nil
	case "web_search":
		return contract.Tool{Type: tool.Type, ExternalWebAccess: tool.ExternalWebAccess}, nil
	default:
		return contract.Tool{}, fmt.Errorf("unsupported response tool type %q", tool.Type)
	}
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
	if err := decodeStrict(trimmed, &items); err != nil {
		return nil, err
	}
	if len(items) == 0 {
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
		return contract.ResponseInputItem{ID: item.ID, Type: "message", Role: item.Role, Content: content}, nil
	case "function_call":
		if item.CallID == "" || item.Name == "" || item.Arguments == "" {
			return contract.ResponseInputItem{}, errors.New("function_call requires call_id, name, and arguments")
		}
		return contract.ResponseInputItem{ID: item.ID, Type: item.Type, CallID: item.CallID, Name: item.Name, Arguments: item.Arguments}, nil
	case "function_call_output":
		if item.CallID == "" || item.Output == "" {
			return contract.ResponseInputItem{}, errors.New("function_call_output requires call_id and output")
		}
		return contract.ResponseInputItem{ID: item.ID, Type: item.Type, CallID: item.CallID, Output: item.Output}, nil
	case "compaction":
		if item.EncryptedContent == "" {
			return contract.ResponseInputItem{}, errors.New("compaction requires encrypted_content")
		}
		return contract.ResponseInputItem{ID: item.ID, Type: item.Type, EncryptedContent: item.EncryptedContent}, nil
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
		case "input_file":
			if part.FileID != "" {
				return nil, errors.New("provider-bound file_id input is not supported")
			}
			if part.FileData == "" {
				return nil, errors.New("input_file requires file_data")
			}
			result = append(result, contract.ContentPart{Type: part.Type, File: &contract.InputFile{Data: part.FileData, Filename: part.Filename}})
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
		encoded, err := encodeResponseOutput(item, false)
		if err != nil {
			return nil, err
		}
		output = append(output, encoded)
	}
	return map[string]any{
		"id": response.Responses.ID, "object": "response", "status": response.Responses.Status,
		"model": response.Model, "output": output,
		"usage": encodeResponsesUsage(response.Usage),
	}, nil
}

func encodeCompactedResponse(response contract.Response) (map[string]any, error) {
	if response.Responses == nil || response.Responses.Object != "response.compaction" || response.Responses.ID == "" || response.Responses.CreatedAt <= 0 {
		return nil, gatewayError(contract.ErrorProvider, "provider returned an invalid compacted response", http.StatusBadGateway, false, nil)
	}
	output := make([]map[string]any, 0, len(response.Responses.Output))
	for _, item := range response.Responses.Output {
		encoded, err := encodeResponseOutput(item, true)
		if err != nil {
			return nil, err
		}
		output = append(output, encoded)
	}
	return map[string]any{"id": response.Responses.ID, "object": response.Responses.Object, "created_at": response.Responses.CreatedAt,
		"output": output, "usage": encodeResponsesUsage(response.Usage)}, nil
}

func encodeResponsesUsage(usage contract.Usage) map[string]any {
	value := map[string]any{"input_tokens": usage.InputTokens, "output_tokens": usage.OutputTokens, "total_tokens": usage.TotalTokens}
	if usage.CacheReadTokens != 0 {
		value["input_tokens_details"] = map[string]any{"cached_tokens": usage.CacheReadTokens}
	}
	if usage.ReasoningTokens != 0 {
		value["output_tokens_details"] = map[string]any{"reasoning_tokens": usage.ReasoningTokens}
	}
	return value
}

func encodeResponseOutput(item contract.ResponseOutputItem, compact bool) (map[string]any, error) {
	encoded := map[string]any{"type": item.Type, "id": item.ID}
	switch item.Type {
	case "message":
		inputRole := item.Role == "user" || item.Role == "system" || item.Role == "developer"
		if item.ID == "" || (item.Role != "assistant" && !(compact && inputRole)) || (item.Role == "assistant" && item.Status == "") {
			return nil, gatewayError(contract.ErrorProvider, "provider returned an incomplete response message", http.StatusBadGateway, false, nil)
		}
		if item.Status != "" {
			encoded["status"] = item.Status
		}
		encoded["role"] = item.Role
		content := make([]map[string]any, 0, len(item.Content))
		for _, part := range item.Content {
			value, err := encodeResponseContentPart(part)
			inputPart := part.Type == "input_text" || part.Type == "input_image" || part.Type == "input_file"
			validPart := item.Role == "assistant" && (part.Type == "output_text" || part.Type == "refusal") || inputRole && inputPart
			if err != nil || !validPart {
				return nil, gatewayError(contract.ErrorProvider, "provider returned unsupported response content", http.StatusBadGateway, false, err)
			}
			content = append(content, value)
		}
		encoded["content"] = content
	case "function_call":
		encoded["status"] = item.Status
		encoded["call_id"] = item.CallID
		encoded["name"] = item.Name
		encoded["arguments"] = item.Arguments
	case "reasoning":
		if item.Status != "" {
			encoded["status"] = item.Status
		}
		summary := make([]map[string]any, 0, len(item.Summary))
		for _, part := range item.Summary {
			value, err := encodeResponseContentPart(part)
			if err != nil || part.Type != "summary_text" {
				return nil, gatewayError(contract.ErrorProvider, "provider returned unsupported reasoning content", http.StatusBadGateway, false, err)
			}
			summary = append(summary, value)
		}
		encoded["summary"] = summary
		if item.Content != nil {
			content := make([]map[string]any, 0, len(item.Content))
			for _, part := range item.Content {
				value, err := encodeResponseContentPart(part)
				if err != nil || part.Type != "reasoning_text" {
					return nil, gatewayError(contract.ErrorProvider, "provider returned unsupported reasoning content", http.StatusBadGateway, false, err)
				}
				content = append(content, value)
			}
			encoded["content"] = content
		}
	case "compaction":
		if item.ID == "" || item.EncryptedContent == "" {
			return nil, gatewayError(contract.ErrorProvider, "provider returned an incomplete compaction item", http.StatusBadGateway, false, nil)
		}
		encoded["encrypted_content"] = item.EncryptedContent
		if item.CreatedBy != "" {
			encoded["created_by"] = item.CreatedBy
		}
	default:
		return nil, gatewayError(contract.ErrorProvider, "provider returned unsupported response item", http.StatusBadGateway, false, nil)
	}
	return encoded, nil
}

func encodeResponseContentPart(part contract.ContentPart) (map[string]any, error) {
	value := map[string]any{"type": part.Type}
	switch part.Type {
	case "input_text":
		value["text"] = part.Text
	case "input_image":
		if part.ImageURL == nil || part.ImageURL.URL == "" {
			return nil, errors.New("response input image has no URL")
		}
		value["image_url"] = part.ImageURL.URL
		if part.ImageURL.Detail != "" {
			value["detail"] = part.ImageURL.Detail
		}
	case "input_file":
		if part.File == nil || part.File.Data == "" {
			return nil, errors.New("response input file is not inline")
		}
		value["file_data"] = part.File.Data
		if part.File.Filename != "" {
			value["filename"] = part.File.Filename
		}
	case "output_text":
		value["text"] = part.Text
		value["annotations"] = []any{}
		if len(part.Annotations) != 0 {
			value["annotations"] = json.RawMessage(append([]byte(nil), part.Annotations...))
		}
		if len(part.Logprobs) != 0 {
			value["logprobs"] = json.RawMessage(append([]byte(nil), part.Logprobs...))
		}
	case "refusal":
		value["refusal"] = part.Refusal
	case "reasoning_text", "summary_text":
		value["text"] = part.Text
	default:
		return nil, fmt.Errorf("unsupported response content part %q", part.Type)
	}
	return value, nil
}

func (h *Handler) streamResponse(response http.ResponseWriter, request *http.Request, principal contract.Principal, canonical contract.Request) {
	session, err := h.startSSE(response, request, principal, canonical)
	if err != nil {
		if session == nil {
			writeError(response, err, canonical.ID)
		} else {
			defer session.Close()
			if session.Active() {
				_ = writeResponseSSE(response, "error", encodeErrorEvent(err, canonical.ID))
				session.flusher.Flush()
			}
		}
		return
	}
	defer session.Close()
	for {
		event, nextErr := session.Next()
		if nextErr != nil {
			if errors.Is(nextErr, io.EOF) {
				return
			}
			if writeResponseSSE(response, "error", encodeErrorEvent(nextErr, canonical.ID)) != nil {
				return
			}
			session.flusher.Flush()
			return
		}
		name, data, encodeErr := encodeResponseEvent(canonical, event)
		if encodeErr != nil {
			_ = writeResponseSSE(response, "error", encodeErrorEvent(encodeErr, canonical.ID))
			session.flusher.Flush()
			return
		}
		if err := writeResponseSSE(response, name, data); err != nil {
			return
		}
		session.flusher.Flush()
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
	switch name {
	case "response.created", "response.queued", "response.in_progress", "response.completed", "response.incomplete":
		if delta.Snapshot == nil || delta.Snapshot.Responses == nil {
			return "", nil, gatewayError(contract.ErrorProvider, "provider returned an incomplete response lifecycle event", http.StatusBadGateway, false, nil)
		}
		snapshot := *delta.Snapshot
		snapshot.Model = request.PublicModel
		if event.Usage != nil {
			snapshot.Usage = *event.Usage
		}
		encoded, err := encodeResponse(snapshot)
		if err != nil {
			return "", nil, err
		}
		payload["response"] = encoded
	case "response.output_item.added", "response.output_item.done":
		if delta.Item == nil || delta.OutputIndex < 0 {
			return "", nil, gatewayError(contract.ErrorProvider, "provider returned an incomplete response output item event", http.StatusBadGateway, false, nil)
		}
		item, err := encodeResponseOutput(*delta.Item, false)
		if err != nil {
			return "", nil, err
		}
		payload["item"] = item
		payload["output_index"] = delta.OutputIndex
	case "response.content_part.added", "response.content_part.done", "response.reasoning_part.added", "response.reasoning_part.done":
		if delta.Part == nil || delta.ItemID == "" || delta.OutputIndex < 0 || delta.ContentIndex < 0 {
			return "", nil, gatewayError(contract.ErrorProvider, "provider returned an incomplete response content part event", http.StatusBadGateway, false, nil)
		}
		part, err := encodeResponseContentPart(*delta.Part)
		if err != nil {
			return "", nil, gatewayError(contract.ErrorProvider, "provider returned an invalid response content part", http.StatusBadGateway, false, err)
		}
		if strings.HasPrefix(name, "response.reasoning_part.") && delta.Part.Type != "reasoning_text" {
			return "", nil, gatewayError(contract.ErrorProvider, "provider returned an invalid reasoning part", http.StatusBadGateway, false, nil)
		}
		payload["item_id"], payload["output_index"], payload["content_index"], payload["part"] = delta.ItemID, delta.OutputIndex, delta.ContentIndex, part
	case "response.output_text.delta", "response.reasoning_text.delta", "response.refusal.delta":
		if delta.ItemID == "" || delta.OutputIndex < 0 || delta.ContentIndex < 0 {
			return "", nil, gatewayError(contract.ErrorProvider, "provider returned an incomplete response text delta event", http.StatusBadGateway, false, nil)
		}
		payload["item_id"], payload["output_index"], payload["content_index"], payload["delta"] = delta.ItemID, delta.OutputIndex, delta.ContentIndex, delta.Delta
		if len(delta.Logprobs) != 0 {
			payload["logprobs"] = json.RawMessage(append([]byte(nil), delta.Logprobs...))
		}
	case "response.output_text.done", "response.reasoning_text.done", "response.refusal.done":
		if delta.ItemID == "" || delta.OutputIndex < 0 || delta.ContentIndex < 0 {
			return "", nil, gatewayError(contract.ErrorProvider, "provider returned an incomplete response text done event", http.StatusBadGateway, false, nil)
		}
		payload["item_id"], payload["output_index"], payload["content_index"] = delta.ItemID, delta.OutputIndex, delta.ContentIndex
		if name == "response.refusal.done" {
			payload["refusal"] = delta.Refusal
		} else {
			payload["text"] = delta.Text
		}
		if len(delta.Logprobs) != 0 {
			payload["logprobs"] = json.RawMessage(append([]byte(nil), delta.Logprobs...))
		}
	case "response.function_call_arguments.delta":
		if delta.ItemID == "" || delta.OutputIndex < 0 {
			return "", nil, gatewayError(contract.ErrorProvider, "provider returned an incomplete function arguments delta event", http.StatusBadGateway, false, nil)
		}
		payload["item_id"], payload["output_index"], payload["delta"] = delta.ItemID, delta.OutputIndex, delta.Delta
	case "response.function_call_arguments.done":
		if delta.ItemID == "" || delta.OutputIndex < 0 || delta.Name == "" {
			return "", nil, gatewayError(contract.ErrorProvider, "provider returned an incomplete function arguments done event", http.StatusBadGateway, false, nil)
		}
		payload["item_id"], payload["output_index"], payload["arguments"], payload["name"] = delta.ItemID, delta.OutputIndex, delta.Arguments, delta.Name
	case "response.reasoning_summary_part.added", "response.reasoning_summary_part.done":
		if delta.Part == nil || delta.ItemID == "" || delta.OutputIndex < 0 || delta.SummaryIndex < 0 {
			return "", nil, gatewayError(contract.ErrorProvider, "provider returned an incomplete reasoning summary part event", http.StatusBadGateway, false, nil)
		}
		part, err := encodeResponseContentPart(*delta.Part)
		if err != nil || delta.Part.Type != "summary_text" {
			return "", nil, gatewayError(contract.ErrorProvider, "provider returned an invalid reasoning summary part", http.StatusBadGateway, false, err)
		}
		payload["item_id"], payload["output_index"], payload["summary_index"], payload["part"] = delta.ItemID, delta.OutputIndex, delta.SummaryIndex, part
		if name == "response.reasoning_summary_part.done" && delta.Status != "" {
			payload["status"] = delta.Status
		}
	case "response.reasoning_summary_text.delta", "response.reasoning_summary_text.done":
		if delta.ItemID == "" || delta.OutputIndex < 0 || delta.SummaryIndex < 0 {
			return "", nil, gatewayError(contract.ErrorProvider, "provider returned an incomplete reasoning summary text event", http.StatusBadGateway, false, nil)
		}
		payload["item_id"], payload["output_index"], payload["summary_index"] = delta.ItemID, delta.OutputIndex, delta.SummaryIndex
		if name == "response.reasoning_summary_text.delta" {
			payload["delta"] = delta.Delta
		} else {
			payload["text"] = delta.Text
		}
	default:
		return "", nil, gatewayError(contract.ErrorProvider, "provider returned an unsupported response event", http.StatusBadGateway, false, nil)
	}
	encoded, err := json.Marshal(payload)
	return name, encoded, err
}

func writeResponseSSE(response io.Writer, event string, data []byte) error {
	_, err := fmt.Fprintf(response, "event: %s\ndata: %s\n\n", event, data)
	return err
}
