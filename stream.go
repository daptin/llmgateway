package llmgateway

import (
	"context"
	"errors"
	"io"
	"sync"
	"time"

	"github.com/daptin/llmgateway/adapter"
	"github.com/daptin/llmgateway/contract"
	"github.com/daptin/llmgateway/internal/routing"
)

type GatewayStream struct {
	mu            sync.Mutex
	engine        *Engine
	prepared      preparedRequest
	route         routing.Attempt
	upstream      adapter.Stream
	first         *contract.StreamEvent
	attempts      []contract.Attempt
	attemptIndex  int
	attemptStart  time.Time
	firstByteAt   time.Time
	usage         contract.Usage
	reservedUsage contract.Usage
	lease         attemptLease
	idleTimeout   time.Duration
	committed     bool
	terminal      bool
	done          func()
	cancel        context.CancelFunc
	doneOnce      sync.Once
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
	terminalizationAttempted := false
	transferred := false
	defer func() {
		if !transferred && !terminalizationAttempted {
			_ = e.cancelPrepared(ctx, prepared, contract.Cancellation{Token: prepared.token, Reason: "stream_setup_abandoned", EndedAt: e.clock.Now()})
		}
	}()
	finish := func(completion contract.Completion) error {
		terminalizationAttempted = true
		if finishErr := e.finalizePrepared(ctx, prepared, completion); finishErr != nil {
			return finishErr
		}
		transferred = true
		return nil
	}
	cancelAdmission := func(cancellation contract.Cancellation) error {
		terminalizationAttempted = true
		if cancelErr := e.cancelPrepared(ctx, prepared, cancellation); cancelErr != nil {
			return cancelErr
		}
		transferred = true
		return nil
	}
	attempts := make([]contract.Attempt, 0, len(prepared.plan.Attempts))
	attemptTotal := func() contract.Usage {
		usage, aggregateErr := aggregateAttemptUsage(attempts)
		if aggregateErr != nil {
			return prepared.reserved
		}
		return usage
	}
	settleRetryInterruption := func(interruption error) error {
		normalized := normalizeError(interruption, contract.ErrorTimeout, 504, false)
		if normalized.HTTPStatus == 499 {
			return cancelAdmission(contract.Cancellation{Token: prepared.token, Reason: "request_cancelled", Usage: attemptTotal(), Attempts: attempts, EndedAt: e.clock.Now()})
		}
		return finish(contract.Completion{Token: prepared.token, Status: "failed", HTTPStatus: normalized.HTTPStatus, ErrorCode: normalized.Code, Usage: attemptTotal(), Attempts: attempts, EndedAt: e.clock.Now()})
	}
	failedUsage := func(index int, route routing.Attempt, reported *contract.Usage) contract.Usage {
		if reported == nil {
			return prepared.attemptExposure[index]
		}
		settled, settleErr := settledUsage(*reported, prepared.request.EstimatedUsage, route.Deployment.Pricing)
		if settleErr != nil || !settled.Valid() {
			return prepared.attemptExposure[index]
		}
		return settled
	}
	attemptNumber := 0
	for index, routeAttempt := range prepared.plan.Attempts {
		lease, gateErr := e.beforeAttempt(ctx, routeAttempt.Deployment, prepared.request)
		if gateErr != nil {
			normalized := normalizeError(gateErr, contract.ErrorUnavailable, 503, true)
			if normalized.HTTPStatus == 499 {
				if cancelErr := cancelAdmission(contract.Cancellation{Token: prepared.token, Reason: "request_cancelled", Usage: attemptTotal(), Attempts: attempts, EndedAt: e.clock.Now()}); cancelErr != nil {
					return nil, cancelErr
				}
				return nil, normalized
			}
			if !normalized.Retryable || index == len(prepared.plan.Attempts)-1 {
				if finishErr := finish(contract.Completion{Token: prepared.token, Status: "failed", HTTPStatus: normalized.HTTPStatus, ErrorCode: normalized.Code, Usage: attemptTotal(), Attempts: attempts, EndedAt: e.clock.Now()}); finishErr != nil {
					return nil, finishErr
				}
				return nil, normalized
			}
			if waitErr := e.waitForRetry(ctx, index, normalized); waitErr != nil {
				if settleErr := settleRetryInterruption(waitErr); settleErr != nil {
					return nil, settleErr
				}
				return nil, waitErr
			}
			continue
		}
		started := e.clock.Now()
		attemptNumber++
		upstream, normalized := e.openProviderStream(ctx, prepared.runtime.adapters[routeAttempt.Provider.ID], routeAttempt.Deployment, prepared.request, lease)
		if normalized != nil {
			outcome := "failed"
			if normalized.HTTPStatus == 499 {
				outcome = "cancelled"
			}
			attempts = append(attempts, terminalStreamAttempt(attemptNumber, routeAttempt, started, e.clock.Now(), normalized, outcome, false, failedUsage(index, routeAttempt, nil)))
			if normalized.HTTPStatus == 499 {
				if cancelErr := cancelAdmission(contract.Cancellation{Token: prepared.token, Reason: "request_cancelled", Usage: attemptTotal(), Attempts: attempts, EndedAt: e.clock.Now()}); cancelErr != nil {
					return nil, cancelErr
				}
				return nil, normalized
			}
			if !normalized.Retryable || index == len(prepared.plan.Attempts)-1 {
				if finishErr := finish(contract.Completion{Token: prepared.token, Status: "failed", HTTPStatus: normalized.HTTPStatus, ErrorCode: normalized.Code, Usage: attemptTotal(), Attempts: attempts, EndedAt: e.clock.Now()}); finishErr != nil {
					return nil, finishErr
				}
				return nil, normalized
			}
			if waitErr := e.waitForRetry(ctx, index, normalized); waitErr != nil {
				if settleErr := settleRetryInterruption(waitErr); settleErr != nil {
					return nil, settleErr
				}
				return nil, waitErr
			}
			continue
		}
		first, preambleUsage, firstErr := firstSemanticEventWithin(ctx, upstream, e.firstEventTimeout)
		if firstErr != nil {
			_ = closeProviderStream(upstream)
			normalized := normalizeError(firstErr, contract.ErrorProvider, 502, false)
			e.afterAttempt(ctx, routeAttempt.Deployment, lease, normalized)
			outcome := "failed"
			if normalized.HTTPStatus == 499 {
				outcome = "cancelled"
			}
			attempts = append(attempts, terminalStreamAttempt(attemptNumber, routeAttempt, started, e.clock.Now(), normalized, outcome, false, failedUsage(index, routeAttempt, preambleUsage)))
			if normalized.HTTPStatus == 499 {
				if cancelErr := cancelAdmission(contract.Cancellation{Token: prepared.token, Reason: "request_cancelled", Usage: attemptTotal(), Attempts: attempts, EndedAt: e.clock.Now()}); cancelErr != nil {
					return nil, cancelErr
				}
				return nil, normalized
			}
			if !normalized.Retryable || index == len(prepared.plan.Attempts)-1 {
				if finishErr := finish(contract.Completion{Token: prepared.token, Status: "failed", HTTPStatus: normalized.HTTPStatus, ErrorCode: normalized.Code, Usage: attemptTotal(), Attempts: attempts, EndedAt: e.clock.Now()}); finishErr != nil {
					return nil, finishErr
				}
				return nil, normalized
			}
			if waitErr := e.waitForRetry(ctx, index, normalized); waitErr != nil {
				if settleErr := settleRetryInterruption(waitErr); settleErr != nil {
					return nil, settleErr
				}
				return nil, waitErr
			}
			continue
		}
		firstAt := e.clock.Now()
		stream := &GatewayStream{
			engine: e, prepared: prepared, route: routeAttempt, upstream: upstream, first: &first,
			attempts: attempts, attemptIndex: attemptNumber, attemptStart: started, firstByteAt: firstAt,
			reservedUsage: prepared.attemptExposure[index],
			lease:         lease, done: e.endRequest, cancel: cancelRequest,
			idleTimeout: e.streamIdleTimeout,
		}
		stream.observeUsage(preambleUsage)
		stream.observeUsage(first.Usage)
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
	s.observeUsage(event.Usage)
	if err != nil {
		if errors.Is(err, io.EOF) {
			normalized := publicError(contract.ErrorProvider, "upstream stream ended unexpectedly", 502, false, io.ErrUnexpectedEOF)
			if finishErr := s.finishLocked(ctx, "failed", normalized.HTTPStatus, normalized.Code, normalized.Retryable); finishErr != nil {
				return contract.StreamEvent{}, finishErr
			}
			return contract.StreamEvent{}, normalized
		}
		normalized := normalizeError(err, contract.ErrorProvider, 502, false)
		if normalized.HTTPStatus == 499 {
			if cancelErr := s.cancelLocked(ctx, "request_cancelled"); cancelErr != nil {
				return contract.StreamEvent{}, cancelErr
			}
			return contract.StreamEvent{}, normalized
		}
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

func (s *GatewayStream) observeUsage(usage *contract.Usage) {
	if usage == nil || (tokenUsageEmpty(*usage) && !tokenUsageEmpty(s.usage)) {
		return
	}
	s.usage = *usage
}

func (s *GatewayStream) Close(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cancelLocked(ctx, "stream_closed")
}

func (s *GatewayStream) cancelLocked(ctx context.Context, reason string) error {
	if s.terminal {
		s.releaseRequest()
		return nil
	}
	defer s.releaseRequest()
	closeErr := closeProviderStream(s.upstream)
	s.engine.releaseAttemptLease(ctx, s.lease)
	ended := s.engine.clock.Now()
	usage, usageErr := settledUsage(s.usage, s.prepared.request.EstimatedUsage, s.route.Deployment.Pricing)
	if usageErr != nil || !usage.Valid() {
		usage = s.reservedUsage
	}
	attempt := contract.Attempt{
		Number: s.attemptIndex, ProviderID: s.route.Provider.ID, DeploymentID: s.route.Deployment.ID,
		StartedAt: s.attemptStart, FirstByteAt: s.firstByteAt, EndedAt: ended, Outcome: "cancelled",
		OutputCommitted: s.committed, Usage: usage,
	}
	s.attempts = append(s.attempts, attempt)
	total, aggregateErr := aggregateAttemptUsage(s.attempts)
	if aggregateErr != nil {
		total = s.prepared.reserved
	}
	err := s.engine.cancelPrepared(ctx, s.prepared, contract.Cancellation{Token: s.prepared.token, Reason: reason, Usage: total, Attempts: s.attempts, EndedAt: ended})
	s.terminal = true
	if err != nil {
		return err
	}
	return closeErr
}

func (s *GatewayStream) finishLocked(ctx context.Context, status string, httpStatus int, code contract.ErrorCode, retryable bool) error {
	defer s.releaseRequest()
	s.terminal = true
	ended := s.engine.clock.Now()
	usage, usageErr := settledUsage(s.usage, s.prepared.request.EstimatedUsage, s.route.Deployment.Pricing)
	if usageErr != nil || !usage.Valid() {
		status = "failed"
		httpStatus = 502
		code = contract.ErrorProvider
		usage = s.reservedUsage
	}
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
	total, aggregateErr := aggregateAttemptUsage(s.attempts)
	if aggregateErr != nil {
		status = "failed"
		httpStatus = 502
		code = contract.ErrorProvider
		total = s.prepared.reserved
	}
	if err := s.engine.finalizePrepared(ctx, s.prepared, contract.Completion{
		Token: s.prepared.token, Status: status, HTTPStatus: httpStatus, ErrorCode: code,
		Usage: total, Attempts: s.attempts, FirstByteAt: s.firstByteAt, EndedAt: ended,
	}); err != nil {
		return err
	}
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

func terminalStreamAttempt(number int, route routing.Attempt, started, ended time.Time, err *contract.Error, outcome string, committed bool, usage contract.Usage) contract.Attempt {
	return contract.Attempt{
		Number: number, ProviderID: route.Provider.ID, DeploymentID: route.Deployment.ID,
		StartedAt: started, EndedAt: ended, Outcome: outcome, ErrorCode: err.Code,
		HTTPStatus: err.HTTPStatus, Retryable: err.Retryable, OutputCommitted: committed, Usage: usage,
	}
}
