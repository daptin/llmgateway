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
	MaxCompletionTokens int64
	Stop                []string
	User                string
	Seed                *int64
	Logprobs            bool
	TopLogprobs         int
}

type Message struct {
	Role       string
	Name       string
	Content    []ContentPart
	ToolCalls  []ToolCall
	ToolCallID string
}

type ContentPart struct {
	Type     string
	Text     string
	ImageURL *ImageURL
	Audio    *InputAudio
}

type ImageURL struct {
	URL    string
	Detail string
}

type InputAudio struct {
	Data   string
	Format string
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
