package contract

import (
	"context"
	"time"
)

// ID is a stable host-provided identifier. It is opaque to the gateway.
type ID string

type Operation string

const (
	OperationChat            Operation = "chat"
	OperationTextCompletion  Operation = "text_completion"
	OperationResponses       Operation = "responses"
	OperationResponseCompact Operation = "response_compaction"
	OperationEmbeddings      Operation = "embeddings"
	OperationImageGeneration Operation = "image_generation"
	OperationImageEdit       Operation = "image_edit"
	OperationImageVariation  Operation = "image_variation"
	OperationModeration      Operation = "moderation"
	OperationRerank          Operation = "rerank"
	OperationAudioSpeech     Operation = "audio_speech"
	OperationTranscription   Operation = "audio_transcription"
	OperationTranslation     Operation = "audio_translation"
	OperationSearch          Operation = "search"
	OperationOCR             Operation = "ocr"
)

func (o Operation) Valid() bool {
	switch o {
	case OperationChat, OperationTextCompletion, OperationResponses, OperationResponseCompact, OperationEmbeddings, OperationImageGeneration, OperationImageEdit, OperationImageVariation, OperationModeration, OperationRerank,
		OperationAudioSpeech, OperationTranscription, OperationTranslation, OperationSearch, OperationOCR:
		return true
	default:
		return false
	}
}

type Principal struct {
	KeyID           ID
	KeyPrefix       string
	OwnerID         ID
	TeamID          ID
	GroupIDs        []ID
	AllowedModelIDs []ID
}

type Request struct {
	ID              ID
	Operation       Operation
	PublicModel     string
	Stream          bool
	MaxOutputTokens int64
	EstimatedUsage  Usage
	Chat            *ChatRequest
	TextCompletion  *TextCompletionRequest
	Responses       *ResponsesRequest
	Embeddings      *EmbeddingsRequest
	ImageGeneration *ImageGenerationRequest
	ImageEdit       *ImageEditRequest
	ImageVariation  *ImageVariationRequest
	Moderation      *ModerationRequest
	Rerank          *RerankRequest
	AudioSpeech     *AudioSpeechRequest
	Transcription   *AudioTranscriptionRequest
	Search          *SearchRequest
	OCR             *OCRRequest
	StartedAt       time.Time
}

type Response struct {
	RequestID      ID
	Model          string
	Chat           *ChatResponse
	TextCompletion *TextCompletionResponse
	Responses      *ResponsesResponse
	Embeddings     *EmbeddingsResponse
	Images         *ImageResponse
	Moderation     *ModerationResponse
	Rerank         *RerankResponse
	AudioSpeech    *AudioSpeechResponse
	Transcription  *AudioTranscriptionResponse
	Search         *SearchResponse
	OCR            *OCRResponse
	Usage          Usage
}

type StreamEvent struct {
	Type            string
	Chat            *ChatDelta
	TextCompletion  *TextCompletionDelta
	Response        *ResponseDelta
	Usage           *Usage
	Terminal        bool
	OutputCommitted bool
}

// EventStream is the protocol-neutral streaming response contract.
type EventStream interface {
	Next(context.Context) (StreamEvent, error)
	Close(context.Context) error
}

type Usage struct {
	InputTokens      int64
	OutputTokens     int64
	CacheReadTokens  int64
	CacheWriteTokens int64
	ReasoningTokens  int64
	TotalTokens      int64
	CostMicros       int64
	Estimated        bool
	Measures         map[string]int64
}

func (u Usage) Valid() bool {
	if u.InputTokens < 0 || u.OutputTokens < 0 || u.CacheReadTokens < 0 || u.CacheWriteTokens < 0 || u.ReasoningTokens < 0 || u.TotalTokens < 0 || u.CostMicros < 0 {
		return false
	}
	for name, value := range u.Measures {
		if !ValidMeasureName(name) || value < 0 || canonicalUsageMeasure(name) {
			return false
		}
	}
	return true
}

// ValidMeasureName reports whether name can be used in metering and pricing.
func ValidMeasureName(name string) bool {
	if name == "" || len(name) > 100 {
		return false
	}
	for _, character := range name {
		if (character >= 'a' && character <= 'z') || (character >= '0' && character <= '9') || character == '_' || character == '-' || character == '.' {
			continue
		}
		return false
	}
	return true
}

func canonicalUsageMeasure(name string) bool {
	switch name {
	case "input_tokens", "output_tokens", "cache_read_tokens", "cache_write_tokens", "reasoning_tokens", "total_tokens", "cost_micros":
		return true
	default:
		return false
	}
}

// AllMeasures returns an independent copy containing both canonical token/cost
// facts and supplemental workload measures.
func (u Usage) AllMeasures() map[string]int64 {
	measures := make(map[string]int64, len(u.Measures)+7)
	for name, value := range u.Measures {
		measures[name] = value
	}
	for name, value := range map[string]int64{
		"input_tokens": u.InputTokens, "output_tokens": u.OutputTokens,
		"cache_read_tokens": u.CacheReadTokens, "cache_write_tokens": u.CacheWriteTokens,
		"reasoning_tokens": u.ReasoningTokens, "total_tokens": u.TotalTokens, "cost_micros": u.CostMicros,
	} {
		measures[name] = value
	}
	return measures
}

type Attempt struct {
	Number          int
	ProviderID      ID
	DeploymentID    ID
	UpstreamRequest string
	StartedAt       time.Time
	FirstByteAt     time.Time
	EndedAt         time.Time
	Outcome         string
	ErrorCode       ErrorCode
	HTTPStatus      int
	Retryable       bool
	OutputCommitted bool
	Usage           Usage
}

type Admission struct {
	RequestID      ID
	Principal      Principal
	ModelID        ID
	Operation      Operation
	StartedAt      time.Time
	EstimatedUsage Usage
}

type ReservationToken struct {
	RequestID ID
	Opaque    string
}

type Completion struct {
	Token       ReservationToken
	Status      string
	HTTPStatus  int
	ErrorCode   ErrorCode
	Usage       Usage
	Attempts    []Attempt
	FirstByteAt time.Time
	EndedAt     time.Time
	CacheStatus string
}

type Cancellation struct {
	Token    ReservationToken
	Reason   string
	Usage    Usage
	Attempts []Attempt
	EndedAt  time.Time
}
