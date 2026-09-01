package catalog

import (
	"bytes"
	"errors"
	"fmt"

	"github.com/daptin/llmgateway/contract"
	"github.com/daptin/llmgateway/internal/jsonx"
)

type modelDefaults struct {
	Chat            chatDefaults            `json:"chat,omitempty"`
	TextCompletion  textCompletionDefaults  `json:"text_completion,omitempty"`
	Responses       responsesDefaults       `json:"responses,omitempty"`
	Embeddings      embeddingsDefaults      `json:"embeddings,omitempty"`
	ImageGeneration imageGenerationDefaults `json:"image_generation,omitempty"`
}

type textCompletionDefaults struct {
	BestOf           *int     `json:"best_of,omitempty"`
	Echo             *bool    `json:"echo,omitempty"`
	FrequencyPenalty *float64 `json:"frequency_penalty,omitempty"`
	MaxTokens        *int64   `json:"max_tokens,omitempty"`
	N                *int     `json:"n,omitempty"`
	PresencePenalty  *float64 `json:"presence_penalty,omitempty"`
	Seed             *int64   `json:"seed,omitempty"`
	Stop             []string `json:"stop,omitempty"`
	Suffix           *string  `json:"suffix,omitempty"`
	Temperature      *float64 `json:"temperature,omitempty"`
	TopP             *float64 `json:"top_p,omitempty"`
}

type chatDefaults struct {
	N                   *int     `json:"n,omitempty"`
	Temperature         *float64 `json:"temperature,omitempty"`
	TopP                *float64 `json:"top_p,omitempty"`
	FrequencyPenalty    *float64 `json:"frequency_penalty,omitempty"`
	PresencePenalty     *float64 `json:"presence_penalty,omitempty"`
	MaxCompletionTokens *int64   `json:"max_completion_tokens,omitempty"`
	Stop                []string `json:"stop,omitempty"`
	Seed                *int64   `json:"seed,omitempty"`
	ParallelToolCalls   *bool    `json:"parallel_tool_calls,omitempty"`
	ReasoningEffort     *string  `json:"reasoning_effort,omitempty"`
}

type responsesDefaults struct {
	MaxOutputTokens   *int64   `json:"max_output_tokens,omitempty"`
	Temperature       *float64 `json:"temperature,omitempty"`
	TopP              *float64 `json:"top_p,omitempty"`
	ParallelToolCalls *bool    `json:"parallel_tool_calls,omitempty"`
	ReasoningEffort   *string  `json:"reasoning_effort,omitempty"`
}

type embeddingsDefaults struct {
	Dimensions     *int    `json:"dimensions,omitempty"`
	EncodingFormat *string `json:"encoding_format,omitempty"`
}

type imageGenerationDefaults struct {
	N              *int    `json:"n,omitempty"`
	Size           *string `json:"size,omitempty"`
	Quality        *string `json:"quality,omitempty"`
	ResponseFormat *string `json:"response_format,omitempty"`
}

func parseModelDefaults(model Model) (modelDefaults, error) {
	raw := bytes.TrimSpace(model.DefaultParameters)
	if len(raw) == 0 {
		return modelDefaults{}, nil
	}
	var defaults modelDefaults
	if err := jsonx.DecodeOne(bytes.NewReader(raw), &defaults); err != nil {
		return modelDefaults{}, fmt.Errorf("model %q default parameters: %w", model.ID, err)
	}
	if err := defaults.validate(); err != nil {
		return modelDefaults{}, fmt.Errorf("model %q default parameters: %w", model.ID, err)
	}
	for operation, present := range map[contract.Operation]bool{
		contract.OperationChat: defaults.Chat.present(), contract.OperationResponses: defaults.Responses.present(),
		contract.OperationTextCompletion: defaults.TextCompletion.present(),
		contract.OperationEmbeddings:     defaults.Embeddings.present(), contract.OperationImageGeneration: defaults.ImageGeneration.present(),
	} {
		if present && !supportsOperation(model.Operations, operation) {
			return modelDefaults{}, fmt.Errorf("model %q configures defaults for undeclared operation %q", model.ID, operation)
		}
	}
	if defaults.Embeddings.Dimensions != nil && !model.Capabilities["dimensions"] {
		return modelDefaults{}, fmt.Errorf("model %q embedding dimensions default requires the dimensions capability", model.ID)
	}
	for capability, configured := range map[string]bool{
		"penalties": defaults.Chat.FrequencyPenalty != nil || defaults.Chat.PresencePenalty != nil ||
			defaults.TextCompletion.FrequencyPenalty != nil || defaults.TextCompletion.PresencePenalty != nil,
		"parallel_tools": defaults.Chat.ParallelToolCalls != nil || defaults.Responses.ParallelToolCalls != nil,
		"reasoning":      defaults.Chat.ReasoningEffort != nil || defaults.Responses.ReasoningEffort != nil,
	} {
		if configured && !model.Capabilities[capability] {
			return modelDefaults{}, fmt.Errorf("model %q defaults require the %s capability", model.ID, capability)
		}
	}
	return defaults, nil
}

