package openai

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/daptin/llmgateway/contract"
)

type moderationRequest struct {
	Model string          `json:"model"`
	Input json.RawMessage `json:"input"`
}

type moderationInputPart struct {
	Type     string    `json:"type"`
	Text     string    `json:"text,omitempty"`
	ImageURL *imageURL `json:"image_url,omitempty"`
}

func (h *Handler) moderations(response http.ResponseWriter, request *http.Request) {
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
	var wire moderationRequest
	if err := decodeStrict(body, &wire); err != nil {
		writeError(response, gatewayError(contract.ErrorInvalidRequest, "invalid moderation request", http.StatusBadRequest, false, err), id)
		return
	}
	canonical, err := canonicalModeration(id, wire, int64(len(body)))
	if err != nil {
		writeError(response, err, id)
		return
	}
	result, err := h.engine.Invoke(request.Context(), principal, canonical)
	if err != nil {
		writeError(response, err, id)
		return
	}
	encoded, err := encodeModerationResponse(result)
	if err != nil {
		writeError(response, err, id)
		return
	}
	writeJSON(response, http.StatusOK, id, encoded)
}

func canonicalModeration(id contract.ID, wire moderationRequest, requestBytes int64) (contract.Request, error) {
	if strings.TrimSpace(wire.Model) == "" {
		return contract.Request{}, gatewayError(contract.ErrorInvalidRequest, "model is required", http.StatusBadRequest, false, nil)
	}
	input, err := decodeModerationInput(wire.Input)
	if err != nil {
		return contract.Request{}, gatewayError(contract.ErrorInvalidRequest, "invalid moderation input", http.StatusBadRequest, false, err)
	}
	return contract.Request{ID: id, Operation: contract.OperationModeration, PublicModel: wire.Model,
		EstimatedUsage: contract.Usage{InputTokens: requestBytes, TotalTokens: requestBytes, Estimated: true},
		Moderation:     &contract.ModerationRequest{Input: input}}, nil
}

func decodeModerationInput(raw json.RawMessage) ([]contract.ModerationInput, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return nil, errors.New("input is required")
	}
	if trimmed[0] == '"' {
		var text string
		if err := json.Unmarshal(trimmed, &text); err != nil || text == "" {
			return nil, errors.New("moderation text cannot be empty")
		}
		return []contract.ModerationInput{{Type: "text", Text: text}}, nil
	}
	var values []json.RawMessage
	if err := json.Unmarshal(trimmed, &values); err != nil || len(values) == 0 {
		return nil, errors.New("moderation input must be text or a non-empty array")
	}
	result := make([]contract.ModerationInput, 0, len(values))
	for _, rawValue := range values {
		value := bytes.TrimSpace(rawValue)
		if len(value) == 0 {
			return nil, errors.New("moderation input is empty")
		}
		if value[0] == '"' {
			var text string
			if err := json.Unmarshal(value, &text); err != nil || text == "" {
				return nil, errors.New("moderation text cannot be empty")
			}
			result = append(result, contract.ModerationInput{Type: "text", Text: text})
			continue
		}
		var part moderationInputPart
		if err := decodeStrict(value, &part); err != nil {
			return nil, err
		}
		switch part.Type {
		case "text":
			if part.Text == "" || part.ImageURL != nil {
				return nil, errors.New("text moderation input is invalid")
			}
			result = append(result, contract.ModerationInput{Type: "text", Text: part.Text})
		case "image_url":
			if part.ImageURL == nil || part.ImageURL.URL == "" || part.Text != "" {
				return nil, errors.New("image moderation input is invalid")
			}
			result = append(result, contract.ModerationInput{Type: "image_url", ImageURL: &contract.ImageURL{URL: part.ImageURL.URL, Detail: part.ImageURL.Detail}})
		default:
			return nil, errors.New("unsupported moderation input type")
		}
	}
	return result, nil
}

func encodeModerationResponse(response contract.Response) (map[string]any, error) {
	if response.Moderation == nil || len(response.Moderation.Results) == 0 {
		return nil, gatewayError(contract.ErrorProvider, "provider returned no moderation result", http.StatusBadGateway, false, nil)
	}
	results := make([]map[string]any, 0, len(response.Moderation.Results))
	for _, result := range response.Moderation.Results {
		results = append(results, map[string]any{"flagged": result.Flagged, "categories": result.Categories, "category_scores": result.CategoryScores})
	}
	return map[string]any{"id": response.Moderation.ID, "model": response.Model, "results": results}, nil
}
