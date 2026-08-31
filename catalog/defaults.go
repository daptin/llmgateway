package catalog

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/daptin/llmgateway/contract"
)

type modelDefaults struct {
	Chat            chatDefaults            `json:"chat,omitempty"`
	Responses       responsesDefaults       `json:"responses,omitempty"`
	Embeddings      embeddingsDefaults      `json:"embeddings,omitempty"`
	ImageGeneration imageGenerationDefaults `json:"image_generation,omitempty"`
}

type chatDefaults struct {
	N                   *int     `json:"n,omitempty"`
	Temperature         *float64 `json:"temperature,omitempty"`
	TopP                *float64 `json:"top_p,omitempty"`
	MaxCompletionTokens *int64   `json:"max_completion_tokens,omitempty"`
	Stop                []string `json:"stop,omitempty"`
	Seed                *int64   `json:"seed,omitempty"`
}

type responsesDefaults struct {
	MaxOutputTokens *int64 `json:"max_output_tokens,omitempty"`
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
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var defaults modelDefaults
	if err := decoder.Decode(&defaults); err != nil {
		return modelDefaults{}, fmt.Errorf("model %q default parameters: %w", model.ID, err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return modelDefaults{}, fmt.Errorf("model %q default parameters must contain one JSON object", model.ID)
	}
	if err := defaults.validate(); err != nil {
		return modelDefaults{}, fmt.Errorf("model %q default parameters: %w", model.ID, err)
	}
	for operation, present := range map[contract.Operation]bool{
		contract.OperationChat: defaults.Chat.present(), contract.OperationResponses: defaults.Responses.present(),
		contract.OperationEmbeddings: defaults.Embeddings.present(), contract.OperationImageGeneration: defaults.ImageGeneration.present(),
	} {
		if present && !supportsOperation(model.Operations, operation) {
			return modelDefaults{}, fmt.Errorf("model %q configures defaults for undeclared operation %q", model.ID, operation)
		}
	}
	if defaults.Embeddings.Dimensions != nil && !model.Capabilities["dimensions"] {
		return modelDefaults{}, fmt.Errorf("model %q embedding dimensions default requires the dimensions capability", model.ID)
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
	if d.Chat.Stop != nil && (len(d.Chat.Stop) == 0 || len(d.Chat.Stop) > 4) {
		return errors.New("chat stop default supports at most four values")
	}
	for _, stop := range d.Chat.Stop {
		if stop == "" {
			return errors.New("chat stop defaults cannot be empty")
		}
	}
	if (d.Responses.MaxOutputTokens != nil && *d.Responses.MaxOutputTokens < 1) || (d.Embeddings.Dimensions != nil && *d.Embeddings.Dimensions < 1) {
		return errors.New("response token and embedding dimension defaults cannot be negative")
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
	return d.N != nil || d.Temperature != nil || d.TopP != nil || d.MaxCompletionTokens != nil || d.Stop != nil || d.Seed != nil
}
func (d responsesDefaults) present() bool  { return d.MaxOutputTokens != nil }
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
		request.MaxOutputTokens = request.Chat.MaxCompletionTokens
	case contract.OperationResponses:
		if request.MaxOutputTokens == 0 {
			if defaults.Responses.MaxOutputTokens != nil {
				request.MaxOutputTokens = *defaults.Responses.MaxOutputTokens
			} else {
				request.MaxOutputTokens = defaultMaxOutputTokens
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
