package openai

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"strings"

	"github.com/daptin/llmgateway/contract"
)

type completionRequest struct {
	Model            string          `json:"model"`
	Prompt           json.RawMessage `json:"prompt"`
	BestOf           *int            `json:"best_of,omitempty"`
	Echo             *bool           `json:"echo,omitempty"`
	FrequencyPenalty *float64        `json:"frequency_penalty,omitempty"`
	LogitBias        map[string]int  `json:"logit_bias,omitempty"`
	Logprobs         *int            `json:"logprobs,omitempty"`
	MaxTokens        *int64          `json:"max_tokens,omitempty"`
	N                *int            `json:"n,omitempty"`
	PresencePenalty  *float64        `json:"presence_penalty,omitempty"`
	Seed             *int64          `json:"seed,omitempty"`
	Stop             json.RawMessage `json:"stop,omitempty"`
	Stream           bool            `json:"stream,omitempty"`
	StreamOptions    *streamOptions  `json:"stream_options,omitempty"`
	Suffix           string          `json:"suffix,omitempty"`
	Temperature      *float64        `json:"temperature,omitempty"`
	TopP             *float64        `json:"top_p,omitempty"`
	User             string          `json:"user,omitempty"`
}

func (h *Handler) completions(response http.ResponseWriter, request *http.Request) {
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
	var wire completionRequest
	if err := decodeStrict(body, &wire); err != nil {
		writeError(response, gatewayError(contract.ErrorInvalidRequest, "invalid text completion request", http.StatusBadRequest, false, err), id)
		return
	}
	canonical, includeUsage, err := canonicalCompletion(id, wire, int64(len(body)))
	if err != nil {
		writeError(response, err, id)
		return
	}
	if canonical.Stream {
		h.streamCompletion(response, request, principal, canonical, includeUsage)
		return
	}
	result, err := h.engine.Invoke(request.Context(), principal, canonical)
	if err != nil {
		writeError(response, err, id)
		return
	}
	encoded, err := encodeCompletionResponse(result)
	if err != nil {
		writeError(response, err, id)
		return
	}
	writeJSON(response, http.StatusOK, id, encoded)
}

func canonicalCompletion(id contract.ID, wire completionRequest, requestBytes int64) (contract.Request, bool, error) {
	if strings.TrimSpace(wire.Model) == "" {
		return contract.Request{}, false, gatewayError(contract.ErrorInvalidRequest, "model is required", http.StatusBadRequest, false, nil)
	}
	prompt, err := decodeCompletionPrompt(wire.Prompt)
	if err != nil {
		return contract.Request{}, false, gatewayError(contract.ErrorInvalidRequest, "invalid prompt", http.StatusBadRequest, false, err)
	}
	completion := &contract.TextCompletionRequest{Echo: wire.Echo, FrequencyPenalty: wire.FrequencyPenalty, LogitBias: wire.LogitBias,
		Logprobs: wire.Logprobs, PresencePenalty: wire.PresencePenalty, Seed: wire.Seed, Suffix: wire.Suffix,
		Temperature: wire.Temperature, TopP: wire.TopP, User: wire.User, Prompt: prompt}
	if wire.N != nil {
		completion.N = *wire.N
	}
	if wire.BestOf != nil {
		completion.BestOf = *wire.BestOf
	}
	if wire.MaxTokens != nil {
		completion.MaxTokens = *wire.MaxTokens
	}
	completion.Stop, err = convertStop(wire.Stop)
	if err != nil {
		return contract.Request{}, false, gatewayError(contract.ErrorInvalidRequest, "invalid stop value", http.StatusBadRequest, false, err)
	}
	if wire.N != nil && (completion.N < 1 || completion.N > 128) {
		return contract.Request{}, false, gatewayError(contract.ErrorInvalidRequest, "n must be between 1 and 128", http.StatusBadRequest, false, nil)
	}
	if wire.BestOf != nil && (completion.BestOf < 1 || completion.BestOf > 128) {
		return contract.Request{}, false, gatewayError(contract.ErrorInvalidRequest, "best_of must be between 1 and 128", http.StatusBadRequest, false, nil)
	}
	if wire.N != nil && wire.BestOf != nil && completion.BestOf < completion.N {
		return contract.Request{}, false, gatewayError(contract.ErrorInvalidRequest, "best_of cannot be less than n", http.StatusBadRequest, false, nil)
	}
	if wire.Stream && wire.BestOf != nil && completion.BestOf > max(completion.N, 1) {
		return contract.Request{}, false, gatewayError(contract.ErrorInvalidRequest, "streaming does not support best_of greater than n", http.StatusBadRequest, false, nil)
	}
	if wire.MaxTokens != nil && completion.MaxTokens < 1 {
		return contract.Request{}, false, gatewayError(contract.ErrorInvalidRequest, "max_tokens must be positive", http.StatusBadRequest, false, nil)
	}
	if !validOptionalCompletionControls(completion) {
		return contract.Request{}, false, gatewayError(contract.ErrorInvalidRequest, "invalid text completion controls", http.StatusBadRequest, false, nil)
	}
	if !wire.Stream && wire.StreamOptions != nil {
		return contract.Request{}, false, gatewayError(contract.ErrorInvalidRequest, "stream_options requires stream=true", http.StatusBadRequest, false, nil)
	}
	includeUsage := wire.StreamOptions != nil && wire.StreamOptions.IncludeUsage
	exposureChoices := max(completion.BestOf, max(completion.N, 1))
	if completion.MaxTokens > 0 && completion.MaxTokens > math.MaxInt64/int64(exposureChoices) {
		return contract.Request{}, false, gatewayError(contract.ErrorInvalidRequest, "text completion output exposure is too large", http.StatusBadRequest, false, nil)
	}
	estimatedOutput := completion.MaxTokens * int64(exposureChoices)
	return contract.Request{ID: id, Operation: contract.OperationTextCompletion, PublicModel: wire.Model, Stream: wire.Stream,
		MaxOutputTokens: estimatedOutput, EstimatedUsage: contract.Usage{InputTokens: requestBytes, OutputTokens: estimatedOutput,
			TotalTokens: requestBytes + estimatedOutput, Estimated: true}, TextCompletion: completion}, includeUsage, nil
}

