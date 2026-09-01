package llmgateway

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/daptin/llmgateway/adapter"
	"github.com/daptin/llmgateway/catalog"
	"github.com/daptin/llmgateway/contract"
	"github.com/daptin/llmgateway/guardrail"
	"github.com/daptin/llmgateway/internal/routing"
)

var (
	ErrDraining        = errors.New("llmgateway is draining")
	ErrNotReady        = errors.New("llmgateway has no valid catalog snapshot")
	ErrCounterLimit    = errors.New("llmgateway transient counter limit exceeded")
	ErrAdmissionDenied = errors.New("llmgateway metering admission denied")
)

type Dependencies struct {
	Catalog    CatalogSource
	Secrets    SecretResolver
	Adapters   *adapter.Registry
	Authorizer Authorizer
	Metering   MeteringPort
	Counters   CounterStore
	Cache      ResponseCache
	Guardrails *guardrail.Registry
	Telemetry  TelemetrySink
	Selector   Selector
	Clock      Clock
}

type Options struct {
	MaxAttempts            int
	DefaultMaxOutputTokens int64
	FinalizationTimeout    time.Duration
	BaseRetryDelay         time.Duration
	MaxRetryDelay          time.Duration
	CircuitFailures        int64
	CircuitWindow          time.Duration
	CircuitCooldown        time.Duration
	CacheTTL               time.Duration
	CacheTimeout           time.Duration
	CacheFillWait          time.Duration
	MaxCacheEntryBytes     int
	FirstEventTimeout      time.Duration
	StreamIdleTimeout      time.Duration
	RequestTimeout         time.Duration
	HealthProbeWorkers     int
}

type runtimeSnapshot struct {
	catalog      *catalog.Snapshot
	adapters     map[contract.ID]adapter.Adapter
	capabilities map[contract.ID]adapter.Capabilities
	guardrails   map[contract.ID][]runtimeGuardrail
	closeOnce    sync.Once
}

func (r *runtimeSnapshot) Capabilities(providerID contract.ID) (adapter.Capabilities, bool) {
	value, ok := r.capabilities[providerID]
	return value, ok
}

func (r *runtimeSnapshot) closeIdleConnections() {
	if r == nil {
		return
	}
	r.closeOnce.Do(func() {
		for _, instance := range r.adapters {
			if closer, ok := instance.(adapter.IdleConnectionCloser); ok {
				closer.CloseIdleConnections()
			}
		}
	})
}

type Engine struct {
	catalog                CatalogSource
	secrets                SecretResolver
	adapters               *adapter.Registry
	authorizer             Authorizer
	metering               MeteringPort
	counters               CounterStore
	cache                  ResponseCache
	guardrails             *guardrail.Registry
	telemetry              TelemetrySink
	selector               Selector
	clock                  Clock
	maxAttempts            int
	defaultMaxOutputTokens int64
	finalizationTimeout    time.Duration
	baseRetryDelay         time.Duration
	maxRetryDelay          time.Duration
	circuitFailures        int64
	circuitWindow          time.Duration
	circuitCooldown        time.Duration
	cacheTTL               time.Duration
	cacheTimeout           time.Duration
	cacheFillWait          time.Duration
	maxCacheEntryBytes     int
	firstEventTimeout      time.Duration
	streamIdleTimeout      time.Duration
	requestTimeout         time.Duration
	healthProbeWorkers     int
	healthMu               sync.Mutex
	healthLast             map[contract.ID]time.Time
	healthActive           map[contract.ID]bool
	snapshot               atomic.Pointer[runtimeSnapshot]
	draining               atomic.Bool
	reloadMu               sync.Mutex
	statusMu               sync.RWMutex
	rejectedRevision       uint64
	reloadFailureStage     string
	activeMu               sync.Mutex
	activeRequests         int64
	drained                chan struct{}
	retired                []*runtimeSnapshot
	snapshotCloseOnce      sync.Once
}

