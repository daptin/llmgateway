package contract

import "encoding/json"

type ChatRequest struct {
	Messages            []Message
	Tools               []Tool
	ToolChoice          *ToolChoice
	ResponseFormat      *ResponseFormat
	N                   int
	Temperature         *float64
	TopP                *float64
	FrequencyPenalty    *float64
	PresencePenalty     *float64
	MaxCompletionTokens int64
	Stop                []string
	User                string
	Seed                *int64
	Logprobs            bool
	TopLogprobs         int
	ParallelToolCalls   *bool
	ReasoningEffort     string
	Store               *bool
	PromptCacheKey      string
}

type Message struct {
	Role       string
	Name       string
	Content    []ContentPart
	ToolCalls  []ToolCall
	ToolCallID string
}

type ContentPart struct {
	Type        string
	Text        string
	Refusal     string
	Annotations json.RawMessage
	Logprobs    json.RawMessage
	ImageURL    *ImageURL
	Audio       *InputAudio
	File        *InputFile
}

type ImageURL struct {
	URL    string
	Detail string
}

type InputAudio struct {
	Data   string
	Format string
}

type InputFile struct {
	Data     string
	Filename string
}

type Tool struct {
	Type     string
	Function FunctionDefinition
}

type FunctionDefinition struct {
	Name        string
	Description string
	Parameters  json.RawMessage
	Strict      *bool
}

type ToolChoice struct {
	Mode         string
	FunctionName string
}

type ResponseFormat struct {
	Type       string
	JSONSchema *JSONSchema
}

type JSONSchema struct {
	Name        string
	Description string
	Schema      json.RawMessage
	Strict      *bool
}

type ToolCall struct {
	ID       string
	Type     string
	Function FunctionCall
}

type FunctionCall struct {
	Name      string
	Arguments string
}

type ChatResponse struct {
	ID      string
	Created int64
	Choices []ChatChoice
}

type ChatChoice struct {
	Index        int
	Message      Message
	FinishReason string
	Logprobs     json.RawMessage
}

type ChatDelta struct {
	ID           string
	Created      int64
	Index        int
	Role         string
	Content      string
	ToolCalls    []ToolCallDelta
	FinishReason string
	Logprobs     json.RawMessage
}

type ToolCallDelta struct {
	Index    int
	ID       string
	Type     string
	Function FunctionCall
}