func (d modelDefaults) validate() error {
	if (d.Chat.N != nil && (*d.Chat.N < 1 || *d.Chat.N > 128)) || (d.Chat.MaxCompletionTokens != nil && *d.Chat.MaxCompletionTokens < 1) {
		return errors.New("chat n and max_completion_tokens defaults are out of range")
	}
	if d.Chat.Temperature != nil && (*d.Chat.Temperature < 0 || *d.Chat.Temperature > 2) {
		return errors.New("chat temperature default must be between 0 and 2")
	}
	if d.Chat.TopP != nil && (*d.Chat.TopP < 0 || *d.Chat.TopP > 1) {
		return errors.New("chat top_p default must be between 0 and 1")
	}
	if (d.Chat.FrequencyPenalty != nil && (*d.Chat.FrequencyPenalty < -2 || *d.Chat.FrequencyPenalty > 2)) ||
		(d.Chat.PresencePenalty != nil && (*d.Chat.PresencePenalty < -2 || *d.Chat.PresencePenalty > 2)) {
		return errors.New("chat penalty defaults must be between -2 and 2")
	}
	if d.Chat.ReasoningEffort != nil && *d.Chat.ReasoningEffort != "none" && *d.Chat.ReasoningEffort != "minimal" &&
		*d.Chat.ReasoningEffort != "low" && *d.Chat.ReasoningEffort != "medium" && *d.Chat.ReasoningEffort != "high" && *d.Chat.ReasoningEffort != "xhigh" {
		return errors.New("chat reasoning_effort default is invalid")
	}
	if d.Chat.Stop != nil && (len(d.Chat.Stop) == 0 || len(d.Chat.Stop) > 4) {
		return errors.New("chat stop default supports at most four values")
	}
	for _, stop := range d.Chat.Stop {
		if stop == "" {
			return errors.New("chat stop defaults cannot be empty")
		}
	}
	if (d.TextCompletion.N != nil && (*d.TextCompletion.N < 1 || *d.TextCompletion.N > 128)) ||
		(d.TextCompletion.BestOf != nil && (*d.TextCompletion.BestOf < 1 || *d.TextCompletion.BestOf > 128)) ||
		(d.TextCompletion.MaxTokens != nil && *d.TextCompletion.MaxTokens < 1) {
		return errors.New("text completion choice and token defaults are out of range")
	}
	if d.TextCompletion.N != nil && d.TextCompletion.BestOf != nil && *d.TextCompletion.BestOf < *d.TextCompletion.N {
		return errors.New("text completion best_of default cannot be less than n")
	}
	if !validDefaultRange(d.TextCompletion.Temperature, 0, 2) || !validDefaultRange(d.TextCompletion.TopP, 0, 1) ||
		!validDefaultRange(d.TextCompletion.FrequencyPenalty, -2, 2) || !validDefaultRange(d.TextCompletion.PresencePenalty, -2, 2) {
		return errors.New("text completion sampling defaults are out of range")
	}
	if d.TextCompletion.Stop != nil && (len(d.TextCompletion.Stop) == 0 || len(d.TextCompletion.Stop) > 4) {
		return errors.New("text completion stop default supports at most four values")
	}
	for _, stop := range d.TextCompletion.Stop {
		if stop == "" {
			return errors.New("text completion stop defaults cannot be empty")
		}
	}
	if (d.Responses.MaxOutputTokens != nil && *d.Responses.MaxOutputTokens < 1) || (d.Embeddings.Dimensions != nil && *d.Embeddings.Dimensions < 1) {
		return errors.New("response token and embedding dimension defaults cannot be negative")
	}
	if !validDefaultRange(d.Responses.Temperature, 0, 2) || !validDefaultRange(d.Responses.TopP, 0, 1) {
		return errors.New("responses sampling defaults are out of range")
	}
	if d.Responses.ReasoningEffort != nil && *d.Responses.ReasoningEffort != "none" && *d.Responses.ReasoningEffort != "minimal" &&
		*d.Responses.ReasoningEffort != "low" && *d.Responses.ReasoningEffort != "medium" && *d.Responses.ReasoningEffort != "high" && *d.Responses.ReasoningEffort != "xhigh" {
		return errors.New("responses reasoning_effort default is invalid")
	}
	if d.Embeddings.EncodingFormat != nil && *d.Embeddings.EncodingFormat != "float" && *d.Embeddings.EncodingFormat != "base64" {
		return errors.New("embedding encoding_format default must be float or base64")
	}
	if d.ImageGeneration.N != nil && (*d.ImageGeneration.N < 1 || *d.ImageGeneration.N > 10) {
		return errors.New("image n default must be between 1 and 10")
	}
	if d.ImageGeneration.ResponseFormat != nil && *d.ImageGeneration.ResponseFormat != "url" && *d.ImageGeneration.ResponseFormat != "b64_json" {
		return errors.New("image response_format default must be url or b64_json")
	}
	if (d.ImageGeneration.Size != nil && *d.ImageGeneration.Size == "") || (d.ImageGeneration.Quality != nil && *d.ImageGeneration.Quality == "") {
		return errors.New("image size and quality defaults cannot be empty")
	}
	return nil
}

