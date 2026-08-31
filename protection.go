package llmgateway

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/daptin/llmgateway/adapter"
	"github.com/daptin/llmgateway/catalog"
	"github.com/daptin/llmgateway/contract"
)

type attemptLease struct {
	concurrency string
	probe       string
}

func (e *Engine) beforeAttempt(ctx context.Context, deployment catalog.Deployment, request contract.Request) (attemptLease, error) {
	prefix := "llmgateway:deployment:" + string(deployment.ID) + ":"
	if open, _, err := e.counters.Get(ctx, prefix+"cooldown"); err != nil {
		return attemptLease{}, transientCounterError(err)
	} else if open > 0 {
		return attemptLease{}, publicError(contract.ErrorUnavailable, "deployment is cooling down", 503, true, nil)
	}
	lease := attemptLease{}
	failures, _, err := e.counters.Get(ctx, prefix+"failures")
	if err != nil {
		return lease, transientCounterError(err)
	}
	if failures >= e.circuitFailures {
		lease.probe, err = e.counters.Acquire(ctx, prefix+"probe", 1, e.circuitCooldown)
		if err != nil {
			if errors.Is(err, ErrCounterLimit) {
				return attemptLease{}, publicError(contract.ErrorUnavailable, "deployment circuit is half-open", 503, true, err)
			}
			return attemptLease{}, transientCounterError(err)
		}
	}
	if deployment.RPM >= 0 {
		value, addErr := e.counters.Add(ctx, prefix+"rpm", 1, time.Minute)
		if addErr != nil {
			e.releaseAttemptLease(ctx, lease)
			return attemptLease{}, transientCounterError(addErr)
		}
		if value > deployment.RPM {
			e.releaseAttemptLease(ctx, lease)
			return attemptLease{}, publicError(contract.ErrorRateLimit, "deployment request limit exceeded", 429, true, ErrCounterLimit)
		}
	}
	if deployment.TPM >= 0 {
		value, addErr := e.counters.Add(ctx, prefix+"tpm", request.EstimatedUsage.TotalTokens, time.Minute)
		if addErr != nil {
			e.releaseAttemptLease(ctx, lease)
			return attemptLease{}, transientCounterError(addErr)
		}
		if value > deployment.TPM {
			e.releaseAttemptLease(ctx, lease)
			return attemptLease{}, publicError(contract.ErrorRateLimit, "deployment token limit exceeded", 429, true, ErrCounterLimit)
		}
	}
	if deployment.MaxConcurrency >= 0 {
		ttl := deployment.RequestTimeout
		if ttl <= 0 {
			ttl = 2 * time.Minute
		}
		lease.concurrency, err = e.counters.Acquire(ctx, prefix+"concurrency", deployment.MaxConcurrency, ttl)
		if err != nil {
			e.releaseAttemptLease(ctx, lease)
			if errors.Is(err, ErrCounterLimit) {
				return attemptLease{}, publicError(contract.ErrorRateLimit, "deployment concurrency limit exceeded", 429, true, err)
			}
			return attemptLease{}, transientCounterError(err)
		}
	}
	return lease, nil
}

func (e *Engine) afterAttempt(ctx context.Context, deployment catalog.Deployment, lease attemptLease, result *contract.Error) {
	e.releaseAttemptLease(ctx, lease)
	prefix := "llmgateway:deployment:" + string(deployment.ID) + ":"
	settleCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), e.finalizationTimeout)
	defer cancel()
	if result == nil {
		_ = e.counters.Delete(settleCtx, prefix+"failures")
		_ = e.counters.Delete(settleCtx, prefix+"cooldown")
		return
	}
	if !result.Retryable {
		return
	}
	failures, err := e.counters.Add(settleCtx, prefix+"failures", 1, e.circuitWindow)
	if err != nil {
		return
	}
	cooldown := e.circuitCooldown
	if result.RetryAfter > cooldown {
		cooldown = result.RetryAfter
	}
	if failures >= e.circuitFailures || result.Code == contract.ErrorRateLimit {
		_, _ = e.counters.Add(settleCtx, prefix+"cooldown", 1, cooldown)
	}
}

func (e *Engine) releaseAttemptLease(ctx context.Context, lease attemptLease) {
	settleCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), e.finalizationTimeout)
	defer cancel()
	if lease.concurrency != "" {
		_ = e.counters.Release(settleCtx, lease.concurrency)
	}
	if lease.probe != "" {
		_ = e.counters.Release(settleCtx, lease.probe)
	}
}

func transientCounterError(cause error) *contract.Error {
	return publicError(contract.ErrorUnavailable, "deployment protection is unavailable", 503, true, fmt.Errorf("transient counter: %w", cause))
}

func (e *Engine) invokeProvider(ctx context.Context, provider adapter.Adapter, deployment catalog.Deployment, request contract.Request, lease attemptLease) (response contract.Response, result *contract.Error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			result = publicError(contract.ErrorProvider, "upstream provider failed", 502, false, fmt.Errorf("provider panic: %v", recovered))
			e.afterAttempt(ctx, deployment, lease, result)
			return
		}
		e.afterAttempt(ctx, deployment, lease, result)
	}()
	response, err := provider.Invoke(ctx, deployment, request)
	if err != nil {
		result = normalizeError(err, contract.ErrorProvider, 502, false)
	}
	return response, result
}

func (e *Engine) openProviderStream(ctx context.Context, provider adapter.Adapter, deployment catalog.Deployment, request contract.Request, lease attemptLease) (stream adapter.Stream, result *contract.Error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			result = publicError(contract.ErrorProvider, "upstream provider failed", 502, false, fmt.Errorf("provider panic: %v", recovered))
			e.afterAttempt(ctx, deployment, lease, result)
		}
	}()
	stream, err := provider.Stream(ctx, deployment, request)
	if err != nil {
		result = normalizeError(err, contract.ErrorProvider, 502, false)
		e.afterAttempt(ctx, deployment, lease, result)
	}
	return stream, result
}

func nextProviderEvent(ctx context.Context, stream adapter.Stream) (event contract.StreamEvent, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = publicError(contract.ErrorProvider, "upstream provider failed", 502, false, fmt.Errorf("provider stream panic: %v", recovered))
		}
	}()
	return stream.Next(ctx)
}

func nextProviderEventWithin(ctx context.Context, stream adapter.Stream, timeout time.Duration, message string) (contract.StreamEvent, error) {
	nextContext, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	event, err := nextProviderEvent(nextContext, stream)
	if errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil {
		return contract.StreamEvent{}, publicError(contract.ErrorTimeout, message, 504, true, err)
	}
	return event, err
}

func closeProviderStream(stream adapter.Stream) (err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("provider stream close panic: %v", recovered)
		}
	}()
	return stream.Close()
}