func New(dependencies Dependencies, options Options) (*Engine, error) {
	if dependencies.Catalog == nil {
		return nil, errors.New("catalog source is required")
	}
	if dependencies.Adapters == nil || dependencies.Authorizer == nil || dependencies.Metering == nil || dependencies.Counters == nil || dependencies.Cache == nil || dependencies.Guardrails == nil || dependencies.Telemetry == nil || dependencies.Selector == nil || dependencies.Clock == nil {
		return nil, errors.New("adapters, authorizer, metering, counters, cache, guardrails, telemetry, selector, and clock are required")
	}
	if options.MaxAttempts == 0 {
		options.MaxAttempts = 3
	}
	if options.DefaultMaxOutputTokens == 0 {
		options.DefaultMaxOutputTokens = 4096
	}
	if options.DefaultMaxOutputTokens < 1 {
		return nil, errors.New("default maximum output tokens must be positive")
	}
	if options.MaxAttempts < 1 || options.MaxAttempts > 12 {
		return nil, errors.New("max attempts must be between 1 and 12")
	}
	if options.FinalizationTimeout == 0 {
		options.FinalizationTimeout = 5 * time.Second
	}
	if options.FinalizationTimeout < 0 {
		return nil, errors.New("finalization timeout must be positive")
	}
	if options.BaseRetryDelay == 0 {
		options.BaseRetryDelay = 100 * time.Millisecond
	}
	if options.MaxRetryDelay == 0 {
		options.MaxRetryDelay = 2 * time.Second
	}
	if options.BaseRetryDelay < 0 || options.MaxRetryDelay < options.BaseRetryDelay {
		return nil, errors.New("retry delays must be positive and ordered")
	}
	if options.CircuitFailures == 0 {
		options.CircuitFailures = 5
	}
	if options.CircuitWindow == 0 {
		options.CircuitWindow = 30 * time.Second
	}
	if options.CircuitCooldown == 0 {
		options.CircuitCooldown = 15 * time.Second
	}
	if options.CircuitFailures < 1 || options.CircuitWindow < options.CircuitCooldown || options.CircuitCooldown < time.Second {
		return nil, errors.New("circuit settings must be positive and the failure window must cover cooldown")
	}
	if options.CacheTTL == 0 {
		options.CacheTTL = 5 * time.Minute
	}
	if options.MaxCacheEntryBytes == 0 {
		options.MaxCacheEntryBytes = 8 << 20
	}
	if options.CacheTimeout == 0 {
		options.CacheTimeout = 250 * time.Millisecond
	}
	if options.CacheFillWait == 0 {
		options.CacheFillWait = 5 * time.Second
	}
	if options.CacheTTL < 0 || options.CacheTimeout < 1 || options.CacheFillWait < 1 || options.MaxCacheEntryBytes < 1 {
		return nil, errors.New("cache TTL, timeouts, and entry bound must be positive")
	}
	if options.FirstEventTimeout == 0 {
		options.FirstEventTimeout = 30 * time.Second
	}
	if options.StreamIdleTimeout == 0 {
		options.StreamIdleTimeout = 60 * time.Second
	}
	if options.FirstEventTimeout < time.Millisecond || options.StreamIdleTimeout < time.Millisecond {
		return nil, errors.New("stream first-event and idle timeouts must be positive")
	}
	if options.RequestTimeout == 0 {
		options.RequestTimeout = 120 * time.Second
	}
	if options.RequestTimeout < time.Millisecond {
		return nil, errors.New("request timeout must be positive")
	}
	if options.HealthProbeWorkers == 0 {
		options.HealthProbeWorkers = 8
	}
	if options.HealthProbeWorkers < 1 || options.HealthProbeWorkers > 64 {
		return nil, errors.New("health probe worker bounds are invalid")
	}
	dependencies.Adapters.Freeze()
	dependencies.Guardrails.Freeze()
	drained := make(chan struct{})
	close(drained)
	return &Engine{
		catalog: dependencies.Catalog, secrets: dependencies.Secrets, adapters: dependencies.Adapters,
		authorizer: dependencies.Authorizer, metering: dependencies.Metering, counters: dependencies.Counters, cache: dependencies.Cache,
		guardrails: dependencies.Guardrails, telemetry: dependencies.Telemetry, selector: dependencies.Selector,
		clock: dependencies.Clock, maxAttempts: options.MaxAttempts, defaultMaxOutputTokens: options.DefaultMaxOutputTokens, finalizationTimeout: options.FinalizationTimeout,
		baseRetryDelay: options.BaseRetryDelay, maxRetryDelay: options.MaxRetryDelay,
		circuitFailures: options.CircuitFailures, circuitWindow: options.CircuitWindow, circuitCooldown: options.CircuitCooldown,
		cacheTTL: options.CacheTTL, cacheTimeout: options.CacheTimeout, cacheFillWait: options.CacheFillWait, maxCacheEntryBytes: options.MaxCacheEntryBytes,
		firstEventTimeout: options.FirstEventTimeout, streamIdleTimeout: options.StreamIdleTimeout,
		requestTimeout:     options.RequestTimeout,
		healthProbeWorkers: options.HealthProbeWorkers, healthLast: make(map[contract.ID]time.Time), healthActive: make(map[contract.ID]bool),
		drained: drained,
	}, nil
}