func (d chatDefaults) present() bool {
	return d.N != nil || d.Temperature != nil || d.TopP != nil || d.FrequencyPenalty != nil || d.PresencePenalty != nil ||
		d.MaxCompletionTokens != nil || d.Stop != nil || d.Seed != nil || d.ParallelToolCalls != nil || d.ReasoningEffort != nil
}
func (d textCompletionDefaults) present() bool {
	return d.BestOf != nil || d.Echo != nil || d.FrequencyPenalty != nil || d.MaxTokens != nil || d.N != nil ||
		d.PresencePenalty != nil || d.Seed != nil || d.Stop != nil || d.Suffix != nil || d.Temperature != nil || d.TopP != nil
}
func (d responsesDefaults) present() bool {
	return d.MaxOutputTokens != nil || d.Temperature != nil || d.TopP != nil || d.ParallelToolCalls != nil || d.ReasoningEffort != nil
}
func (d embeddingsDefaults) present() bool { return d.Dimensions != nil || d.EncodingFormat != nil }
func (d imageGenerationDefaults) present() bool {
	return d.N != nil || d.Size != nil || d.Quality != nil || d.ResponseFormat != nil
}

func supportsOperation(operations []contract.Operation, expected contract.Operation) bool {
	for _, operation := range operations {
		if operation == expected {
			return true
		}
	}
	return false
}

