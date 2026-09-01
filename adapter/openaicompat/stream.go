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
	body      io.ReadCloser
	reader    *bufio.Reader
	operation contract.Operation
	maximum   int
	usage     contract.Usage
	pending   []contract.StreamEvent
	done      bool
	closeOnce sync.Once
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
	case contract.OperationResponses:
		event, err = decodeResponsesEvent(name, data)
	default:
		err = errors.New("unsupported streaming operation")
	}
	if err != nil {
		return contract.StreamEvent{}, providerFailure("upstream returned an invalid stream event", err)
	}
	return s.deliver(event), nil
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
	Type       string          `json:"type"`
	Sequence   int64           `json:"sequence_number"`
	ResponseID string          `json:"response_id"`
	Delta      string          `json:"delta"`
	Item       json.RawMessage `json:"item"`
	Response   json.RawMessage `json:"response"`
}

func decodeResponsesEvent(name string, data []byte) (contract.StreamEvent, error) {
	var wire responsesEventWire
	if err := json.Unmarshal(data, &wire); err != nil {
		return contract.StreamEvent{}, err
	}
	if wire.Type != "" {
		name = wire.Type
	}
	if name == "" {
		return contract.StreamEvent{}, errors.New("responses stream event has no type")
	}
	if name == "error" {
		return contract.StreamEvent{}, &contract.Error{Code: contract.ErrorProvider, Message: "upstream response stream failed", HTTPStatus: 502}
	}
	delta := &contract.ResponseDelta{ResponseID: wire.ResponseID, Sequence: wire.Sequence, Delta: wire.Delta}
	if len(wire.Item) > 0 {
		item, err := decodeResponseOutputItem(wire.Item)
		if err != nil {
			return contract.StreamEvent{}, err
		}
		delta.Item = &item
	}
	event := contract.StreamEvent{Type: name, Response: delta}
	if name == "response.completed" {
		var completed struct {
			ID    string    `json:"id"`
			Usage usageWire `json:"usage"`
		}
		if err := json.Unmarshal(wire.Response, &completed); err != nil {
			return contract.StreamEvent{}, err
		}
		if delta.ResponseID == "" {
			delta.ResponseID = completed.ID
		}
		usage := canonicalUsage(completed.Usage)
		event.Usage = &usage
		event.Terminal = true
	}
	return event, nil
}

func decodeResponseOutputItem(raw json.RawMessage) (contract.ResponseOutputItem, error) {
	var wire struct {
		Type      string `json:"type"`
		ID        string `json:"id"`
		Role      string `json:"role"`
		Status    string `json:"status"`
		CallID    string `json:"call_id"`
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
		Content   []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		Summary []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"summary"`
	}
	if err := json.Unmarshal(raw, &wire); err != nil {
		return contract.ResponseOutputItem{}, err
	}
	if wire.Type == "" {
		return contract.ResponseOutputItem{}, fmt.Errorf("response output item has no type")
	}
	item := contract.ResponseOutputItem{Type: wire.Type, ID: wire.ID, Role: wire.Role, Status: wire.Status, CallID: wire.CallID, Name: wire.Name, Arguments: wire.Arguments}
	for _, content := range wire.Content {
		item.Content = append(item.Content, contract.ContentPart{Type: content.Type, Text: content.Text})
	}
	for _, summary := range wire.Summary {
		item.Summary = append(item.Summary, contract.ContentPart{Type: summary.Type, Text: summary.Text})
	}
	return item, nil
}
