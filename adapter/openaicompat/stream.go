package openaicompat

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"

	"github.com/daptin/llmgateway/contract"
)

type eventStream struct {
	body         io.ReadCloser
	reader       *bufio.Reader
	operation    contract.Operation
	maximum      int
	usage        contract.Usage
	pending      []contract.StreamEvent
	lastSequence int64
	hasSequence  bool
	done         bool
	closeOnce    sync.Once
}

func newEventStream(body io.ReadCloser, operation contract.Operation, maximum int) *eventStream {
	return &eventStream{body: body, reader: bufio.NewReaderSize(body, min(maximum, 64<<10)), operation: operation, maximum: maximum}
}

func (s *eventStream) Next(ctx context.Context) (contract.StreamEvent, error) {
	if s.done {
		return contract.StreamEvent{}, io.EOF
	}
	if err := ctx.Err(); err != nil {
		return contract.StreamEvent{}, err
	}
	if len(s.pending) > 0 {
		event := s.pending[0]
		s.pending = s.pending[1:]
		return s.deliver(event), nil
	}
	name, data, err := s.nextFrame()
	if err != nil {
		if errors.Is(err, io.EOF) {
			return contract.StreamEvent{}, providerFailure("upstream stream ended before [DONE]", io.ErrUnexpectedEOF)
		}
		return contract.StreamEvent{}, err
	}
	if bytes.Equal(bytes.TrimSpace(data), []byte("[DONE]")) {
		s.done = true
		usage := s.usage
		return contract.StreamEvent{Type: "done", Usage: &usage, Terminal: true}, nil
	}
	var event contract.StreamEvent
	switch s.operation {
	case contract.OperationChat:
		var events []contract.StreamEvent
		events, err = decodeChatEvents(data)
		if err == nil {
			event = events[0]
			s.pending = append(s.pending, events[1:]...)
		}
	case contract.OperationTextCompletion:
		var events []contract.StreamEvent
		events, err = decodeTextCompletionEvents(data)
		if err == nil {
			event = events[0]
			s.pending = append(s.pending, events[1:]...)
		}
	case contract.OperationResponses:
		event, err = decodeResponsesEvent(name, data)
		if err == nil {
			if s.hasSequence && event.Response.Sequence <= s.lastSequence {
				err = errors.New("responses stream sequence numbers are not increasing")
			} else {
				s.lastSequence = event.Response.Sequence
				s.hasSequence = true
			}
		}
	default:
		err = errors.New("unsupported streaming operation")
	}
	if err != nil {
		return contract.StreamEvent{}, providerFailure("upstream returned an invalid stream event", err)
	}
	return s.deliver(event), nil
}

type textCompletionChunkWire struct {
	ID      string `json:"id"`
	Created int64  `json:"created"`
	Choices []struct {
		Text         string          `json:"text"`
		Index        int             `json:"index"`
		Logprobs     json.RawMessage `json:"logprobs"`
		FinishReason *string         `json:"finish_reason"`
	} `json:"choices"`
	Usage *usageWire `json:"usage"`
}

func decodeTextCompletionEvents(data []byte) ([]contract.StreamEvent, error) {
	var wire textCompletionChunkWire
	if err := json.Unmarshal(data, &wire); err != nil {
		return nil, err
	}
	var usage *contract.Usage
	if wire.Usage != nil {
		value := canonicalUsage(*wire.Usage)
		usage = &value
	}
	if len(wire.Choices) == 0 {
		if usage == nil {
			return nil, errors.New("text completion stream event has neither choices nor usage")
		}
		return []contract.StreamEvent{{Type: "usage", Usage: usage}}, nil
	}
	events := make([]contract.StreamEvent, 0, len(wire.Choices))
	indexes := make(map[int]bool, len(wire.Choices))
	for _, choice := range wire.Choices {
		if choice.Index < 0 || indexes[choice.Index] {
			return nil, errors.New("text completion stream event contains invalid choice indices")
		}
		indexes[choice.Index] = true
		delta := &contract.TextCompletionDelta{ID: wire.ID, Created: wire.Created, Text: choice.Text, Index: choice.Index,
			Logprobs: append([]byte(nil), choice.Logprobs...)}
		typeName := "content_delta"
		if choice.FinishReason != nil {
			delta.FinishReason = *choice.FinishReason
			typeName = "finish"
		}
		events = append(events, contract.StreamEvent{Type: typeName, TextCompletion: delta})
	}
	events[len(events)-1].Usage = usage
	return events, nil
}