// ApplyDefaults normalizes one request using the defaults compiled for its
// public model. Callers pass the engine-wide output bound as the final fallback.
func (s *Snapshot) ApplyDefaults(modelID contract.ID, request contract.Request, defaultMaxOutputTokens int64) (contract.Request, error) {
	defaults, ok := s.defaults[modelID]
	if !ok {
		return contract.Request{}, fmt.Errorf("unknown model %q", modelID)
	}
	if defaultMaxOutputTokens < 1 {
		return contract.Request{}, errors.New("default maximum output tokens must be positive")
	}
	switch request.Operation {
	case contract.OperationChat:
		if request.Chat == nil {
			return request, nil
		}
		chat := *request.Chat
		request.Chat = &chat
		if request.Chat.N == 0 {
			if defaults.Chat.N != nil {
				request.Chat.N = *defaults.Chat.N
			} else {
				request.Chat.N = 1
			}
		}
		if request.Chat.Temperature == nil {
			request.Chat.Temperature = cloneFloat(defaults.Chat.Temperature)
		}
		if request.Chat.TopP == nil {
			request.Chat.TopP = cloneFloat(defaults.Chat.TopP)
		}
		if request.Chat.FrequencyPenalty == nil {
			request.Chat.FrequencyPenalty = cloneFloat(defaults.Chat.FrequencyPenalty)
		}
		if request.Chat.PresencePenalty == nil {
			request.Chat.PresencePenalty = cloneFloat(defaults.Chat.PresencePenalty)
		}
		if request.Chat.MaxCompletionTokens == 0 {
			if defaults.Chat.MaxCompletionTokens != nil {
				request.Chat.MaxCompletionTokens = *defaults.Chat.MaxCompletionTokens
			} else {
				request.Chat.MaxCompletionTokens = defaultMaxOutputTokens
			}
		}
		if len(request.Chat.Stop) == 0 && len(defaults.Chat.Stop) != 0 {
			request.Chat.Stop = append([]string(nil), defaults.Chat.Stop...)
		}
		if request.Chat.Seed == nil && defaults.Chat.Seed != nil {
			seed := *defaults.Chat.Seed
			request.Chat.Seed = &seed
		}
		if request.Chat.ParallelToolCalls == nil && defaults.Chat.ParallelToolCalls != nil {
			value := *defaults.Chat.ParallelToolCalls
			request.Chat.ParallelToolCalls = &value
		}
		if request.Chat.ReasoningEffort == "" && defaults.Chat.ReasoningEffort != nil {
			request.Chat.ReasoningEffort = *defaults.Chat.ReasoningEffort
		}
		request.MaxOutputTokens = request.Chat.MaxCompletionTokens
	case contract.OperationTextCompletion:
		if request.TextCompletion == nil {
			return request, nil
		}
		completion := *request.TextCompletion
		request.TextCompletion = &completion
		if completion.N == 0 {
			if defaults.TextCompletion.N != nil {
				request.TextCompletion.N = *defaults.TextCompletion.N
			} else {
				request.TextCompletion.N = 1
			}
		}
		if request.TextCompletion.BestOf == 0 {
			if defaults.TextCompletion.BestOf != nil {
				request.TextCompletion.BestOf = *defaults.TextCompletion.BestOf
			} else {
				request.TextCompletion.BestOf = request.TextCompletion.N
			}
		}
		if request.TextCompletion.MaxTokens == 0 {
			if defaults.TextCompletion.MaxTokens != nil {
				request.TextCompletion.MaxTokens = *defaults.TextCompletion.MaxTokens
			} else {
				request.TextCompletion.MaxTokens = defaultMaxOutputTokens
			}
		}
		if request.TextCompletion.Temperature == nil {
			request.TextCompletion.Temperature = cloneFloat(defaults.TextCompletion.Temperature)
		}
		if request.TextCompletion.TopP == nil {
			request.TextCompletion.TopP = cloneFloat(defaults.TextCompletion.TopP)
		}
		if request.TextCompletion.FrequencyPenalty == nil {
			request.TextCompletion.FrequencyPenalty = cloneFloat(defaults.TextCompletion.FrequencyPenalty)
		}
		if request.TextCompletion.PresencePenalty == nil {
			request.TextCompletion.PresencePenalty = cloneFloat(defaults.TextCompletion.PresencePenalty)
		}
		if len(request.TextCompletion.Stop) == 0 && len(defaults.TextCompletion.Stop) != 0 {
			request.TextCompletion.Stop = append([]string(nil), defaults.TextCompletion.Stop...)
		}
		if request.TextCompletion.Seed == nil && defaults.TextCompletion.Seed != nil {
			seed := *defaults.TextCompletion.Seed
			request.TextCompletion.Seed = &seed
		}
		if request.TextCompletion.Echo == nil && defaults.TextCompletion.Echo != nil {
			echo := *defaults.TextCompletion.Echo
			request.TextCompletion.Echo = &echo
		}
		if request.TextCompletion.Suffix == "" && defaults.TextCompletion.Suffix != nil {
			request.TextCompletion.Suffix = *defaults.TextCompletion.Suffix
		}
		request.MaxOutputTokens = request.TextCompletion.MaxTokens
	case contract.OperationResponses:
		if request.MaxOutputTokens == 0 {
			if defaults.Responses.MaxOutputTokens != nil {
				request.MaxOutputTokens = *defaults.Responses.MaxOutputTokens
			} else {
				request.MaxOutputTokens = defaultMaxOutputTokens
			}
		}
		if request.Responses != nil {
			responses := *request.Responses
			request.Responses = &responses
			if request.Responses.Temperature == nil {
				request.Responses.Temperature = cloneFloat(defaults.Responses.Temperature)
			}
			if request.Responses.TopP == nil {
				request.Responses.TopP = cloneFloat(defaults.Responses.TopP)
			}
			if request.Responses.ParallelToolCalls == nil && defaults.Responses.ParallelToolCalls != nil {
				value := *defaults.Responses.ParallelToolCalls
				request.Responses.ParallelToolCalls = &value
			}
			if request.Responses.ReasoningEffort == "" && defaults.Responses.ReasoningEffort != nil {
				request.Responses.ReasoningEffort = *defaults.Responses.ReasoningEffort
			}
		}
	case contract.OperationEmbeddings:
		if request.Embeddings == nil {
			return request, nil
		}
		embeddings := *request.Embeddings
		request.Embeddings = &embeddings
		if request.Embeddings.Dimensions == 0 {
			if defaults.Embeddings.Dimensions != nil {
				request.Embeddings.Dimensions = *defaults.Embeddings.Dimensions
			}
		}
		if request.Embeddings.EncodingFormat == "" {
			if defaults.Embeddings.EncodingFormat != nil {
				request.Embeddings.EncodingFormat = *defaults.Embeddings.EncodingFormat
			} else {
				request.Embeddings.EncodingFormat = "float"
			}
		}
	case contract.OperationImageGeneration:
		if request.ImageGeneration == nil {
			return request, nil
		}
		image := *request.ImageGeneration
		request.ImageGeneration = &image
		if request.ImageGeneration.N == 0 {
			if defaults.ImageGeneration.N != nil {
				request.ImageGeneration.N = *defaults.ImageGeneration.N
			} else {
				request.ImageGeneration.N = 1
			}
		}
		if request.ImageGeneration.Size == "" && defaults.ImageGeneration.Size != nil {
			request.ImageGeneration.Size = *defaults.ImageGeneration.Size
		}
		if request.ImageGeneration.Quality == "" && defaults.ImageGeneration.Quality != nil {
			request.ImageGeneration.Quality = *defaults.ImageGeneration.Quality
		}
		if request.ImageGeneration.ResponseFormat == "" {
			if defaults.ImageGeneration.ResponseFormat != nil {
				request.ImageGeneration.ResponseFormat = *defaults.ImageGeneration.ResponseFormat
			} else {
				request.ImageGeneration.ResponseFormat = "url"
			}
		}
	case contract.OperationImageEdit:
		if request.ImageEdit == nil {
			return request, nil
		}
		edit := *request.ImageEdit
		request.ImageEdit = &edit
		if edit.N == 0 {
			request.ImageEdit.N = 1
		}
		if edit.ResponseFormat == "" {
			request.ImageEdit.ResponseFormat = "url"
		}
	case contract.OperationImageVariation:
		if request.ImageVariation == nil {
			return request, nil
		}
		variation := *request.ImageVariation
		request.ImageVariation = &variation
		if variation.N == 0 {
			request.ImageVariation.N = 1
		}
		if variation.ResponseFormat == "" {
			request.ImageVariation.ResponseFormat = "url"
		}
	case contract.OperationRerank:
		if request.Rerank != nil && request.Rerank.TopN == 0 {
			rerank := *request.Rerank
			rerank.TopN = len(rerank.Documents)
			request.Rerank = &rerank
		}
	case contract.OperationAudioSpeech:
		if request.AudioSpeech != nil && request.AudioSpeech.ResponseFormat == "" {
			speech := *request.AudioSpeech
			speech.ResponseFormat = "mp3"
			request.AudioSpeech = &speech
		}
	case contract.OperationTranscription, contract.OperationTranslation:
		if request.Transcription != nil && request.Transcription.ResponseFormat == "" {
			transcription := *request.Transcription
			transcription.ResponseFormat = "json"
			request.Transcription = &transcription
		}
	}
	return request, nil
}

func cloneFloat(value *float64) *float64 {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}

func validDefaultRange(value *float64, minimum, maximum float64) bool {
	return value == nil || (*value >= minimum && *value <= maximum)
}
