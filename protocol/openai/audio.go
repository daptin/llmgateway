package openai

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"

	"github.com/daptin/llmgateway/contract"
)

type speechRequest struct {
	Model          string   `json:"model"`
	Input          string   `json:"input"`
	Voice          string   `json:"voice"`
	Instructions   string   `json:"instructions,omitempty"`
	Speed          *float64 `json:"speed,omitempty"`
	ResponseFormat string   `json:"response_format,omitempty"`
}

func (h *Handler) audioSpeech(response http.ResponseWriter, request *http.Request) {
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
	var wire speechRequest
	if err := decodeStrict(body, &wire); err != nil {
		writeError(response, gatewayError(contract.ErrorInvalidRequest, "invalid audio speech request", http.StatusBadRequest, false, err), id)
		return
	}
	if wire.ResponseFormat == "" {
		wire.ResponseFormat = "mp3"
	}
	estimatedInput := int64(len(wire.Input) + len(wire.Instructions))
	canonical := contract.Request{ID: id, Operation: contract.OperationAudioSpeech, PublicModel: wire.Model,
		EstimatedUsage: contract.Usage{InputTokens: estimatedInput, TotalTokens: estimatedInput, Estimated: true,
			Measures: map[string]int64{"request_bytes": int64(len(body))}},
		AudioSpeech: &contract.AudioSpeechRequest{Input: wire.Input, Voice: wire.Voice, Instructions: wire.Instructions,
			Speed: wire.Speed, ResponseFormat: wire.ResponseFormat}}
	if err := canonical.Validate(); err != nil {
		writeError(response, gatewayError(contract.ErrorInvalidRequest, "invalid audio speech request", http.StatusBadRequest, false, err), id)
		return
	}
	result, err := h.engine.Invoke(request.Context(), principal, canonical)
	if err != nil {
		writeError(response, err, id)
		return
	}
	if result.AudioSpeech == nil || len(result.AudioSpeech.Data) == 0 || result.AudioSpeech.ContentType == "" {
		writeError(response, gatewayError(contract.ErrorProvider, "provider returned no audio", http.StatusBadGateway, false, nil), id)
		return
	}
	response.Header().Set("Content-Type", result.AudioSpeech.ContentType)
	response.Header().Set("X-Request-ID", string(id))
	response.WriteHeader(http.StatusOK)
	_, _ = response.Write(result.AudioSpeech.Data)
}

func (h *Handler) audioTranscription(response http.ResponseWriter, request *http.Request) {
	h.audioText(response, request, contract.OperationTranscription)
}

func (h *Handler) audioTranslation(response http.ResponseWriter, request *http.Request) {
	h.audioText(response, request, contract.OperationTranslation)
}

func (h *Handler) audioText(response http.ResponseWriter, request *http.Request, operation contract.Operation) {
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
	allowed := map[string]bool{"file": true, "model": true, "language": operation == contract.OperationTranscription, "prompt": true,
		"temperature": true, "response_format": true, "timestamp_granularities[]": operation == contract.OperationTranscription}
	for name := range values {
		if !allowed[name] {
			writeError(response, gatewayError(contract.ErrorInvalidRequest, "unsupported multipart field", http.StatusBadRequest, false, nil), id)
			return
		}
	}
	for name, entries := range values {
		if name != "timestamp_granularities[]" && len(entries) != 1 {
			writeError(response, gatewayError(contract.ErrorInvalidRequest, "duplicate multipart field", http.StatusBadRequest, false, nil), id)
			return
		}
	}
	file, fileOK := oneMultipartValue(values, "file")
	model, modelOK := oneMultipartValue(values, "model")
	if !fileOK || file.filename == "" || len(file.data) == 0 || !modelOK || strings.TrimSpace(string(model.data)) == "" {
		writeError(response, gatewayError(contract.ErrorInvalidRequest, "file and model are required", http.StatusBadRequest, false, nil), id)
		return
	}
	format := multipartText(values, "response_format")
	if format == "" {
		format = "json"
	}
	var temperature *float64
	if raw := multipartText(values, "temperature"); raw != "" {
		parsed, parseErr := strconv.ParseFloat(raw, 64)
		if parseErr != nil {
			writeError(response, gatewayError(contract.ErrorInvalidRequest, "temperature is invalid", http.StatusBadRequest, false, parseErr), id)
			return
		}
		temperature = &parsed
	}
	granularities := make([]string, 0, len(values["timestamp_granularities[]"]))
	for _, entry := range values["timestamp_granularities[]"] {
		granularities = append(granularities, string(entry.data))
	}
	canonical := contract.Request{ID: id, Operation: operation, PublicModel: strings.TrimSpace(string(model.data)),
		EstimatedUsage: contract.Usage{Estimated: true, Measures: map[string]int64{"audio_bytes": int64(len(file.data))}},
		Transcription: &contract.AudioTranscriptionRequest{File: contract.MediaFile{Name: file.filename, ContentType: file.contentType, Data: file.data},
			Language: multipartText(values, "language"), Prompt: multipartText(values, "prompt"), Temperature: temperature,
			ResponseFormat: format, TimestampGranularities: granularities}}
	if err := canonical.Validate(); err != nil {
		writeError(response, gatewayError(contract.ErrorInvalidRequest, "invalid audio request", http.StatusBadRequest, false, err), id)
		return
	}
	result, err := h.engine.Invoke(request.Context(), principal, canonical)
	if err != nil {
		writeError(response, err, id)
		return
	}
	if result.Transcription == nil || result.Transcription.Format == "" {
		writeError(response, gatewayError(contract.ErrorProvider, "provider returned no transcription", http.StatusBadGateway, false, nil), id)
		return
	}
	if result.Transcription.Format == "json" || result.Transcription.Format == "verbose_json" || result.Transcription.Format == "diarized_json" {
		var encoded any
		if err := json.Unmarshal(result.Transcription.JSON, &encoded); err != nil {
			writeError(response, gatewayError(contract.ErrorProvider, "provider returned invalid transcription JSON", http.StatusBadGateway, false, err), id)
			return
		}
		writeJSON(response, http.StatusOK, id, encoded)
		return
	}
	response.Header().Set("Content-Type", "text/plain; charset=utf-8")
	response.Header().Set("X-Request-ID", string(id))
	response.WriteHeader(http.StatusOK)
	_, _ = response.Write([]byte(result.Transcription.Text))
}
