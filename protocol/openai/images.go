package openai

import (
	"net/http"
	"strconv"
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

func (h *Handler) imageEdits(response http.ResponseWriter, request *http.Request) {
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
	values, err := readMultipartForm(response, request, h.maxBody)
	if err != nil {
		writeError(response, err, id)
		return
	}
	allowed := map[string]bool{"image": true, "image[]": true, "mask": true, "model": true, "prompt": true, "n": true, "size": true,
		"quality": true, "response_format": true, "user": true, "background": true, "input_fidelity": true, "output_format": true,
		"output_compression": true, "moderation": true}
	for name, entries := range values {
		if !allowed[name] || (name != "image" && name != "image[]" && len(entries) != 1) {
			writeError(response, gatewayError(contract.ErrorInvalidRequest, "unsupported or duplicate multipart field", http.StatusBadRequest, false, nil), id)
			return
		}
	}
	if len(values["image"]) != 0 && len(values["image[]"]) != 0 {
		writeError(response, gatewayError(contract.ErrorInvalidRequest, "image and image[] cannot be combined", http.StatusBadRequest, false, nil), id)
		return
	}
	imageValues := values["image"]
	if len(imageValues) == 0 {
		imageValues = values["image[]"]
	}
	images := make([]contract.MediaFile, 0, len(imageValues))
	for _, image := range imageValues {
		if image.filename == "" || len(image.data) == 0 {
			writeError(response, gatewayError(contract.ErrorInvalidRequest, "image files are required", http.StatusBadRequest, false, nil), id)
			return
		}
		images = append(images, contract.MediaFile{Name: image.filename, ContentType: image.contentType, Data: image.data})
	}
	var mask *contract.MediaFile
	if value, ok := oneMultipartValue(values, "mask"); ok {
		if value.filename == "" || len(value.data) == 0 {
			writeError(response, gatewayError(contract.ErrorInvalidRequest, "mask must be a file", http.StatusBadRequest, false, nil), id)
			return
		}
		mask = &contract.MediaFile{Name: value.filename, ContentType: value.contentType, Data: value.data}
	}
	model, prompt := strings.TrimSpace(multipartText(values, "model")), multipartText(values, "prompt")
	if model == "" || strings.TrimSpace(prompt) == "" || len(images) == 0 {
		writeError(response, gatewayError(contract.ErrorInvalidRequest, "model, prompt, and image are required", http.StatusBadRequest, false, nil), id)
		return
	}
	n, err := optionalMultipartInt(values, "n")
	if err != nil {
		writeError(response, gatewayError(contract.ErrorInvalidRequest, "n is invalid", http.StatusBadRequest, false, err), id)
		return
	}
	compression, err := optionalMultipartInt(values, "output_compression")
	if err != nil {
		writeError(response, gatewayError(contract.ErrorInvalidRequest, "output_compression is invalid", http.StatusBadRequest, false, err), id)
		return
	}
	count := 0
	if n != nil {
		count = *n
	}
	format := multipartText(values, "response_format")
	if format == "" {
		format = "url"
	}
	imageBytes := int64(0)
	for _, image := range images {
		imageBytes += int64(len(image.Data))
	}
	if mask != nil {
		imageBytes += int64(len(mask.Data))
	}
	inputTokens := int64(len(prompt))
	canonical := contract.Request{ID: id, Operation: contract.OperationImageEdit, PublicModel: model,
		EstimatedUsage: contract.Usage{InputTokens: inputTokens, TotalTokens: inputTokens, Estimated: true,
			Measures: map[string]int64{"image_bytes": imageBytes}},
		ImageEdit: &contract.ImageEditRequest{Images: images, Mask: mask, Prompt: prompt, N: count, Size: multipartText(values, "size"),
			Quality: multipartText(values, "quality"), ResponseFormat: format, User: multipartText(values, "user"),
			Background: multipartText(values, "background"), InputFidelity: multipartText(values, "input_fidelity"),
			OutputFormat: multipartText(values, "output_format"), OutputCompression: compression, Moderation: multipartText(values, "moderation")}}
	if err := canonical.Validate(); err != nil {
		writeError(response, gatewayError(contract.ErrorInvalidRequest, "invalid image edit request", http.StatusBadRequest, false, err), id)
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

func (h *Handler) imageVariations(response http.ResponseWriter, request *http.Request) {
	id, principal, ok := h.authenticatedRequest(response, request)
	if !ok {
		return
	}
	values, err := readMultipartForm(response, request, h.maxBody)
	if err != nil {
		writeError(response, err, id)
		return
	}
	allowed := map[string]bool{"image": true, "model": true, "n": true, "size": true, "response_format": true, "user": true}
	for name, entries := range values {
		if !allowed[name] || len(entries) != 1 {
			writeError(response, gatewayError(contract.ErrorInvalidRequest, "unsupported or duplicate multipart field", http.StatusBadRequest, false, nil), id)
			return
		}
	}
	image, imageOK := oneMultipartValue(values, "image")
	model := strings.TrimSpace(multipartText(values, "model"))
	if !imageOK || image.filename == "" || len(image.data) == 0 || model == "" {
		writeError(response, gatewayError(contract.ErrorInvalidRequest, "model and image are required", http.StatusBadRequest, false, nil), id)
		return
	}
	n, err := optionalMultipartInt(values, "n")
	if err != nil {
		writeError(response, gatewayError(contract.ErrorInvalidRequest, "n is invalid", http.StatusBadRequest, false, err), id)
		return
	}
	count := 1
	if n != nil {
		count = *n
	}
	format := strings.TrimSpace(multipartText(values, "response_format"))
	if format == "" {
		format = "url"
	}
	canonical := contract.Request{ID: id, Operation: contract.OperationImageVariation, PublicModel: model,
		EstimatedUsage: contract.Usage{Estimated: true, Measures: map[string]int64{"image_bytes": int64(len(image.data))}},
		ImageVariation: &contract.ImageVariationRequest{Image: contract.MediaFile{Name: image.filename, ContentType: image.contentType, Data: image.data},
			N: count, Size: strings.TrimSpace(multipartText(values, "size")), ResponseFormat: format, User: strings.TrimSpace(multipartText(values, "user"))}}
	if err := canonical.Validate(); err != nil {
		writeError(response, gatewayError(contract.ErrorInvalidRequest, "invalid image variation request", http.StatusBadRequest, false, err), id)
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

func optionalMultipartInt(values map[string][]multipartValue, name string) (*int, error) {
	raw := multipartText(values, name)
	if raw == "" {
		return nil, nil
	}
	value, err := strconv.Atoi(raw)
	if err != nil {
		return nil, err
	}
	return &value, nil
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
		EstimatedUsage: contract.Usage{InputTokens: int64(len(wire.Prompt)), TotalTokens: int64(len(wire.Prompt)), Estimated: true,
			Measures: map[string]int64{"request_bytes": requestBytes}},
		ImageGeneration: &contract.ImageGenerationRequest{Prompt: wire.Prompt, N: n, Size: wire.Size, Quality: wire.Quality, ResponseFormat: wire.ResponseFormat},
	}, nil
}

func encodeImages(response contract.Response) (map[string]any, error) {
	if response.Images == nil {
		return nil, gatewayError(contract.ErrorProvider, "provider returned no image response", http.StatusBadGateway, false, nil)
	}
	data := make([]map[string]any, 0, len(response.Images.Data))
	for _, image := range response.Images.Data {
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
	return map[string]any{"created": response.Images.Created, "data": data}, nil
}
