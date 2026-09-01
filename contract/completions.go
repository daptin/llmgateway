package contract

import "encoding/json"

type CompletionPrompt struct {
	Texts  []string
	Tokens [][]int64
}

type TextCompletionRequest struct {
	Prompt           CompletionPrompt
	BestOf           int
	Echo             *bool
	FrequencyPenalty *float64
	LogitBias        map[string]int
	Logprobs         *int
	MaxTokens        int64
	N                int
	PresencePenalty  *float64
	Seed             *int64
	Stop             []string
	Suffix           string
	Temperature      *float64
	TopP             *float64
	User             string
}

type TextCompletionResponse struct {
	ID      string
	Created int64
	Choices []TextCompletionChoice
}

type TextCompletionChoice struct {
	Text         string
	Index        int
	Logprobs     json.RawMessage
	FinishReason string
}

type TextCompletionDelta struct {
	ID           string
	Created      int64
	Text         string
	Index        int
	Logprobs     json.RawMessage
	FinishReason string
}
