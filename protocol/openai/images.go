package openai

import (
	"net/http"
	"strings"

	"github.com/daptin/llmgateway/contract"
)

type imageRequest struct {
	Model          string `json:"model"`
	Prompt         string `json:"prompt"`
	N              *int   `json:"n,omitempty"`
	Size           string `json:"size,omitempty"`
	Quality        string `json:"quality,omitempty"`
	ResponseFormat string `json:"response_format,omitempty"`
}

func (h *Handler) imageGenerations(response http.ResponseWriter, request *http.Request) {
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
	var wire imageRequest
	if err := decodeStrict(body, &wire); err != nil {
		writeError(response, gatewayError(contract.ErrorInvalidRequest, "invalid image generation request", http.StatusBadRequest, false, err), id)
		return
	}
	canonical, err := canonicalImage(id, wire, int64(len(body)))
	if err != nil {
		writeError(response, err, id)
		return
	}
	result, err := h.engine.Invoke(request.Context(), principal, canonical)
	if err != nil {
		writeError(response, err, id)
		return
	}
	encoded, err := encodeImages(result)
	if err != nil {
		writeError(response, err, id)
		return
	}
	writeJSON(response, http.StatusOK, id, encoded)
}

func canonicalImage(id contract.ID, wire imageRequest, requestBytes int64) (contract.Request, error) {
	if strings.TrimSpace(wire.Model) == "" || strings.TrimSpace(wire.Prompt) == "" {
		return contract.Request{}, gatewayError(contract.ErrorInvalidRequest, "model and prompt are required", http.StatusBadRequest, false, nil)
	}
	var n int
	if wire.N != nil {
		n = *wire.N
	}
	if wire.N != nil && (n < 1 || n > 10) {
		return contract.Request{}, gatewayError(contract.ErrorInvalidRequest, "n must be between 1 and 10", http.StatusBadRequest, false, nil)
	}
	if wire.ResponseFormat != "" && wire.ResponseFormat != "url" && wire.ResponseFormat != "b64_json" {
		return contract.Request{}, gatewayError(contract.ErrorInvalidRequest, "response_format must be url or b64_json", http.StatusBadRequest, false, nil)
	}
	return contract.Request{
		ID: id, Operation: contract.OperationImageGeneration, PublicModel: wire.Model,
		EstimatedUsage:  contract.Usage{InputTokens: requestBytes, TotalTokens: requestBytes, Estimated: true},
		ImageGeneration: &contract.ImageGenerationRequest{Prompt: wire.Prompt, N: n, Size: wire.Size, Quality: wire.Quality, ResponseFormat: wire.ResponseFormat},
	}, nil
}

func encodeImages(response contract.Response) (map[string]any, error) {
	if response.ImageGeneration == nil {
		return nil, gatewayError(contract.ErrorProvider, "provider returned no image response", http.StatusBadGateway, false, nil)
	}
	data := make([]map[string]any, 0, len(response.ImageGeneration.Data))
	for _, image := range response.ImageGeneration.Data {
		value := map[string]any{}
		if image.URL != "" {
			value["url"] = image.URL
		}
		if image.Base64 != "" {
			value["b64_json"] = image.Base64
		}
		if image.RevisedPrompt != "" {
			value["revised_prompt"] = image.RevisedPrompt
		}
		data = append(data, value)
	}
	return map[string]any{"created": response.ImageGeneration.Created, "data": data}, nil
}
