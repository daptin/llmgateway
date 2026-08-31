package llmgateway

import (
	"context"
	"sync"

	"github.com/daptin/llmgateway/adapter"
	"github.com/daptin/llmgateway/catalog"
	"github.com/daptin/llmgateway/contract"
)

type HealthReport struct {
	Checked   int
	Healthy   int
	Unhealthy int
}

type probeTarget struct {
	deployment catalog.Deployment
	checker    adapter.HealthChecker
}

// Probe executes only explicitly enabled, side-effect-free adapter health
// checks. Successful probes clear infrastructure circuit state but never clear
// provider rate-limit cooldowns.
func (e *Engine) Probe(ctx context.Context) (HealthReport, error) {
	runtime, err := e.currentRuntime()
	if err != nil {
		return HealthReport{}, err
	}
	targets := make([]probeTarget, 0)
	for _, deployment := range runtime.catalog.Deployments() {
		if !deployment.Enabled || !deployment.HealthCheck {
			continue
		}
		checker, ok := runtime.adapters[deployment.ProviderID].(adapter.HealthChecker)
		if !ok {
			continue // Reload rejects this invariant; retain a defensive guard.
		}
		targets = append(targets, probeTarget{deployment: deployment, checker: checker})
	}
	report := HealthReport{Checked: len(targets)}
	if len(targets) == 0 {
		return report, nil
	}
	jobs := make(chan probeTarget, len(targets))
	results := make(chan bool, len(targets))
	workers := min(e.healthProbeWorkers, len(targets))
	var wait sync.WaitGroup
	wait.Add(workers)
	for index := 0; index < workers; index++ {
		go func() {
			defer wait.Done()
			for target := range jobs {
				results <- e.probeDeployment(ctx, runtime.catalog.Revision(), target)
			}
		}()
	}
	for _, target := range targets {
		jobs <- target
	}
	close(jobs)
	wait.Wait()
	close(results)
	for healthy := range results {
		if healthy {
			report.Healthy++
		} else {
			report.Unhealthy++
		}
	}
	return report, ctx.Err()
}

func (e *Engine) probeDeployment(parent context.Context, revision uint64, target probeTarget) bool {
	ctx, cancel := context.WithTimeout(parent, e.healthProbeTimeout)
	err := target.checker.HealthCheck(ctx, target.deployment)
	cancel()
	outcome := "healthy"
	if err == nil {
		e.afterAttempt(parent, target.deployment, attemptLease{}, nil)
	} else {
		outcome = "unhealthy"
		e.afterAttempt(parent, target.deployment, attemptLease{}, publicError(contract.ErrorProvider, "upstream health probe failed", 502, true, err))
	}
	e.telemetry.Record(context.WithoutCancel(parent), TelemetryEvent{Name: "health.probe", Revision: revision,
		Attributes: map[string]string{"provider_id": string(target.deployment.ProviderID),
			"deployment_id": string(target.deployment.ID), "outcome": outcome}})
	return err == nil
}
