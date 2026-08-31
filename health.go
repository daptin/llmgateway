package llmgateway

import (
	"context"
	"sync"
	"time"

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
	now := e.clock.Now()
	for _, deployment := range runtime.catalog.Deployments() {
		if !deployment.Enabled || !deployment.HealthCheck.Enabled {
			continue
		}
		checker, ok := runtime.adapters[deployment.ProviderID].(adapter.HealthChecker)
		if !ok {
			continue // Reload rejects this invariant; retain a defensive guard.
		}
		if !e.reserveHealthProbe(deployment, now) {
			continue
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

func (e *Engine) pruneHealthState(snapshot *catalog.Snapshot) {
	current := make(map[contract.ID]struct{})
	for _, deployment := range snapshot.Deployments() {
		current[deployment.ID] = struct{}{}
	}
	e.healthMu.Lock()
	for deploymentID := range e.healthLast {
		if _, exists := current[deploymentID]; !exists && !e.healthActive[deploymentID] {
			delete(e.healthLast, deploymentID)
		}
	}
	e.healthMu.Unlock()
}

func (e *Engine) probeDeployment(parent context.Context, revision uint64, target probeTarget) bool {
	defer e.completeHealthProbe(target.deployment.ID)
	ctx, cancel := context.WithTimeout(parent, target.deployment.HealthCheck.Timeout)
	err := target.checker.HealthCheck(ctx, target.deployment)
	cancel()
	outcome := "healthy"
	if err == nil {
		e.recordHealthSuccess(parent, target.deployment)
	} else {
		outcome = "unhealthy"
		e.recordHealthFailure(parent, target.deployment)
	}
	e.telemetry.Record(context.WithoutCancel(parent), TelemetryEvent{Name: "health.probe", Revision: revision,
		Attributes: map[string]string{"provider_id": string(target.deployment.ProviderID),
			"deployment_id": string(target.deployment.ID), "outcome": outcome}})
	return err == nil
}

func (e *Engine) reserveHealthProbe(deployment catalog.Deployment, now time.Time) bool {
	e.healthMu.Lock()
	defer e.healthMu.Unlock()
	if e.healthActive[deployment.ID] || (!e.healthLast[deployment.ID].IsZero() && now.Sub(e.healthLast[deployment.ID]) < deployment.HealthCheck.Interval) {
		return false
	}
	e.healthActive[deployment.ID] = true
	e.healthLast[deployment.ID] = now
	return true
}

func (e *Engine) completeHealthProbe(deploymentID contract.ID) {
	e.healthMu.Lock()
	delete(e.healthActive, deploymentID)
	e.healthMu.Unlock()
}

func (e *Engine) recordHealthSuccess(ctx context.Context, deployment catalog.Deployment) {
	prefix := "llmgateway:deployment:" + string(deployment.ID) + ":"
	settleCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), e.finalizationTimeout)
	defer cancel()
	_ = e.counters.Delete(settleCtx, prefix+"health_failures")
	_ = e.counters.Delete(settleCtx, prefix+"failures")
	_ = e.counters.Delete(settleCtx, prefix+"circuit_cooldown")
}

func (e *Engine) recordHealthFailure(ctx context.Context, deployment catalog.Deployment) {
	prefix := "llmgateway:deployment:" + string(deployment.ID) + ":"
	settleCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), e.finalizationTimeout)
	defer cancel()
	ttl := deployment.HealthCheck.Interval * time.Duration(deployment.HealthCheck.FailureThreshold+1)
	failures, err := e.counters.Add(settleCtx, prefix+"health_failures", 1, ttl)
	if err == nil && failures >= deployment.HealthCheck.FailureThreshold {
		_, _ = e.counters.Add(settleCtx, prefix+"circuit_cooldown", 1, e.circuitCooldown)
	}
}
