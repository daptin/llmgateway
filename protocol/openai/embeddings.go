package openai

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/daptin/llmgateway/contract"
)

type embeddingsRequest struct {
	Model          string          `json:"model"`
	Input          json.RawMessage `json:"input"`
	Dimensions     int             `json:"dimensions,omitempty"`
	EncodingFormat string          `json:"encoding_format,omitempty"`
	User           string          `json:"user,omitempty"`
}

func (h *Handler) embeddings(response http.ResponseWriter, request *http.Request) {
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
	var wire embeddingsRequest
	if err := decodeStrict(body, &wire); err != nil {
		writeError(response, gatewayError(contract.ErrorInvalidRequest, "invalid embeddings request", http.StatusBadRequest, false, err), id)
		return
	}
	canonical, err := canonicalEmbeddings(id, wire, int64(len(body)))
	if err != nil {
		writeError(response, err, id)
		return
	}
	result, err := h.engine.Invoke(request.Context(), principal, canonical)
	if err != nil {
		writeError(response, err, id)
		return
	}
	encoded, err := encodeEmbeddings(result)
	if err != nil {
		writeError(response, err, id)
		return
	}
	writeJSON(response, http.StatusOK, id, encoded)
}

func canonicalEmbeddings(id contract.ID, wire embeddingsRequest, requestBytes int64) (contract.Request, error) {
	if strings.TrimSpace(wire.Model) == "" || len(bytes.TrimSpace(wire.Input)) == 0 {
		return contract.Request{}, gatewayError(contract.ErrorInvalidRequest, "model and input are required", http.StatusBadRequest, false, nil)
	}
	if wire.Dimensions < 0 {
		return contract.Request{}, gatewayError(contract.ErrorInvalidRequest, "dimensions cannot be negative", http.StatusBadRequest, false, nil)
	}
	if wire.EncodingFormat == "" {
		wire.EncodingFormat = "float"
	}
	if wire.EncodingFormat != "float" && wire.EncodingFormat != "base64" {
		return contract.Request{}, gatewayError(contract.ErrorInvalidRequest, "encoding_format must be float or base64", http.StatusBadRequest, false, nil)
	}
	input, err := decodeEmbeddingInput(wire.Input)
	if err != nil {
		return contract.Request{}, gatewayError(contract.ErrorInvalidRequest, "invalid embeddings input", http.StatusBadRequest, false, err)
	}
	return contract.Request{
		ID: id, Operation: contract.OperationEmbeddings, PublicModel: wire.Model,
		EstimatedUsage: contract.Usage{InputTokens: requestBytes, TotalTokens: requestBytes, Estimated: true},
		Embeddings:     &contract.EmbeddingsRequest{Input: input, Dimensions: wire.Dimensions, EncodingFormat: wire.EncodingFormat, User: wire.User},
	}, nil
}

func decodeEmbeddingInput(raw json.RawMessage) (contract.EmbeddingInput, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return contract.EmbeddingInput{}, errors.New("input is required")
	}
	if trimmed[0] == '"' {
		var text string
		if err := json.Unmarshal(trimmed, &text); err != nil || text == "" {
			return contract.EmbeddingInput{}, errors.New("input text cannot be empty")
		}
		return contract.EmbeddingInput{Texts: []string{text}}, nil
	}
	var values []json.RawMessage
	if err := json.Unmarshal(trimmed, &values); err != nil || len(values) == 0 {
		return contract.EmbeddingInput{}, errors.New("input must be a string or non-empty array")
	}
	if bytes.HasPrefix(bytes.TrimSpace(values[0]), []byte("\"")) {
		texts := make([]string, len(values))
		for index, value := range values {
			if err := json.Unmarshal(value, &texts[index]); err != nil || texts[index] == "" {
				return contract.EmbeddingInput{}, errors.New("text arrays must contain only non-empty strings")
			}
		}
		return contract.EmbeddingInput{Texts: texts}, nil
	}
	if bytes.HasPrefix(bytes.TrimSpace(values[0]), []byte("[")) {
		tokens := make([][]int64, len(values))
		for index, value := range values {
			if err := json.Unmarshal(value, &tokens[index]); err != nil || len(tokens[index]) == 0 || !validTokenIDs(tokens[index]) {
				return contract.EmbeddingInput{}, errors.New("token arrays must contain only non-negative integers")
			}
		}
		return contract.EmbeddingInput{Tokens: tokens}, nil
	}
	var tokens []int64
	if err := json.Unmarshal(trimmed, &tokens); err != nil || len(tokens) == 0 || !validTokenIDs(tokens) {
		return contract.EmbeddingInput{}, errors.New("token arrays must contain only non-negative integers")
	}
	return contract.EmbeddingInput{Tokens: [][]int64{tokens}}, nil
}

func validTokenIDs(tokens []int64) bool {
	for _, token := range tokens {
		if token < 0 {
			return false
		}
	}
	return true
}

func encodeEmbeddings(response contract.Response) (map[string]any, error) {
	if response.Embeddings == nil {
		return nil, gatewayError(contract.ErrorProvider, "provider returned no embeddings response", http.StatusBadGateway, false, nil)
	}
	data := make([]map[string]any, 0, len(response.Embeddings.Data))
	for _, embedding := range response.Embeddings.Data {
		value := any(embedding.Vector)
		if embedding.Base64 != "" {
			value = embedding.Base64
		}
		data = append(data, map[string]any{"object": "embedding", "index": embedding.Index, "embedding": value})
	}
	return map[string]any{"object": "list", "data": data, "model": response.Model, "usage": encodeUsage(response.Usage)}, nil
}