func (s *eventStream) deliver(event contract.StreamEvent) contract.StreamEvent {
	if event.Usage != nil {
		s.usage = *event.Usage
	}
	if event.Terminal {
		s.done = true
	}
	return event
}

func (s *eventStream) Close() error {
	var err error
	s.closeOnce.Do(func() { err = s.body.Close() })
	s.done = true
	return err
}

func (s *eventStream) nextFrame() (string, []byte, error) {
	var name string
	var data []byte
	total := 0
	for {
		line, err := s.reader.ReadString('\n')
		total += len(line)
		if total > s.maximum {
			return "", nil, errors.New("upstream SSE event exceeded the configured bound")
		}
		line = strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\r")
		if line == "" {
			if len(data) > 0 {
				return name, bytes.TrimSuffix(data, []byte("\n")), nil
			}
		} else if strings.HasPrefix(line, "event:") {
			name = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		} else if strings.HasPrefix(line, "data:") {
			value := strings.TrimPrefix(line, "data:")
			value = strings.TrimPrefix(value, " ")
			data = append(data, value...)
			data = append(data, '\n')
		}
		if err != nil {
			if errors.Is(err, io.EOF) && len(data) > 0 {
				return name, bytes.TrimSuffix(data, []byte("\n")), nil
			}
			return "", nil, err
		}
	}
}

type chatChunkWire struct {
	ID      string `json:"id"`
	Created int64  `json:"created"`
	Choices []struct {
		Index int `json:"index"`
		Delta struct {
			Role      string `json:"role"`
			Content   string `json:"content"`
			ToolCalls []struct {
				Index    int              `json:"index"`
				ID       string           `json:"id"`
				Type     string           `json:"type"`
				Function functionCallWire `json:"function"`
			} `json:"tool_calls"`
		} `json:"delta"`
		FinishReason *string         `json:"finish_reason"`
		Logprobs     json.RawMessage `json:"logprobs"`
	} `json:"choices"`
	Usage *usageWire `json:"usage"`
}

func decodeChatEvents(data []byte) ([]contract.StreamEvent, error) {
	var wire chatChunkWire
	if err := json.Unmarshal(data, &wire); err != nil {
		return nil, err
	}
	var usage *contract.Usage
	if wire.Usage != nil {
		value := canonicalUsage(*wire.Usage)
		usage = &value
	}
	if len(wire.Choices) == 0 {
		if usage == nil {
			return nil, errors.New("chat stream event has neither choices nor usage")
		}
		return []contract.StreamEvent{{Type: "usage", Usage: usage}}, nil
	}
	events := make([]contract.StreamEvent, 0, len(wire.Choices))
	indexes := make(map[int]bool, len(wire.Choices))
	for _, choice := range wire.Choices {
		if choice.Index < 0 || indexes[choice.Index] {
			return nil, errors.New("chat stream event contains invalid choice indices")
		}
		indexes[choice.Index] = true
		event := contract.StreamEvent{Type: "content_delta"}
		delta := &contract.ChatDelta{ID: wire.ID, Created: wire.Created, Index: choice.Index, Role: choice.Delta.Role, Content: choice.Delta.Content, Logprobs: append([]byte(nil), choice.Logprobs...)}
		for _, call := range choice.Delta.ToolCalls {
			delta.ToolCalls = append(delta.ToolCalls, contract.ToolCallDelta{Index: call.Index, ID: call.ID, Type: call.Type, Function: contract.FunctionCall{Name: call.Function.Name, Arguments: call.Function.Arguments}})
			event.Type = "tool_call_delta"
		}
		if choice.FinishReason != nil {
			delta.FinishReason = *choice.FinishReason
			event.Type = "finish"
		}
		event.Chat = delta
		events = append(events, event)
	}
	events[len(events)-1].Usage = usage
	return events, nil
}

