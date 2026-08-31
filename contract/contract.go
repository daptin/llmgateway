package contract

import (
	"time"
)

// ID is a stable host-provided identifier. It is opaque to the gateway.
type ID string

type Operation string

const (
	OperationChat            Operation = "chat"
	OperationResponses       Operation = "responses"
	OperationEmbeddings      Operation = "embeddings"
	OperationImageGeneration Operation = "image_generation"
)

func (o Operation) Valid() bool {
	switch o {
	case OperationChat, OperationResponses, OperationEmbeddings, OperationImageGeneration:
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
	PolicyBindings  []PolicyBinding
}

type PolicyBinding struct {
	ScopeKind string
	ScopeID   ID
	PolicyID  ID
}

type Request struct {
	ID              ID
	Operation       Operation
	PublicModel     string
	Stream          bool
	MaxOutputTokens int64
	EstimatedUsage  Usage
	Chat            *ChatRequest
	Responses       *ResponsesRequest
	Embeddings      *EmbeddingsRequest
	ImageGeneration *ImageGenerationRequest
	StartedAt       time.Time
}

type Response struct {
	RequestID       ID
	Model           string
	Chat            *ChatResponse
	Responses       *ResponsesResponse
	Embeddings      *EmbeddingsResponse
	ImageGeneration *ImageGenerationResponse
	Usage           Usage
}

type StreamEvent struct {
	Type            string
	Chat            *ChatDelta
	Response        *ResponseDelta
	Usage           *Usage
	Terminal        bool
	OutputCommitted bool
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
}

func (u Usage) Valid() bool {
	return u.InputTokens >= 0 && u.OutputTokens >= 0 &&
		u.CacheReadTokens >= 0 && u.CacheWriteTokens >= 0 &&
		u.ReasoningTokens >= 0 && u.TotalTokens >= 0 && u.CostMicros >= 0
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
	RequestID         ID
	Principal         Principal
	ModelID           ID
	Operation         Operation
	StartedAt         time.Time
	EstimatedUsage    Usage
	LimitReservations []LimitReservation
}

type LimitReservation struct {
	ScopeKind   string
	ScopeID     ID
	PolicyID    ID
	Metric      string
	Window      string
	WindowStart time.Time
	WindowEnd   time.Time
	Maximum     int64
	Amount      int64
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

type ReapResult struct {
	Examined int
	Released int
}
