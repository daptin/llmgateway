package llmgateway

import (
	"context"

	"github.com/daptin/llmgateway/catalog"
	"github.com/daptin/llmgateway/contract"
	"github.com/daptin/llmgateway/guardrail"
)

type runtimeGuardrail struct {
	configuration catalog.Guardrail
	checker       guardrail.Checker
}

func (e *Engine) checkInput(ctx context.Context, runtime *runtimeSnapshot, model catalog.Model, request contract.Request) error {
	for _, bound := range runtime.guardrails[model.ID] {
		if bound.configuration.Phase != "input" && bound.configuration.Phase != "both" {
			continue
		}
		decision, err := runGuardrail(ctx, bound.configuration, func(checkCtx context.Context) (guardrail.Decision, error) {
			return bound.checker.CheckInput(checkCtx, request)
		})
		e.recordGuardrail(ctx, request, bound.configuration, "input", decision, err)
		if guardrailErr := guardrailOutcome(bound.configuration, decision, err); guardrailErr != nil {
			return guardrailErr
		}
	}
	return nil
}

func (e *Engine) checkOutput(ctx context.Context, prepared preparedRequest, response contract.Response) error {
	for _, bound := range prepared.runtime.guardrails[prepared.model.ID] {
		if bound.configuration.Phase != "output" && bound.configuration.Phase != "both" {
			continue
		}
		decision, err := runGuardrail(ctx, bound.configuration, func(checkCtx context.Context) (guardrail.Decision, error) {
			return bound.checker.CheckOutput(checkCtx, prepared.request, response)
		})
		e.recordGuardrail(ctx, prepared.request, bound.configuration, "output", decision, err)
		if guardrailErr := guardrailOutcome(bound.configuration, decision, err); guardrailErr != nil {
			return guardrailErr
		}
	}
	return nil
}

func (e *Engine) checkStream(ctx context.Context, prepared preparedRequest, event contract.StreamEvent) error {
	for _, bound := range prepared.runtime.guardrails[prepared.model.ID] {
		if bound.configuration.Phase != "output" && bound.configuration.Phase != "both" {
			continue
		}
		decision, err := runGuardrail(ctx, bound.configuration, func(checkCtx context.Context) (guardrail.Decision, error) {
			return bound.checker.CheckStream(checkCtx, prepared.request, event)
		})
		e.recordGuardrail(ctx, prepared.request, bound.configuration, "stream", decision, err)
		if guardrailErr := guardrailOutcome(bound.configuration, decision, err); guardrailErr != nil {
			return guardrailErr
		}
	}
	return nil
}

func validateStreamingGuardrails(runtime *runtimeSnapshot, model catalog.Model) error {
	for _, bound := range runtime.guardrails[model.ID] {
		if (bound.configuration.Phase == "output" || bound.configuration.Phase == "both") && !bound.checker.SupportsStreaming() {
			return publicError(contract.ErrorInvalidRequest, "streaming is incompatible with the model guardrails", 400, false, nil)
		}
	}
	return nil
}

func runGuardrail(ctx context.Context, configuration catalog.Guardrail, check func(context.Context) (guardrail.Decision, error)) (guardrail.Decision, error) {
	if configuration.Timeout <= 0 {
		return check(ctx)
	}
	checkCtx, cancel := context.WithTimeout(ctx, configuration.Timeout)
	defer cancel()
	return check(checkCtx)
}

func guardrailOutcome(configuration catalog.Guardrail, decision guardrail.Decision, err error) error {
	if configuration.AuditOnly {
		return nil
	}
	if err != nil {
		if configuration.FailMode == "open" {
			return nil
		}
		return publicError(contract.ErrorUnavailable, "guardrail is unavailable", 503, false, err)
	}
	if !decision.Allowed {
		return publicError(contract.ErrorPermission, "request rejected by guardrail", 400, false, nil)
	}
	return nil
}

func (e *Engine) recordGuardrail(ctx context.Context, request contract.Request, configuration catalog.Guardrail, phase string, decision guardrail.Decision, err error) {
	outcome := "allowed"
	if err != nil {
		outcome = "error"
	} else if !decision.Allowed {
		outcome = "denied"
	}
	e.telemetry.Record(ctx, TelemetryEvent{
		Name: "guardrail", RequestID: request.ID,
		Attributes: map[string]string{"guardrail_id": string(configuration.ID), "phase": phase, "outcome": outcome, "reason": decision.Reason},
	})
}
