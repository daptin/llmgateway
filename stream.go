package llmgateway

import (
	"context"
	"errors"
	"io"
	"sync"
	"time"

	"github.com/daptin/llmgateway/accounting"
	"github.com/daptin/llmgateway/adapter"
	"github.com/daptin/llmgateway/contract"
	"github.com/daptin/llmgateway/internal/routing"
)

type GatewayStream struct {
	mu           sync.Mutex
	engine       *Engine
	prepared     preparedRequest
	route        routing.Attempt
	upstream     adapter.Stream
	first        *contract.StreamEvent
	attempts     []contract.Attempt
	attemptIndex int
	attemptStart time.Time
	firstByteAt  time.Time
	usage        contract.Usage
	lease        attemptLease
	idleTimeout  time.Duration
	committed    bool
	terminal     bool
	done         func()
	cancel       context.CancelFunc
	doneOnce     sync.Once
}

type EventStream = contract.EventStream

func (e *Engine) Stream(ctx context.Context, principal contract.Principal, request contract.Request) (EventStream, error) {
	ctx, cancelRequest := context.WithTimeout(ctx, e.requestTimeout)
	if !e.beginRequest() {
		cancelRequest()
		return nil, ErrDraining
	}
	released := false
	defer func() {
		if !released {
			cancelRequest()
			e.endRequest()
		}
	}()
	if !request.Stream {
		return nil, publicError(contract.ErrorInvalidRequest, "invalid streaming request", 400, false, nil)
	}
	prepared, err := e.prepare(ctx, principal, request)
	if err != nil {
		return nil, err
	}
	transferred := false
	defer func() {
		if !transferred {
			_ = e.cancelPrepared(ctx, prepared, contract.Cancellation{Token: prepared.token, Reason: "stream_setup_abandoned", EndedAt: e.clock.Now()})
		}
	}()
	finish := func(completion contract.Completion) error {
		if finishErr := e.finalizePrepared(ctx, prepared, completion); finishErr != nil {
			return finishErr
		}
		transferred = true
		return nil
	}
	attempts := make([]contract.Attempt, 0, len(prepared.plan.Attempts))
	for index, routeAttempt := range prepared.plan.Attempts {
		lease, gateErr := e.beforeAttempt(ctx, routeAttempt.Deployment, prepared.request)
		if gateErr != nil {
			normalized := normalizeError(gateErr, contract.ErrorUnavailable, 503, true)
			if !normalized.Retryable || index == len(prepared.plan.Attempts)-1 {
				if finishErr := finish(contract.Completion{Token: prepared.token, Status: "failed", HTTPStatus: normalized.HTTPStatus, ErrorCode: normalized.Code, Attempts: attempts, EndedAt: e.clock.Now()}); finishErr != nil {
					return nil, finishErr
				}
				return nil, normalized
			}
			if waitErr := e.waitForRetry(ctx, index, normalized); waitErr != nil {
				return nil, waitErr
			}
			continue
		}
		started := e.clock.Now()
		upstream, normalized := e.openProviderStream(ctx, prepared.runtime.adapters[routeAttempt.Provider.ID], routeAttempt.Deployment, prepared.request, lease)
		if normalized != nil {
			attempts = append(attempts, failedStreamAttempt(index+1, routeAttempt, started, e.clock.Now(), normalized, false))
			if !normalized.Retryable || index == len(prepared.plan.Attempts)-1 {
				if finishErr := finish(contract.Completion{Token: prepared.token, Status: "failed", HTTPStatus: normalized.HTTPStatus, ErrorCode: normalized.Code, Attempts: attempts, EndedAt: e.clock.Now()}); finishErr != nil {
					return nil, finishErr
				}
				return nil, normalized
			}
			if waitErr := e.waitForRetry(ctx, index, normalized); waitErr != nil {
				return nil, waitErr
			}
			continue
		}
		first, firstErr := nextProviderEventWithin(ctx, upstream, e.firstEventTimeout, "upstream first event timed out")
		if firstErr != nil {
			_ = closeProviderStream(upstream)
			normalized := normalizeError(firstErr, contract.ErrorProvider, 502, false)
			e.afterAttempt(ctx, routeAttempt.Deployment, lease, normalized)
			attempts = append(attempts, failedStreamAttempt(index+1, routeAttempt, started, e.clock.Now(), normalized, false))
			if !normalized.Retryable || index == len(prepared.plan.Attempts)-1 {
				if finishErr := finish(contract.Completion{Token: prepared.token, Status: "failed", HTTPStatus: normalized.HTTPStatus, ErrorCode: normalized.Code, Attempts: attempts, EndedAt: e.clock.Now()}); finishErr != nil {
					return nil, finishErr
				}
				return nil, normalized
			}
			if waitErr := e.waitForRetry(ctx, index, normalized); waitErr != nil {
				return nil, waitErr
			}
			continue
		}
		if first.Chat == nil && first.Response == nil && first.Usage == nil && !first.Terminal {
			_ = closeProviderStream(upstream)
			normalized := publicError(contract.ErrorProvider, "provider returned an empty first stream event", 502, false, nil)
			e.afterAttempt(ctx, routeAttempt.Deployment, lease, normalized)
			attempts = append(attempts, failedStreamAttempt(index+1, routeAttempt, started, e.clock.Now(), normalized, false))
			if finishErr := finish(contract.Completion{Token: prepared.token, Status: "failed", HTTPStatus: 502, ErrorCode: normalized.Code, Attempts: attempts, EndedAt: e.clock.Now()}); finishErr != nil {
				return nil, finishErr
			}
			return nil, normalized
		}
		firstAt := e.clock.Now()
		stream := &GatewayStream{
			engine: e, prepared: prepared, route: routeAttempt, upstream: upstream, first: &first,
			attempts: attempts, attemptIndex: index + 1, attemptStart: started, firstByteAt: firstAt,
			lease: lease, done: e.endRequest, cancel: cancelRequest,
			idleTimeout: e.streamIdleTimeout,
		}
		if first.Usage != nil {
			stream.usage = *first.Usage
		}
		transferred = true
		released = true
		return stream, nil
	}
	return nil, publicError(contract.ErrorUnavailable, "no healthy deployment", 503, true, nil)
}

