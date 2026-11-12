package contract

import "encoding/json"

type ResponsesRequest struct {
	Store                bool
	PreviousResponseID   string
	Instructions         string
	Input                []ResponseInputItem
	Include              []string
	Tools                []Tool
	ToolChoice           *ToolChoice
	TextFormat           *ResponseFormat
	Temperature          *float64
	TopP                 *float64
	ParallelToolCalls    *bool
	ReasoningEffort      string
	ReasoningSummary     string
	PromptCacheKey       string
	PromptCacheMode      string
	PromptCacheTTL       string
	PromptCacheRetention string
	SafetyIdentifier     string
	ServiceTier          string
	Truncation           string
	User                 string
	TopLogprobs          *int
}

type ResponseInputItem struct {
	ID               string
	Type             string
	Role             string
	Content          []ContentPart
	CallID           string
	Name             string
	Arguments        string
	Output           string
	EncryptedContent string
	Summary          []ContentPart
	Status           string
	Action           json.RawMessage
}

type ResponsesResponse struct {
	Store              bool
	PreviousResponseID string
	ID                 string
	Object             string
	CreatedAt          int64
	Status             string
	Output             []ResponseOutputItem
}

type ResponseOutputItem struct {
	Type             string
	ID               string
	Role             string
	Content          []ContentPart
	Summary          []ContentPart
	CallID           string
	Name             string
	Arguments        string
	Status           string
	EncryptedContent string
	CreatedBy        string
	Action           json.RawMessage
}

type ResponseDelta struct {
	ResponseID   string
	Sequence     int64
	ItemID       string
	OutputIndex  int64
	ContentIndex int64
	SummaryIndex int64
	Item         *ResponseOutputItem
	Part         *ContentPart
	Delta        string
	Text         string
	Refusal      string
	Arguments    string
	Name         string
	Status       string
	Logprobs     json.RawMessage
	Snapshot     *Response
}