func validOptionalCompletionControls(completion *contract.TextCompletionRequest) bool {
	if completion.Temperature != nil && (*completion.Temperature < 0 || *completion.Temperature > 2) ||
		completion.TopP != nil && (*completion.TopP < 0 || *completion.TopP > 1) ||
		completion.FrequencyPenalty != nil && (*completion.FrequencyPenalty < -2 || *completion.FrequencyPenalty > 2) ||
		completion.PresencePenalty != nil && (*completion.PresencePenalty < -2 || *completion.PresencePenalty > 2) ||
		completion.Logprobs != nil && (*completion.Logprobs < 0 || *completion.Logprobs > 5) {
		return false
	}
	for token, bias := range completion.LogitBias {
		if token == "" || bias < -100 || bias > 100 {
			return false
		}
	}
	return true
}

func decodeCompletionPrompt(raw json.RawMessage) (contract.CompletionPrompt, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return contract.CompletionPrompt{}, errors.New("prompt is required")
	}
	if trimmed[0] == '"' {
		var text string
		if err := json.Unmarshal(trimmed, &text); err != nil || text == "" {
			return contract.CompletionPrompt{}, errors.New("prompt text cannot be empty")
		}
		return contract.CompletionPrompt{Texts: []string{text}}, nil
	}
	var values []json.RawMessage
	if err := json.Unmarshal(trimmed, &values); err != nil || len(values) == 0 {
		return contract.CompletionPrompt{}, errors.New("prompt must be text, text array, token array, or token-array list")
	}
	if bytes.TrimSpace(values[0])[0] == '"' {
		texts := make([]string, len(values))
		for index, value := range values {
			if err := json.Unmarshal(value, &texts[index]); err != nil || texts[index] == "" {
				return contract.CompletionPrompt{}, errors.New("prompt text array is invalid")
			}
		}
		return contract.CompletionPrompt{Texts: texts}, nil
	}
	if bytes.TrimSpace(values[0])[0] == '[' {
		var tokens [][]int64
		if err := json.Unmarshal(trimmed, &tokens); err != nil || !validTokenPrompts(tokens) {
			return contract.CompletionPrompt{}, errors.New("prompt token arrays are invalid")
		}
		return contract.CompletionPrompt{Tokens: tokens}, nil
	}
	var tokens []int64
	if err := json.Unmarshal(trimmed, &tokens); err != nil || !validTokenPrompts([][]int64{tokens}) {
		return contract.CompletionPrompt{}, errors.New("prompt token array is invalid")
	}
	return contract.CompletionPrompt{Tokens: [][]int64{tokens}}, nil
}