// Next hands the first validated event to the caller and irrevocably commits
// this stream to its selected deployment. Provider switching is impossible
// after this method returns an event.
func (s *GatewayStream) Next(ctx context.Context) (contract.StreamEvent, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.terminal {
		return contract.StreamEvent{}, io.EOF
	}
	if s.first != nil {
		event := *s.first
		s.first = nil
		if guardrailErr := s.engine.checkStream(ctx, s.prepared, event); guardrailErr != nil {
			normalized := normalizeError(guardrailErr, contract.ErrorPermission, 400, false)
			if finishErr := s.finishLocked(ctx, "rejected", normalized.HTTPStatus, normalized.Code, false); finishErr != nil {
				return contract.StreamEvent{}, finishErr
			}
			return contract.StreamEvent{}, normalized
		}
		s.committed = true
		event.OutputCommitted = true
		if event.Terminal {
			if err := s.finishLocked(ctx, "succeeded", 200, "", false); err != nil {
				return contract.StreamEvent{}, err
			}
		}
		return event, nil
	}
	event, err := nextProviderEventWithin(ctx, s.upstream, s.idleTimeout, "upstream stream became idle")
	if event.Usage != nil {
		s.usage = *event.Usage
	}
	if err != nil {
		if errors.Is(err, io.EOF) {
			if finishErr := s.finishLocked(ctx, "succeeded", 200, "", false); finishErr != nil {
				return contract.StreamEvent{}, finishErr
			}
			return contract.StreamEvent{}, io.EOF
		}
		normalized := normalizeError(err, contract.ErrorProvider, 502, false)
		if finishErr := s.finishLocked(ctx, "failed", normalized.HTTPStatus, normalized.Code, normalized.Retryable); finishErr != nil {
			return contract.StreamEvent{}, finishErr
		}
		return contract.StreamEvent{}, normalized
	}
	if guardrailErr := s.engine.checkStream(ctx, s.prepared, event); guardrailErr != nil {
		normalized := normalizeError(guardrailErr, contract.ErrorPermission, 400, false)
		if finishErr := s.finishLocked(ctx, "rejected", normalized.HTTPStatus, normalized.Code, false); finishErr != nil {
			return contract.StreamEvent{}, finishErr
		}
		return contract.StreamEvent{}, normalized
	}
	event.OutputCommitted = s.committed
	if event.Terminal {
		if finishErr := s.finishLocked(ctx, "succeeded", 200, "", false); finishErr != nil {
			return contract.StreamEvent{}, finishErr
		}
	}
	return event, nil
}