type responsesEventWire struct {
	Type         string          `json:"type"`
	Sequence     *int64          `json:"sequence_number"`
	ResponseID   string          `json:"response_id"`
	ItemID       string          `json:"item_id"`
	OutputIndex  *int64          `json:"output_index"`
	ContentIndex *int64          `json:"content_index"`
	SummaryIndex *int64          `json:"summary_index"`
	Delta        string          `json:"delta"`
	Text         string          `json:"text"`
	Refusal      string          `json:"refusal"`
	Arguments    string          `json:"arguments"`
	Name         string          `json:"name"`
	Status       string          `json:"status"`
	Logprobs     json.RawMessage `json:"logprobs"`
	Item         json.RawMessage `json:"item"`
	Part         json.RawMessage `json:"part"`
	Response     json.RawMessage `json:"response"`
}

func decodeResponsesEvent(name string, data []byte) (contract.StreamEvent, error) {
	var wire responsesEventWire
	if err := json.Unmarshal(data, &wire); err != nil {
		return contract.StreamEvent{}, err
	}
	if name != "" && wire.Type != "" && name != wire.Type {
		return contract.StreamEvent{}, errors.New("responses stream event type conflicts with SSE event name")
	}
	if wire.Type != "" {
		name = wire.Type
	}
	if name == "" || wire.Sequence == nil || *wire.Sequence < 0 {
		return contract.StreamEvent{}, errors.New("responses stream event has no type or sequence number")
	}
	if name == "error" {
		return contract.StreamEvent{}, &contract.Error{Code: contract.ErrorProvider, Message: "upstream response stream failed", HTTPStatus: 502}
	}
	if len(wire.Logprobs) != 0 {
		var values []json.RawMessage
		if err := json.Unmarshal(wire.Logprobs, &values); err != nil {
			return contract.StreamEvent{}, errors.New("responses stream event logprobs must be an array")
		}
	}
	delta := &contract.ResponseDelta{ResponseID: wire.ResponseID, Sequence: *wire.Sequence, ItemID: wire.ItemID,
		Delta: wire.Delta, Text: wire.Text, Refusal: wire.Refusal, Arguments: wire.Arguments, Name: wire.Name, Status: wire.Status,
		Logprobs: append([]byte(nil), wire.Logprobs...)}
	if wire.OutputIndex != nil {
		delta.OutputIndex = *wire.OutputIndex
	}
	if wire.ContentIndex != nil {
		delta.ContentIndex = *wire.ContentIndex
	}
	if wire.SummaryIndex != nil {
		delta.SummaryIndex = *wire.SummaryIndex
	}
	event := contract.StreamEvent{Type: name, Response: delta}
	switch name {
	case "response.created", "response.queued", "response.in_progress", "response.completed", "response.incomplete":
		if len(wire.Response) == 0 {
			return contract.StreamEvent{}, errors.New("responses lifecycle event has no response")
		}
		snapshot, err := decodeResponse(contract.OperationResponses, wire.Response)
		if err != nil {
			return contract.StreamEvent{}, err
		}
		delta.Snapshot = &snapshot
		delta.ResponseID = snapshot.Responses.ID
		if name == "response.completed" || name == "response.incomplete" {
			usage := snapshot.Usage
			event.Usage = &usage
			event.Terminal = true
		}
	case "response.failed":
		return contract.StreamEvent{}, &contract.Error{Code: contract.ErrorProvider, Message: "upstream response stream failed", HTTPStatus: 502}
	case "response.output_item.added", "response.output_item.done":
		if wire.OutputIndex == nil || *wire.OutputIndex < 0 || len(wire.Item) == 0 {
			return contract.StreamEvent{}, errors.New("responses output item event is incomplete")
		}
		item, err := decodeResponseOutputItem(wire.Item, false)
		if err != nil {
			return contract.StreamEvent{}, err
		}
		delta.Item = &item
	case "response.content_part.added", "response.content_part.done", "response.reasoning_part.added", "response.reasoning_part.done":
		if !validResponseContentCoordinates(wire) || len(wire.Part) == 0 {
			return contract.StreamEvent{}, errors.New("responses content part event is incomplete")
		}
		part, err := decodeResponseContentPart(wire.Part)
		if err != nil {
			return contract.StreamEvent{}, err
		}
		if strings.HasPrefix(name, "response.reasoning_part.") && part.Type != "reasoning_text" {
			return contract.StreamEvent{}, errors.New("responses reasoning part is invalid")
		}
		delta.Part = &part
	case "response.output_text.delta", "response.reasoning_text.delta", "response.refusal.delta":
		if !validResponseContentCoordinates(wire) {
			return contract.StreamEvent{}, errors.New("responses text delta event is incomplete")
		}
	case "response.output_text.done", "response.reasoning_text.done", "response.refusal.done":
		if !validResponseContentCoordinates(wire) {
			return contract.StreamEvent{}, errors.New("responses text done event is incomplete")
		}
	case "response.function_call_arguments.delta":
		if !validResponseOutputCoordinates(wire) {
			return contract.StreamEvent{}, errors.New("responses function arguments delta event is incomplete")
		}
	case "response.function_call_arguments.done":
		if !validResponseOutputCoordinates(wire) || wire.Name == "" {
			return contract.StreamEvent{}, errors.New("responses function arguments done event is incomplete")
		}
	case "response.reasoning_summary_part.added", "response.reasoning_summary_part.done":
		if !validResponseSummaryCoordinates(wire) || len(wire.Part) == 0 {
			return contract.StreamEvent{}, errors.New("responses reasoning summary part event is incomplete")
		}
		if name == "response.reasoning_summary_part.done" && wire.Status != "" && wire.Status != "incomplete" {
			return contract.StreamEvent{}, errors.New("responses reasoning summary part status is invalid")
		}
		part, err := decodeResponseContentPart(wire.Part)
		if err != nil || part.Type != "summary_text" {
			return contract.StreamEvent{}, errors.New("responses reasoning summary part is invalid")
		}
		delta.Part = &part
	case "response.reasoning_summary_text.delta", "response.reasoning_summary_text.done":
		if !validResponseSummaryCoordinates(wire) {
			return contract.StreamEvent{}, errors.New("responses reasoning summary text event is incomplete")
		}
	default:
		return contract.StreamEvent{}, fmt.Errorf("unsupported responses stream event %q", name)
	}
	return event, nil
}

