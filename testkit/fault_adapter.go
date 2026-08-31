package testkit

import (
	"context"
	"errors"
	"io"
	"sync"

	"github.com/daptin/llmgateway/adapter"
	"github.com/daptin/llmgateway/catalog"
	"github.com/daptin/llmgateway/contract"
)

type AdapterStep struct {
	Response      contract.Response
	Events        []contract.StreamEvent
	BuildError    error
	InvokeError   error
	StreamError   error
	TerminalError error
}

type FaultAdapter struct {
	mu           sync.Mutex
	capabilities adapter.Capabilities
	steps        []AdapterStep
	next         int
	attempts     []contract.ID
	requests     []contract.Request
}

func NewFaultAdapter(capabilities adapter.Capabilities, steps ...AdapterStep) *FaultAdapter {
	return &FaultAdapter{capabilities: capabilities, steps: append([]AdapterStep(nil), steps...)}
}

func (a *FaultAdapter) Capabilities() adapter.Capabilities { return a.capabilities }

func (a *FaultAdapter) Invoke(ctx context.Context, deployment catalog.Deployment, request contract.Request) (contract.Response, error) {
	if err := ctx.Err(); err != nil {
		return contract.Response{}, err
	}
	step, err := a.take(deployment.ID, request)
	if err != nil {
		return contract.Response{}, err
	}
	if step.InvokeError != nil {
		return contract.Response{}, step.InvokeError
	}
	return step.Response, nil
}

func (a *FaultAdapter) Stream(ctx context.Context, deployment catalog.Deployment, request contract.Request) (adapter.Stream, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	step, err := a.take(deployment.ID, request)
	if err != nil {
		return nil, err
	}
	if step.StreamError != nil {
		return nil, step.StreamError
	}
	return &faultStream{events: append([]contract.StreamEvent(nil), step.Events...), terminalError: step.TerminalError}, nil
}

func (a *FaultAdapter) Attempts() []contract.ID {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]contract.ID(nil), a.attempts...)
}

func (a *FaultAdapter) Requests() []contract.Request {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]contract.Request(nil), a.requests...)
}

func (a *FaultAdapter) take(deploymentID contract.ID, request contract.Request) (AdapterStep, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.attempts = append(a.attempts, deploymentID)
	a.requests = append(a.requests, request)
	if a.next >= len(a.steps) {
		return AdapterStep{}, errors.New("fault adapter script exhausted")
	}
	step := a.steps[a.next]
	a.next++
	return step, nil
}

type faultStream struct {
	mu            sync.Mutex
	events        []contract.StreamEvent
	next          int
	terminalError error
	closed        bool
}

func (s *faultStream) Next(ctx context.Context) (contract.StreamEvent, error) {
	if err := ctx.Err(); err != nil {
		return contract.StreamEvent{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return contract.StreamEvent{}, io.EOF
	}
	if s.next < len(s.events) {
		event := s.events[s.next]
		s.next++
		return event, nil
	}
	s.closed = true
	if s.terminalError != nil {
		return contract.StreamEvent{}, s.terminalError
	}
	return contract.StreamEvent{}, io.EOF
}

func (s *faultStream) Close() error {
	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()
	return nil
}