func (e *Engine) Reload(ctx context.Context) error {
	if e.draining.Load() {
		return ErrDraining
	}
	e.reloadMu.Lock()
	defer e.reloadMu.Unlock()
	var after uint64
	if current := e.snapshot.Load(); current != nil {
		after = current.catalog.Revision()
	}
	document, err := e.catalog.Load(ctx, after)
	if err != nil {
		if !errors.Is(err, catalog.ErrStaleRevision) {
			e.recordReloadFailure(0, "load")
		}
		return err
	}
	compiled, err := catalog.Compile(document)
	if err != nil {
		e.recordReloadFailure(document.Revision, "validate")
		return err
	}
	candidate := &runtimeSnapshot{
		catalog: compiled, adapters: make(map[contract.ID]adapter.Adapter),
		capabilities: make(map[contract.ID]adapter.Capabilities),
	}
	installed := false
	defer func() {
		if !installed {
			candidate.closeIdleConnections()
		}
	}()
	for _, provider := range compiled.Providers() {
		if !provider.Enabled {
			continue
		}
		factory, ok := e.adapters.Factory(provider.Type)
		if !ok {
			e.recordReloadFailure(document.Revision, "adapter_registry")
			return fmt.Errorf("provider %q uses unregistered adapter %q", provider.ID, provider.Type)
		}
		var secretBytes []byte
		if provider.SecretRef != "" {
			if e.secrets == nil {
				e.recordReloadFailure(document.Revision, "secret")
				return fmt.Errorf("provider %q requires a secret resolver", provider.ID)
			}
			secretBytes, err = e.secrets.ResolveSecret(ctx, provider.SecretRef)
			if err != nil {
				e.recordReloadFailure(document.Revision, "secret")
				return fmt.Errorf("resolve provider %q secret: %w", provider.ID, err)
			}
		}
		instance, buildErr := factory.Build(ctx, provider, adapter.NewSecret(secretBytes))
		for index := range secretBytes {
			secretBytes[index] = 0
		}
		if buildErr != nil {
			e.recordReloadFailure(document.Revision, "adapter_build")
			return fmt.Errorf("build provider %q adapter: %w", provider.ID, buildErr)
		}
		if instance == nil {
			e.recordReloadFailure(document.Revision, "adapter_build")
			return fmt.Errorf("provider %q adapter factory returned nil", provider.ID)
		}
		candidate.adapters[provider.ID] = instance
		declared := instance.Capabilities()
		operations := make(map[contract.Operation]bool, len(declared.Operations))
		for operation, supported := range declared.Operations {
			operations[operation] = supported
		}
		features := make(map[string]bool, len(declared.Features))
		for feature, supported := range declared.Features {
			features[feature] = supported
		}
		candidate.capabilities[provider.ID] = adapter.Capabilities{Operations: operations, Features: features}
	}
	for _, deployment := range compiled.Deployments() {
		if !deployment.Enabled {
			continue
		}
		instance := candidate.adapters[deployment.ProviderID]
		if validator, ok := instance.(adapter.DeploymentValidator); ok {
			if validateErr := validator.ValidateDeployment(deployment); validateErr != nil {
				e.recordReloadFailure(document.Revision, "deployment_config")
				return fmt.Errorf("validate deployment %q: %w", deployment.ID, validateErr)
			}
		}
		if deployment.HealthCheck.Enabled {
			if _, ok := instance.(adapter.HealthChecker); ok {
				continue
			}
			e.recordReloadFailure(document.Revision, "health_check")
			return fmt.Errorf("deployment %q enables health checks on an adapter without probe support", deployment.ID)
		}
	}
	candidate.guardrails = make(map[contract.ID][]runtimeGuardrail)
	for _, model := range compiled.Models() {
		for _, configuration := range compiled.GuardrailsForModel(model.ID) {
			factory, ok := e.guardrails.Factory(configuration.Kind)
			if !ok {
				e.recordReloadFailure(document.Revision, "guardrail_registry")
				return fmt.Errorf("guardrail %q uses unregistered kind %q", configuration.ID, configuration.Kind)
			}
			checker, buildErr := factory.Build(configuration)
			if buildErr != nil {
				e.recordReloadFailure(document.Revision, "guardrail_build")
				return fmt.Errorf("build guardrail %q: %w", configuration.ID, buildErr)
			}
			if checker == nil {
				e.recordReloadFailure(document.Revision, "guardrail_build")
				return fmt.Errorf("guardrail %q factory returned nil", configuration.ID)
			}
			candidate.guardrails[model.ID] = append(candidate.guardrails[model.ID], runtimeGuardrail{configuration: configuration, checker: checker})
		}
	}
	current := e.snapshot.Load()
	if current != nil && candidate.catalog.Revision() <= current.catalog.Revision() {
		return catalog.ErrStaleRevision
	}
	previous := e.snapshot.Swap(candidate)
	installed = true
	if previous != nil {
		e.retire(previous)
	}
	e.pruneHealthState(compiled)
	e.statusMu.Lock()
	e.rejectedRevision = 0
	e.reloadFailureStage = ""
	e.statusMu.Unlock()
	return nil
}