func validResponseOutputCoordinates(wire responsesEventWire) bool {
	return wire.ItemID != "" && wire.OutputIndex != nil && *wire.OutputIndex >= 0
}

func validResponseContentCoordinates(wire responsesEventWire) bool {
	return validResponseOutputCoordinates(wire) && wire.ContentIndex != nil && *wire.ContentIndex >= 0
}

func validResponseSummaryCoordinates(wire responsesEventWire) bool {
	return validResponseOutputCoordinates(wire) && wire.SummaryIndex != nil && *wire.SummaryIndex >= 0
}

func decodeResponseContentPart(raw json.RawMessage) (contract.ContentPart, error) {
	var wire struct {
		Type        string          `json:"type"`
		Text        string          `json:"text"`
		Refusal     string          `json:"refusal"`
		ImageURL    string          `json:"image_url"`
		Detail      string          `json:"detail"`
		FileID      string          `json:"file_id"`
		FileData    string          `json:"file_data"`
		Filename    string          `json:"filename"`
		Annotations json.RawMessage `json:"annotations"`
		Logprobs    json.RawMessage `json:"logprobs"`
	}
	if err := json.Unmarshal(raw, &wire); err != nil {
		return contract.ContentPart{}, err
	}
	switch wire.Type {
	case "input_text", "output_text", "reasoning_text", "summary_text":
	case "input_image":
		if wire.ImageURL == "" {
			return contract.ContentPart{}, errors.New("response input image has no URL")
		}
	case "input_file":
		if wire.FileID != "" || wire.FileData == "" {
			return contract.ContentPart{}, errors.New("response input file is not inline")
		}
	case "refusal":
	default:
		return contract.ContentPart{}, fmt.Errorf("unsupported response content part %q", wire.Type)
	}
	if len(wire.Annotations) != 0 {
		var values []json.RawMessage
		if err := json.Unmarshal(wire.Annotations, &values); err != nil {
			return contract.ContentPart{}, errors.New("response content part annotations must be an array")
		}
	}
	if len(wire.Logprobs) != 0 {
		var values []json.RawMessage
		if err := json.Unmarshal(wire.Logprobs, &values); err != nil {
			return contract.ContentPart{}, errors.New("response content part logprobs must be an array")
		}
	}
	part := contract.ContentPart{Type: wire.Type, Text: wire.Text, Refusal: wire.Refusal,
		Annotations: append([]byte(nil), wire.Annotations...), Logprobs: append([]byte(nil), wire.Logprobs...)}
	if wire.Type == "input_image" {
		part.ImageURL = &contract.ImageURL{URL: wire.ImageURL, Detail: wire.Detail}
	}
	if wire.Type == "input_file" {
		part.File = &contract.InputFile{Data: wire.FileData, Filename: wire.Filename}
	}
	return part, nil
}