func validTokenPrompts(prompts [][]int64) bool {
	if len(prompts) == 0 {
		return false
	}
	for _, prompt := range prompts {
		if len(prompt) == 0 {
			return false
		}
		for _, token := range prompt {
			if token < 0 {
				return false
			}
		}
	}
	return true
}

func encodeCompletionResponse(response contract.Response) (map[string]any, error) {
	if response.TextCompletion == nil {
		return nil, gatewayError(contract.ErrorProvider, "provider returned no text completion", http.StatusBadGateway, false, nil)
	}
	choices := make([]map[string]any, 0, len(response.TextCompletion.Choices))
	for _, choice := range response.TextCompletion.Choices {
		logprobs, err := decodeOptionalJSON(choice.Logprobs)
		if err != nil {
			return nil, gatewayError(contract.ErrorProvider, "provider returned invalid text completion logprobs", http.StatusBadGateway, false, err)
		}
		choices = append(choices, map[string]any{"text": choice.Text, "index": choice.Index, "logprobs": logprobs, "finish_reason": choice.FinishReason})
	}
	return map[string]any{"id": response.TextCompletion.ID, "object": "text_completion", "created": response.TextCompletion.Created,
		"model": response.Model, "choices": choices, "usage": encodeDetailedUsage(response.Usage)}, nil
}

func (h *Handler) streamCompletion(response http.ResponseWriter, request *http.Request, principal contract.Principal, canonical contract.Request, includeUsage bool) {
	session, err := h.startSSE(response, request, principal, canonical)
	if err != nil {
		if session == nil {
			writeError(response, err, canonical.ID)
		} else {
			defer session.Close()
			if session.Active() {
				_, _ = fmt.Fprintf(response, "data: %s\n\n", encodeErrorEvent(err, canonical.ID))
				_, _ = io.WriteString(response, "data: [DONE]\n\n")
				session.flusher.Flush()
			}
		}
		return
	}
	defer session.Close()
	for {
		event, nextErr := session.Next()
		if nextErr != nil {
			if !errors.Is(nextErr, io.EOF) {
				_, _ = fmt.Fprintf(response, "data: %s\n\n", encodeErrorEvent(nextErr, canonical.ID))
			}
			_, _ = io.WriteString(response, "data: [DONE]\n\n")
			session.flusher.Flush()
			return
		}
		if event.Usage != nil && !includeUsage && event.TextCompletion == nil {
			if event.Terminal {
				_, _ = io.WriteString(response, "data: [DONE]\n\n")
				session.flusher.Flush()
				return
			}
			continue
		}
		encoded, encodeErr := encodeCompletionChunk(canonical, event, includeUsage)
		if encodeErr != nil {
			_, _ = fmt.Fprintf(response, "data: %s\n\n", encodeErrorEvent(encodeErr, canonical.ID))
			_, _ = io.WriteString(response, "data: [DONE]\n\n")
			session.flusher.Flush()
			return
		}
		if _, err := fmt.Fprintf(response, "data: %s\n\n", encoded); err != nil {
			return
		}
		session.flusher.Flush()
		if event.Terminal {
			_, _ = io.WriteString(response, "data: [DONE]\n\n")
			session.flusher.Flush()
			return
		}
	}
}

func encodeCompletionChunk(request contract.Request, event contract.StreamEvent, includeUsage bool) ([]byte, error) {
	choices := make([]any, 0, 1)
	if event.TextCompletion != nil {
		choice := event.TextCompletion
		logprobs, err := decodeOptionalJSON(choice.Logprobs)
		if err != nil {
			return nil, gatewayError(contract.ErrorProvider, "provider returned invalid text completion logprobs", http.StatusBadGateway, false, err)
		}
		var finishReason any
		if choice.FinishReason != "" {
			finishReason = choice.FinishReason
		}
		choices = append(choices, map[string]any{"text": choice.Text, "index": choice.Index, "logprobs": logprobs,
			"finish_reason": finishReason})
	}
	value := map[string]any{"id": "", "object": "text_completion", "created": int64(0), "model": request.PublicModel, "choices": choices}
	if event.TextCompletion != nil {
		value["id"] = event.TextCompletion.ID
		value["created"] = event.TextCompletion.Created
	}
	if includeUsage {
		value["usage"] = nil
		if event.Usage != nil {
			value["usage"] = encodeDetailedUsage(*event.Usage)
		}
	}
	return json.Marshal(value)
}

func decodeOptionalJSON(raw json.RawMessage) (any, error) {
	if len(bytes.TrimSpace(raw)) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil, nil
	}
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		return nil, err
	}
	return value, nil
}