// Status is a redacted, point-in-time view of engine lifecycle and catalog
// readiness. Failure stages are stable categories and never contain host or
// provider error text.
type Status struct {
	Ready            bool   `json:"ready"`
	Draining         bool   `json:"draining"`
	Degraded         bool   `json:"degraded"`
	Revision         uint64 `json:"revision"`
	RejectedRevision uint64 `json:"rejected_revision,omitempty"`
	ReloadStage      string `json:"reload_stage,omitempty"`
}

func (e *Engine) Status() Status {
	status := Status{Draining: e.draining.Load()}
	if current := e.snapshot.Load(); current != nil {
		status.Revision = current.catalog.Revision()
		status.Ready = !status.Draining
	}
	e.statusMu.RLock()
	status.RejectedRevision = e.rejectedRevision
	status.ReloadStage = e.reloadFailureStage
	e.statusMu.RUnlock()
	status.Degraded = status.RejectedRevision > status.Revision || (status.ReloadStage == "load" && status.Revision > 0)
	return status
}

func (e *Engine) recordReloadFailure(revision uint64, stage string) {
	e.statusMu.Lock()
	e.rejectedRevision = revision
	e.reloadFailureStage = stage
	e.statusMu.Unlock()
	e.telemetry.Record(context.Background(), TelemetryEvent{Name: "catalog.reload_failed", Revision: revision, Attributes: map[string]string{"stage": stage}})
}

func (e *Engine) Snapshot() (*catalog.Snapshot, error) {
	if e.draining.Load() {
		return nil, ErrDraining
	}
	current := e.snapshot.Load()
	if current == nil {
		return nil, ErrNotReady
	}
	return current.catalog, nil
}

// Authorize evaluates model visibility through the same host policy used by
// invocation. Protocol handlers use it for discovery without duplicating ACLs.
func (e *Engine) Authorize(ctx context.Context, principal contract.Principal, publicModel string) error {
	current, err := e.currentRuntime()
	if err != nil {
		return err
	}
	model, ok := current.catalog.ModelByName(publicModel)
	if !ok || !model.Enabled {
		return publicError(contract.ErrorModelNotFound, "model not found", 404, false, nil)
	}
	if err := e.authorizer.Authorize(ctx, principal, model); err != nil {
		return normalizeError(err, contract.ErrorPermission, 403, false)
	}
	return nil
}