func decodeResponseOutputItem(raw json.RawMessage, compact bool) (contract.ResponseOutputItem, error) {
	var wire struct {
		Type             string            `json:"type"`
		ID               string            `json:"id"`
		Role             string            `json:"role"`
		Status           string            `json:"status"`
		CallID           string            `json:"call_id"`
		Name             string            `json:"name"`
		Arguments        string            `json:"arguments"`
		EncryptedContent string            `json:"encrypted_content"`
		CreatedBy        string            `json:"created_by"`
		Content          []json.RawMessage `json:"content"`
		Summary          []json.RawMessage `json:"summary"`
	}
	if err := json.Unmarshal(raw, &wire); err != nil {
		return contract.ResponseOutputItem{}, err
	}
	if wire.ID == "" {
		return contract.ResponseOutputItem{}, errors.New("response output item has no id")
	}
	switch wire.Type {
	case "message":
		inputRole := wire.Role == "user" || wire.Role == "system" || wire.Role == "developer"
		validRole := wire.Role == "assistant" || (compact && inputRole)
		if !validRole || wire.Content == nil || (wire.Role == "assistant" && wire.Status == "") {
			return contract.ResponseOutputItem{}, errors.New("response message output is incomplete")
		}
	case "function_call":
		if wire.Status == "" || wire.CallID == "" || wire.Name == "" {
			return contract.ResponseOutputItem{}, errors.New("response function call output is incomplete")
		}
	case "reasoning":
		if wire.Summary == nil {
			return contract.ResponseOutputItem{}, errors.New("response reasoning output is incomplete")
		}
	case "compaction":
		if wire.EncryptedContent == "" {
			return contract.ResponseOutputItem{}, errors.New("response compaction output is incomplete")
		}
	default:
		return contract.ResponseOutputItem{}, fmt.Errorf("unsupported response output item %q", wire.Type)
	}
	item := contract.ResponseOutputItem{Type: wire.Type, ID: wire.ID, Role: wire.Role, Status: wire.Status, CallID: wire.CallID, Name: wire.Name, Arguments: wire.Arguments,
		EncryptedContent: wire.EncryptedContent, CreatedBy: wire.CreatedBy}
	for _, rawPart := range wire.Content {
		part, err := decodeResponseContentPart(rawPart)
		inputPart := part.Type == "input_text" || part.Type == "input_image" || part.Type == "input_file"
		validPart := wire.Type == "reasoning" && part.Type == "reasoning_text" || wire.Type == "message" &&
			(part.Type == "output_text" || part.Type == "refusal" || compact && wire.Role != "assistant" && inputPart)
		if err != nil || !validPart {
			return contract.ResponseOutputItem{}, errors.New("response message contains invalid content")
		}
		item.Content = append(item.Content, part)
	}
	for _, rawPart := range wire.Summary {
		part, err := decodeResponseContentPart(rawPart)
		if err != nil || part.Type != "summary_text" {
			return contract.ResponseOutputItem{}, errors.New("response reasoning contains invalid summary")
		}
		item.Summary = append(item.Summary, part)
	}
	return item, nil
}
