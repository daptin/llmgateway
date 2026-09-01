package contract

import "encoding/json"

type RerankRequest struct {
	Query           string
	Documents       []RerankDocument
	TopN            int
	RankFields      []string
	ReturnDocuments *bool
	MaxChunksPerDoc int
	MaxTokensPerDoc int
	Instruction     string
}

type RerankDocument struct {
	Text   string
	Object json.RawMessage
}

type RerankResponse struct {
	ID      string
	Results []RerankResult
	Meta    RerankMeta
}

type RerankResult struct {
	Index          int
	RelevanceScore float64
	Document       *RerankDocument
}

type RerankMeta struct {
	SearchUnits  int64
	InputTokens  int64
	OutputTokens int64
}