func (e *Engine) Drain(ctx context.Context) error {
	e.activeMu.Lock()
	e.draining.Store(true)
	drained := e.drained
	e.activeMu.Unlock()
	select {
	case <-drained:
		e.snapshotCloseOnce.Do(e.closeSnapshots)
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (e *Engine) Invoke(ctx context.Context, principal contract.Principal, request contract.Request) (contract.Response, error) {
	ctx, cancelRequest := context.WithTimeout(ctx, e.requestTimeout)
	defer cancelRequest()
	if !e.beginRequest() {
		return contract.Response{}, ErrDraining
	}
	defer e.endRequest()
	if request.Stream {
		return contract.Response{}, publicError(contract.ErrorInvalidRequest, "invalid non-streaming request", 400, false, nil)
	}
	prepared, err := e.resolve(ctx, principal, request)
	if err != nil {
		return contract.Response{}, err
	}
	cacheKey, cached, cacheHit := e.lookupCache(ctx, principal, prepared)
	cacheFillLease := ""
	if !cacheHit && cacheKey != "" {
		cached, cacheHit, cacheFillLease = e.coordinateCacheFill(ctx, prepared.request, cacheKey)
		if cacheFillLease != "" {
			defer e.releaseCacheFill(ctx, cacheFillLease)
		}
	}
	if cacheHit {
		prepared, err = e.admitCached(ctx, principal, prepared, cached.Usage)
	} else {
		prepared, err = e.admit(ctx, principal, prepared)
	}
	if err != nil {
		return contract.Response{}, err
	}
	settled := false
	defer func() {
		if !settled {
			_ = e.cancelPrepared(ctx, prepared, contract.Cancellation{Token: prepared.token, Reason: "invoke_abandoned", EndedAt: e.clock.Now()})
		}
	}()
	finish := func(completion contract.Completion) error {
		if finishErr := e.finalizePrepared(ctx, prepared, completion); finishErr != nil {
			return finishErr
		}
		settled = true
		return nil
	}
	if cacheHit {
		cached.RequestID = prepared.request.ID
		cached.Model = prepared.request.PublicModel
		cached.Usage.CostMicros = 0
		if guardrailErr := e.checkOutput(ctx, prepared, cached); guardrailErr != nil {
			normalized := normalizeError(guardrailErr, contract.ErrorPermission, 400, false)
			if finishErr := finish(contract.Completion{Token: prepared.token, Status: "rejected", HTTPStatus: normalized.HTTPStatus, ErrorCode: normalized.Code, Usage: cached.Usage, EndedAt: e.clock.Now(), CacheStatus: "hit"}); finishErr != nil {
				return contract.Response{}, finishErr
			}
			return contract.Response{}, normalized
		}
		if finishErr := finish(contract.Completion{Token: prepared.token, Status: "succeeded", HTTPStatus: 200, Usage: cached.Usage, EndedAt: e.clock.Now(), CacheStatus: "hit"}); finishErr != nil {
			return contract.Response{}, finishErr
		}
		return cached, nil
	}
	attempts := make([]contract.Attempt, 0, len(prepared.plan.Attempts))
	attemptTotal := func() contract.Usage {
		usage, aggregateErr := aggregateAttemptUsage(attempts)
		if aggregateErr != nil {
			return prepared.reserved
		}
		return usage
	}
	attemptNumber := 0
	for index, routeAttempt := range prepared.plan.Attempts {
		lease, gateErr := e.beforeAttempt(ctx, routeAttempt.Deployment, prepared.request)
		if gateErr != nil {
			normalized := normalizeError(gateErr, contract.ErrorUnavailable, 503, true)
			if !normalized.Retryable || index == len(prepared.plan.Attempts)-1 {
				if finishErr := finish(contract.Completion{Token: prepared.token, Status: "failed", HTTPStatus: normalized.HTTPStatus, ErrorCode: normalized.Code, Usage: attemptTotal(), Attempts: attempts, EndedAt: e.clock.Now()}); finishErr != nil {
					return contract.Response{}, finishErr
				}
				return contract.Response{}, normalized
			}
			if waitErr := e.waitForRetry(ctx, index, normalized); waitErr != nil {
				return contract.Response{}, waitErr
			}
			continue
		}
		started := e.clock.Now()
		attemptNumber++
		response, normalized := e.invokeProvider(ctx, prepared.runtime.adapters[routeAttempt.Provider.ID], routeAttempt.Deployment, prepared.request, lease)
		ended := e.clock.Now()
		if normalized != nil {
			attempts = append(attempts, contract.Attempt{
				Number: attemptNumber, ProviderID: routeAttempt.Provider.ID, DeploymentID: routeAttempt.Deployment.ID,
				StartedAt: started, EndedAt: ended, Outcome: "failed", ErrorCode: normalized.Code,
				HTTPStatus: normalized.HTTPStatus, Retryable: normalized.Retryable, Usage: prepared.attemptExposure[index],
			})
			if !normalized.Retryable || index == len(prepared.plan.Attempts)-1 {
				if finishErr := finish(contract.Completion{Token: prepared.token, Status: "failed", HTTPStatus: normalized.HTTPStatus, ErrorCode: normalized.Code, Usage: attemptTotal(), Attempts: attempts, EndedAt: ended}); finishErr != nil {
					return contract.Response{}, finishErr
				}
				return contract.Response{}, normalized
			}
			if waitErr := e.waitForRetry(ctx, index, normalized); waitErr != nil {
				if finishErr := finish(contract.Completion{Token: prepared.token, Status: "cancelled", HTTPStatus: 499, ErrorCode: contract.ErrorProvider, Usage: attemptTotal(), Attempts: attempts, EndedAt: e.clock.Now()}); finishErr != nil {
					return contract.Response{}, finishErr
				}
				return contract.Response{}, waitErr
			}
			continue
		}
		usage, usageErr := settledUsage(response.Usage, prepared.request.EstimatedUsage, routeAttempt.Deployment.Pricing)
		if usageErr != nil || !usage.Valid() {
			normalized := publicError(contract.ErrorProvider, "provider returned invalid usage", 502, false, usageErr)
			attempts = append(attempts, contract.Attempt{Number: attemptNumber, ProviderID: routeAttempt.Provider.ID, DeploymentID: routeAttempt.Deployment.ID, StartedAt: started, EndedAt: ended, Outcome: "failed", ErrorCode: normalized.Code, HTTPStatus: normalized.HTTPStatus, Usage: prepared.attemptExposure[index]})
			if finishErr := finish(contract.Completion{Token: prepared.token, Status: "failed", HTTPStatus: 502, ErrorCode: normalized.Code, Usage: attemptTotal(), Attempts: attempts, EndedAt: ended}); finishErr != nil {
				return contract.Response{}, finishErr
			}
			return contract.Response{}, normalized
		}
		response.RequestID = prepared.request.ID
		response.Model = prepared.request.PublicModel
		response.Usage = usage
		attempts = append(attempts, contract.Attempt{Number: attemptNumber, ProviderID: routeAttempt.Provider.ID, DeploymentID: routeAttempt.Deployment.ID, StartedAt: started, EndedAt: ended, Outcome: "succeeded", HTTPStatus: 200, Usage: usage})
		total, aggregateErr := aggregateAttemptUsage(attempts)
		if aggregateErr != nil {
			normalized := publicError(contract.ErrorProvider, "provider usage exceeds supported aggregate range", 502, false, aggregateErr)
			if finishErr := finish(contract.Completion{Token: prepared.token, Status: "failed", HTTPStatus: 502, ErrorCode: normalized.Code, Usage: prepared.reserved, Attempts: attempts, EndedAt: ended}); finishErr != nil {
				return contract.Response{}, finishErr
			}
			return contract.Response{}, normalized
		}
		if guardrailErr := e.checkOutput(ctx, prepared, response); guardrailErr != nil {
			normalized := normalizeError(guardrailErr, contract.ErrorPermission, 400, false)
			if finishErr := finish(contract.Completion{Token: prepared.token, Status: "rejected", HTTPStatus: normalized.HTTPStatus, ErrorCode: normalized.Code, Usage: total, Attempts: attempts, EndedAt: ended}); finishErr != nil {
				return contract.Response{}, finishErr
			}
			return contract.Response{}, normalized
		}
		if err := finish(contract.Completion{Token: prepared.token, Status: "succeeded", HTTPStatus: 200, Usage: total, Attempts: attempts, EndedAt: ended}); err != nil {
			return contract.Response{}, err
		}
		e.storeCache(ctx, cacheKey, response)
		return response, nil
	}
	return contract.Response{}, publicError(contract.ErrorUnavailable, "no healthy deployment", 503, true, nil)
}

type preparedRequest struct {
	runtime         *runtimeSnapshot
	request         contract.Request
	model           catalog.Model
	plan            routing.Plan
	token           contract.ReservationToken
	principal       contract.Principal
	reserved        contract.Usage
	attemptExposure []contract.Usage
}

func (e *Engine) prepare(ctx context.Context, principal contract.Principal, request contract.Request) (preparedRequest, error) {
	resolved, err := e.resolve(ctx, principal, request)
	if err != nil {
		return preparedRequest{}, err
	}
	return e.admit(ctx, principal, resolved)
}

func (e *Engine) resolve(ctx context.Context, principal contract.Principal, request contract.Request) (preparedRequest, error) {
	runtime, err := e.currentRuntime()
	if err != nil {
		return preparedRequest{}, err
	}
	if request.ID == "" || request.PublicModel == "" || !request.Operation.Valid() {
		return preparedRequest{}, publicError(contract.ErrorInvalidRequest, "invalid request", 400, false, errors.New("request id, model, and operation are required"))
	}
	if request.StartedAt.IsZero() {
		request.StartedAt = e.clock.Now()
	}
	model, ok := runtime.catalog.ModelByName(request.PublicModel)
	if !ok || !model.Enabled {
		return preparedRequest{}, publicError(contract.ErrorModelNotFound, "model not found", 404, false, nil)
	}
	request, err = runtime.catalog.ApplyDefaults(model.ID, request, e.defaultMaxOutputTokens)
	if err != nil {
		return preparedRequest{}, publicError(contract.ErrorInternal, "failed to normalize model defaults", 500, false, err)
	}
	if err := applyRequestExposure(&request); err != nil {
		return preparedRequest{}, publicError(contract.ErrorInvalidRequest, "request token exposure exceeds supported range", 400, false, err)
	}
	if err := request.Validate(); err != nil {
		return preparedRequest{}, publicError(contract.ErrorInvalidRequest, "invalid request", 400, false, err)
	}
	request, err = applyModelParameterPolicy(model, request)
	if err != nil {
		return preparedRequest{}, publicError(contract.ErrorInvalidRequest, "request uses a capability disabled for this model", 400, false, err)
	}
	if err := request.Validate(); err != nil {
		return preparedRequest{}, publicError(contract.ErrorInvalidRequest, "invalid normalized request", 400, false, err)
	}
	if err := e.authorizer.Authorize(ctx, principal, model); err != nil {
		return preparedRequest{}, normalizeError(err, contract.ErrorPermission, 403, false)
	}
	if request.Stream {
		if err := validateStreamingGuardrails(runtime, model); err != nil {
			return preparedRequest{}, err
		}
	}
	if err := e.checkInput(ctx, runtime, model, request); err != nil {
		return preparedRequest{}, err
	}
	return preparedRequest{runtime: runtime, request: request, model: model, principal: principal}, nil
}

func (e *Engine) admit(ctx context.Context, principal contract.Principal, prepared preparedRequest) (preparedRequest, error) {
	plan, err := routing.Build(prepared.runtime.catalog, prepared.request.PublicModel, prepared.request.Operation, requiredAdapterFeatures(prepared.request), prepared.runtime, e.selector)
	if err != nil {
		return preparedRequest{}, publicError(contract.ErrorUnavailable, "no healthy deployment", 503, true, err)
	}
	if len(plan.Attempts) > e.maxAttempts {
		plan.Attempts = plan.Attempts[:e.maxAttempts]
	}
	attemptEstimate := prepared.request.EstimatedUsage
	if normalizeErr := normalizeTokenTotal(&attemptEstimate); normalizeErr != nil || !attemptEstimate.Valid() {
		return preparedRequest{}, publicError(contract.ErrorInvalidRequest, "invalid usage estimate", 400, false, nil)
	}
	exposure, attemptExposure, exposureErr := reservationExposure(attemptEstimate, plan.Attempts)
	if exposureErr != nil {
		return preparedRequest{}, publicError(contract.ErrorInvalidRequest, "usage estimate exceeds supported range", 400, false, exposureErr)
	}
	prepared.request.EstimatedUsage = attemptEstimate
	prepared.plan = plan
	prepared.attemptExposure = attemptExposure
	return e.reserve(ctx, principal, prepared, exposure)
}

func (e *Engine) admitCached(ctx context.Context, principal contract.Principal, prepared preparedRequest, usage contract.Usage) (preparedRequest, error) {
	if normalizeErr := normalizeTokenTotal(&usage); normalizeErr != nil || !usage.Valid() {
		return preparedRequest{}, publicError(contract.ErrorInternal, "cached usage is invalid", 500, false, normalizeErr)
	}
	usage.CostMicros = 0
	return e.reserve(ctx, principal, prepared, usage)
}

func (e *Engine) reserve(ctx context.Context, principal contract.Principal, prepared preparedRequest, exposure contract.Usage) (preparedRequest, error) {
	token, err := e.metering.Admit(ctx, contract.Admission{
		RequestID: prepared.request.ID, Principal: principal, ModelID: prepared.model.ID, Operation: prepared.request.Operation,
		StartedAt: prepared.request.StartedAt, EstimatedUsage: exposure,
	})
	if err != nil {
		if errors.Is(err, ErrAdmissionDenied) {
			return preparedRequest{}, publicError(contract.ErrorInsufficientQuota, "insufficient quota", 402, false, err)
		}
		return preparedRequest{}, publicError(contract.ErrorInternal, "metering admission failed", 500, false, err)
	}
	prepared.token = token
	prepared.reserved = exposure
	return prepared, nil
}

func (e *Engine) currentRuntime() (*runtimeSnapshot, error) {
	current := e.snapshot.Load()
	if current == nil {
		return nil, ErrNotReady
	}
	return current, nil
}

func (e *Engine) beginRequest() bool {
	e.activeMu.Lock()
	defer e.activeMu.Unlock()
	if e.draining.Load() {
		return false
	}
	if e.activeRequests == 0 {
		e.drained = make(chan struct{})
	}
	e.activeRequests++
	return true
}

func (e *Engine) endRequest() {
	e.activeMu.Lock()
	if e.activeRequests < 1 {
		e.activeMu.Unlock()
		panic("llmgateway: unbalanced request lifecycle")
	}
	e.activeRequests--
	var retired []*runtimeSnapshot
	if e.activeRequests == 0 {
		close(e.drained)
		retired = e.retired
		e.retired = nil
	}
	e.activeMu.Unlock()
	for _, snapshot := range retired {
		snapshot.closeIdleConnections()
	}
}

func (e *Engine) retire(snapshot *runtimeSnapshot) {
	e.activeMu.Lock()
	if e.activeRequests == 0 {
		e.activeMu.Unlock()
		snapshot.closeIdleConnections()
		return
	}
	e.retired = append(e.retired, snapshot)
	e.activeMu.Unlock()
}

func (e *Engine) closeSnapshots() {
	e.activeMu.Lock()
	retired := e.retired
	e.retired = nil
	current := e.snapshot.Load()
	e.activeMu.Unlock()
	for _, snapshot := range retired {
		snapshot.closeIdleConnections()
	}
	if current != nil {
		current.closeIdleConnections()
	}
}

func (e *Engine) finalize(parent context.Context, completion contract.Completion) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(parent), e.finalizationTimeout)
	defer cancel()
	if err := e.metering.Complete(ctx, completion); err != nil {
		return publicError(contract.ErrorInternal, "metering completion failed", 500, false, err)
	}
	return nil
}