func (s *GatewayStream) Close(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.terminal {
		s.releaseRequest()
		return nil
	}
	defer s.releaseRequest()
	closeErr := closeProviderStream(s.upstream)
	s.engine.releaseAttemptLease(ctx, s.lease)
	ended := s.engine.clock.Now()
	attempt := contract.Attempt{
		Number: s.attemptIndex, ProviderID: s.route.Provider.ID, DeploymentID: s.route.Deployment.ID,
		StartedAt: s.attemptStart, FirstByteAt: s.firstByteAt, EndedAt: ended, Outcome: "cancelled",
		OutputCommitted: s.committed, Usage: s.usage,
	}
	s.attempts = append(s.attempts, attempt)
	err := s.engine.cancelPrepared(ctx, s.prepared, contract.Cancellation{Token: s.prepared.token, Reason: "stream_closed", Usage: s.usage, Attempts: s.attempts, EndedAt: ended})
	s.terminal = true
	if err != nil {
		return err
	}
	return closeErr
}

func (s *GatewayStream) finishLocked(ctx context.Context, status string, httpStatus int, code contract.ErrorCode, retryable bool) error {
	defer s.releaseRequest()
	ended := s.engine.clock.Now()
	usage := s.usage
	if usage.TotalTokens == 0 {
		usage.TotalTokens = usage.InputTokens + usage.OutputTokens
	}
	cost, err := accounting.CostMicros(usage, s.route.Deployment.Pricing)
	if err != nil || !usage.Valid() {
		status = "failed"
		httpStatus = 502
		code = contract.ErrorProvider
		usage = contract.Usage{}
	}
	usage.CostMicros = cost
	closeErr := closeProviderStream(s.upstream)
	if closeErr != nil && status == "succeeded" {
		status = "failed"
		httpStatus = 502
		code = contract.ErrorProvider
		retryable = false
	}
	var circuitResult *contract.Error
	if status == "failed" {
		circuitResult = publicError(code, safeMessage(code), httpStatus, retryable, closeErr)
	}
	s.engine.afterAttempt(ctx, s.route.Deployment, s.lease, circuitResult)
	attempt := contract.Attempt{
		Number: s.attemptIndex, ProviderID: s.route.Provider.ID, DeploymentID: s.route.Deployment.ID,
		StartedAt: s.attemptStart, FirstByteAt: s.firstByteAt, EndedAt: ended, Outcome: status,
		ErrorCode: code, HTTPStatus: httpStatus, Retryable: retryable, OutputCommitted: s.committed, Usage: usage,
	}
	s.attempts = append(s.attempts, attempt)
	if err := s.engine.finalizePrepared(ctx, s.prepared, contract.Completion{
		Token: s.prepared.token, Status: status, HTTPStatus: httpStatus, ErrorCode: code,
		Usage: usage, Attempts: s.attempts, FirstByteAt: s.firstByteAt, EndedAt: ended,
	}); err != nil {
		return err
	}
	s.terminal = true
	if closeErr != nil {
		return normalizeError(closeErr, contract.ErrorProvider, 502, false)
	}
	return nil
}

func (s *GatewayStream) releaseRequest() {
	s.doneOnce.Do(func() {
		if s.cancel != nil {
			s.cancel()
		}
		if s.done != nil {
			s.done()
		}
	})
}

func failedStreamAttempt(number int, route routing.Attempt, started, ended time.Time, err *contract.Error, committed bool) contract.Attempt {
	return contract.Attempt{
		Number: number, ProviderID: route.Provider.ID, DeploymentID: route.Deployment.ID,
		StartedAt: started, EndedAt: ended, Outcome: "failed", ErrorCode: err.Code,
		HTTPStatus: err.HTTPStatus, Retryable: err.Retryable, OutputCommitted: committed,
	}
}
