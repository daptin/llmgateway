package contract

import "encoding/json"

type ResponsesRequest struct {
	Instructions string
	Input        []ResponseInputItem
	Tools        []Tool
	ToolChoice   *ToolChoice
	TextFormat   *ResponseFormat
}

type ResponseInputItem struct {
	Type      string
	Role      string
	Content   []ContentPart
	CallID    string
	Name      string
	Arguments string
	Output    string
}

type ResponsesResponse struct {
	ID     string
	Status string
	Output []ResponseOutputItem
}

type ResponseOutputItem struct {
	Type      string
	ID        string
	Role      string
	Content   []ContentPart
	Summary   []ContentPart
	CallID    string
	Name      string
	Arguments string
	Status    string
}

type ResponseDelta struct {
	ResponseID string
	Sequence   int64
	Item       *ResponseOutputItem
	Delta      string
	Error      json.RawMessage
}