func (e *Engine) finalizePrepared(parent context.Context, prepared preparedRequest, completion contract.Completion) error {
	err := e.finalize(parent, completion)
	e.recordTerminal(parent, prepared, completion.Status, completion.ErrorCode, completion.CacheStatus, completion.Usage, completion.Attempts, completion.EndedAt, err)
	return err
}

func (e *Engine) cancel(parent context.Context, cancellation contract.Cancellation) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(parent), e.finalizationTimeout)
	defer cancel()
	if err := e.metering.Cancel(ctx, cancellation); err != nil {
		return publicError(contract.ErrorInternal, "metering cancellation failed", 500, false, err)
	}
	return nil
}

func (e *Engine) cancelPrepared(parent context.Context, prepared preparedRequest, cancellation contract.Cancellation) error {
	err := e.cancel(parent, cancellation)
	e.recordTerminal(parent, prepared, "cancelled", contract.ErrorProvider, "", cancellation.Usage, cancellation.Attempts, cancellation.EndedAt, err)
	return err
}

func (e *Engine) recordTerminal(parent context.Context, prepared preparedRequest, status string, code contract.ErrorCode, cacheStatus string,
	usage contract.Usage, attempts []contract.Attempt, ended time.Time, meteringErr error) {
	ctx := context.WithoutCancel(parent)
	for _, attempt := range attempts {
		e.telemetry.Record(ctx, TelemetryEvent{Name: "attempt.completed", RequestID: prepared.request.ID,
			Revision: prepared.runtime.catalog.Revision(), Attributes: map[string]string{
				"operation": string(prepared.request.Operation), "model": prepared.request.PublicModel,
				"provider_id": string(attempt.ProviderID), "deployment_id": string(attempt.DeploymentID),
				"outcome": attempt.Outcome, "error_code": string(attempt.ErrorCode),
			}, Measures: map[string]int64{"attempt": int64(attempt.Number), "duration_ms": durationMillis(attempt.StartedAt, attempt.EndedAt),
				"time_to_first_byte_ms": durationMillis(attempt.StartedAt, attempt.FirstByteAt), "input_tokens": attempt.Usage.InputTokens,
				"output_tokens": attempt.Usage.OutputTokens, "cost_micros": attempt.Usage.CostMicros}})
	}
	meteringStatus := "ok"
	if meteringErr != nil {
		meteringStatus = "failed"
	}
	e.telemetry.Record(ctx, TelemetryEvent{Name: "request.completed", RequestID: prepared.request.ID,
		Revision: prepared.runtime.catalog.Revision(), Attributes: map[string]string{
			"operation": string(prepared.request.Operation), "model": prepared.request.PublicModel, "status": status,
			"error_code": string(code), "cache_status": cacheStatus, "key_id": string(prepared.principal.KeyID),
			"key_prefix": prepared.principal.KeyPrefix, "owner_id": string(prepared.principal.OwnerID),
			"team_id": string(prepared.principal.TeamID), "metering": meteringStatus,
		}, Measures: map[string]int64{"attempt_count": int64(len(attempts)), "duration_ms": durationMillis(prepared.request.StartedAt, ended),
			"input_tokens": usage.InputTokens, "output_tokens": usage.OutputTokens, "total_tokens": usage.TotalTokens, "cost_micros": usage.CostMicros}})
}

func durationMillis(start, end time.Time) int64 {
	if start.IsZero() || end.IsZero() || end.Before(start) {
		return 0
	}
	return end.Sub(start).Milliseconds()
}

func (e *Engine) waitForRetry(ctx context.Context, failureIndex int, failure *contract.Error) error {
	delay := e.baseRetryDelay
	for index := 0; index < failureIndex && delay < e.maxRetryDelay; index++ {
		if delay > e.maxRetryDelay/2 {
			delay = e.maxRetryDelay
			break
		}
		delay *= 2
	}
	if failure.RetryAfter > delay {
		delay = failure.RetryAfter
	}
	if delay > e.maxRetryDelay {
		delay = e.maxRetryDelay
	}
	timer := e.clock.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return normalizeError(ctx.Err(), contract.ErrorProvider, 499, false)
	case <-timer.C():
		return nil
	}
}

func normalizeError(err error, fallback contract.ErrorCode, status int, retryable bool) *contract.Error {
	var gatewayError *contract.Error
	if errors.As(err, &gatewayError) {
		return gatewayError.Safe()
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return publicError(contract.ErrorTimeout, "request timed out", 504, true, err)
	}
	if errors.Is(err, context.Canceled) {
		return publicError(contract.ErrorProvider, "request cancelled", 499, false, err)
	}
	return publicError(fallback, safeMessage(fallback), status, retryable, err)
}

func publicError(code contract.ErrorCode, message string, status int, retryable bool, cause error) *contract.Error {
	return &contract.Error{Code: code, Message: message, HTTPStatus: status, Retryable: retryable, Cause: cause}
}

func safeMessage(code contract.ErrorCode) string {
	switch code {
	case contract.ErrorPermission:
		return "permission denied"
	case contract.ErrorProvider:
		return "upstream provider failed"
	default:
		return "request failed"
	}
}
