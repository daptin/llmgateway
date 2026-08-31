package llmgateway

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/daptin/llmgateway/accounting"
	"github.com/daptin/llmgateway/adapter"
	"github.com/daptin/llmgateway/catalog"
	"github.com/daptin/llmgateway/contract"
	"github.com/daptin/llmgateway/guardrail"
	"github.com/daptin/llmgateway/internal/routing"
)

var (
	ErrDraining     = errors.New("llmgateway is draining")
	ErrNotReady     = errors.New("llmgateway has no valid catalog snapshot")
	ErrCounterLimit = errors.New("llmgateway transient counter limit exceeded")
)

type Dependencies struct {
	Catalog    CatalogSource
	Secrets    SecretResolver
	Adapters   *adapter.Registry
	Authorizer Authorizer
	Accounting AccountingStore
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
	MaxCacheEntryBytes     int
	FirstEventTimeout      time.Duration
	StreamIdleTimeout      time.Duration
	RequestTimeout         time.Duration
	HealthProbeTimeout     time.Duration
	HealthProbeWorkers     int
}

type runtimeSnapshot struct {
	catalog    *catalog.Snapshot
	adapters   map[contract.ID]adapter.Adapter
	guardrails map[contract.ID][]runtimeGuardrail
}

func (r *runtimeSnapshot) Capabilities(providerID contract.ID) (adapter.Capabilities, bool) {
	value, ok := r.adapters[providerID]
	if !ok {
		return adapter.Capabilities{}, false
	}
	return value.Capabilities(), true
}

type Engine struct {
	catalog                CatalogSource
	secrets                SecretResolver
	adapters               *adapter.Registry
	authorizer             Authorizer
	accounting             AccountingStore
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
	maxCacheEntryBytes     int
	firstEventTimeout      time.Duration
	streamIdleTimeout      time.Duration
	requestTimeout         time.Duration
	healthProbeTimeout     time.Duration
	healthProbeWorkers     int
	snapshot               atomic.Pointer[runtimeSnapshot]
	draining               atomic.Bool
	reloadMu               sync.Mutex
	statusMu               sync.RWMutex
	rejectedRevision       uint64
	reloadFailureStage     string
	activeMu               sync.Mutex
	activeRequests         int64
	drained                chan struct{}
}

func New(dependencies Dependencies, options Options) (*Engine, error) {
	if dependencies.Catalog == nil {
		return nil, errors.New("catalog source is required")
	}
	if dependencies.Adapters == nil || dependencies.Authorizer == nil || dependencies.Accounting == nil || dependencies.Counters == nil || dependencies.Cache == nil || dependencies.Guardrails == nil || dependencies.Telemetry == nil || dependencies.Selector == nil || dependencies.Clock == nil {
		return nil, errors.New("adapters, authorizer, accounting, counters, cache, guardrails, telemetry, selector, and clock are required")
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
	if options.CacheTTL < 0 || options.CacheTimeout < 1 || options.MaxCacheEntryBytes < 1 {
		return nil, errors.New("cache TTL and entry bound must be positive")
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
	if options.HealthProbeTimeout == 0 {
		options.HealthProbeTimeout = 5 * time.Second
	}
	if options.HealthProbeWorkers == 0 {
		options.HealthProbeWorkers = 8
	}
	if options.HealthProbeTimeout < time.Millisecond || options.HealthProbeWorkers < 1 || options.HealthProbeWorkers > 64 {
		return nil, errors.New("health probe timeout and worker bounds are invalid")
	}
	dependencies.Adapters.Freeze()
	dependencies.Guardrails.Freeze()
	drained := make(chan struct{})
	close(drained)
	return &Engine{
		catalog: dependencies.Catalog, secrets: dependencies.Secrets, adapters: dependencies.Adapters,
		authorizer: dependencies.Authorizer, accounting: dependencies.Accounting, counters: dependencies.Counters, cache: dependencies.Cache,
		guardrails: dependencies.Guardrails, telemetry: dependencies.Telemetry, selector: dependencies.Selector,
		clock: dependencies.Clock, maxAttempts: options.MaxAttempts, defaultMaxOutputTokens: options.DefaultMaxOutputTokens, finalizationTimeout: options.FinalizationTimeout,
		baseRetryDelay: options.BaseRetryDelay, maxRetryDelay: options.MaxRetryDelay,
		circuitFailures: options.CircuitFailures, circuitWindow: options.CircuitWindow, circuitCooldown: options.CircuitCooldown,
		cacheTTL: options.CacheTTL, cacheTimeout: options.CacheTimeout, maxCacheEntryBytes: options.MaxCacheEntryBytes,
		firstEventTimeout: options.FirstEventTimeout, streamIdleTimeout: options.StreamIdleTimeout,
		requestTimeout:     options.RequestTimeout,
		healthProbeTimeout: options.HealthProbeTimeout, healthProbeWorkers: options.HealthProbeWorkers,
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
	instances := make(map[contract.ID]adapter.Adapter)
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
		instances[provider.ID] = instance
	}
	for _, deployment := range compiled.Deployments() {
		if !deployment.Enabled {
			continue
		}
		instance := instances[deployment.ProviderID]
		if validator, ok := instance.(adapter.DeploymentValidator); ok {
			if validateErr := validator.ValidateDeployment(deployment); validateErr != nil {
				e.recordReloadFailure(document.Revision, "deployment_config")
				return fmt.Errorf("validate deployment %q: %w", deployment.ID, validateErr)
			}
		}
		if deployment.HealthCheck {
			if _, ok := instance.(adapter.HealthChecker); ok {
				continue
			}
			e.recordReloadFailure(document.Revision, "health_check")
			return fmt.Errorf("deployment %q enables health checks on an adapter without probe support", deployment.ID)
		}
	}
	compiledGuardrails := make(map[contract.ID][]runtimeGuardrail)
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
			compiledGuardrails[model.ID] = append(compiledGuardrails[model.ID], runtimeGuardrail{configuration: configuration, checker: checker})
		}
	}
	next := &runtimeSnapshot{catalog: compiled, adapters: instances, guardrails: compiledGuardrails}
	current := e.snapshot.Load()
	if current != nil && next.catalog.Revision() <= current.catalog.Revision() {
		return catalog.ErrStaleRevision
	}
	e.snapshot.Store(next)
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
	prepared, err = e.admit(ctx, principal, prepared)
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
	for index, routeAttempt := range prepared.plan.Attempts {
		lease, gateErr := e.beforeAttempt(ctx, routeAttempt.Deployment, prepared.request)
		if gateErr != nil {
			normalized := normalizeError(gateErr, contract.ErrorUnavailable, 503, true)
			if !normalized.Retryable || index == len(prepared.plan.Attempts)-1 {
				if finishErr := finish(contract.Completion{Token: prepared.token, Status: "failed", HTTPStatus: normalized.HTTPStatus, ErrorCode: normalized.Code, Attempts: attempts, EndedAt: e.clock.Now()}); finishErr != nil {
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
		response, normalized := e.invokeProvider(ctx, prepared.runtime.adapters[routeAttempt.Provider.ID], routeAttempt.Deployment, prepared.request, lease)
		ended := e.clock.Now()
		if normalized != nil {
			attempts = append(attempts, contract.Attempt{
				Number: index + 1, ProviderID: routeAttempt.Provider.ID, DeploymentID: routeAttempt.Deployment.ID,
				StartedAt: started, EndedAt: ended, Outcome: "failed", ErrorCode: normalized.Code,
				HTTPStatus: normalized.HTTPStatus, Retryable: normalized.Retryable,
			})
			if !normalized.Retryable || index == len(prepared.plan.Attempts)-1 {
				if finishErr := finish(contract.Completion{Token: prepared.token, Status: "failed", HTTPStatus: normalized.HTTPStatus, ErrorCode: normalized.Code, Attempts: attempts, EndedAt: ended}); finishErr != nil {
					return contract.Response{}, finishErr
				}
				return contract.Response{}, normalized
			}
			if waitErr := e.waitForRetry(ctx, index, normalized); waitErr != nil {
				if finishErr := finish(contract.Completion{Token: prepared.token, Status: "cancelled", HTTPStatus: 499, ErrorCode: contract.ErrorProvider, Attempts: attempts, EndedAt: e.clock.Now()}); finishErr != nil {
					return contract.Response{}, finishErr
				}
				return contract.Response{}, waitErr
			}
			continue
		}
		usage, usageErr := settledUsage(response.Usage, prepared.request.EstimatedUsage, routeAttempt.Deployment.Pricing)
		if usageErr != nil || !usage.Valid() {
			normalized := publicError(contract.ErrorProvider, "provider returned invalid usage", 502, false, usageErr)
			attempts = append(attempts, contract.Attempt{Number: index + 1, ProviderID: routeAttempt.Provider.ID, DeploymentID: routeAttempt.Deployment.ID, StartedAt: started, EndedAt: ended, Outcome: "failed", ErrorCode: normalized.Code, HTTPStatus: normalized.HTTPStatus})
			if finishErr := finish(contract.Completion{Token: prepared.token, Status: "failed", HTTPStatus: 502, ErrorCode: normalized.Code, Attempts: attempts, EndedAt: ended}); finishErr != nil {
				return contract.Response{}, finishErr
			}
			return contract.Response{}, normalized
		}
		response.RequestID = prepared.request.ID
		response.Model = prepared.request.PublicModel
		response.Usage = usage
		attempts = append(attempts, contract.Attempt{Number: index + 1, ProviderID: routeAttempt.Provider.ID, DeploymentID: routeAttempt.Deployment.ID, StartedAt: started, EndedAt: ended, Outcome: "succeeded", HTTPStatus: 200, Usage: usage})
		if guardrailErr := e.checkOutput(ctx, prepared, response); guardrailErr != nil {
			normalized := normalizeError(guardrailErr, contract.ErrorPermission, 400, false)
			if finishErr := finish(contract.Completion{Token: prepared.token, Status: "rejected", HTTPStatus: normalized.HTTPStatus, ErrorCode: normalized.Code, Usage: usage, Attempts: attempts, EndedAt: ended}); finishErr != nil {
				return contract.Response{}, finishErr
			}
			return contract.Response{}, normalized
		}
		if err := finish(contract.Completion{Token: prepared.token, Status: "succeeded", HTTPStatus: 200, Usage: usage, Attempts: attempts, EndedAt: ended}); err != nil {
			return contract.Response{}, err
		}
		e.storeCache(ctx, cacheKey, response)
		return response, nil
	}
	return contract.Response{}, publicError(contract.ErrorUnavailable, "no healthy deployment", 503, true, nil)
}

type preparedRequest struct {
	runtime   *runtimeSnapshot
	request   contract.Request
	model     catalog.Model
	plan      routing.Plan
	token     contract.ReservationToken
	principal contract.Principal
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
	estimate := prepared.request.EstimatedUsage
	if normalizeErr := normalizeTokenTotal(&estimate); normalizeErr != nil || !estimate.Valid() {
		return preparedRequest{}, publicError(contract.ErrorInvalidRequest, "invalid usage estimate", 400, false, nil)
	}
	for _, routeAttempt := range plan.Attempts {
		cost, costErr := accounting.CostMicros(estimate, routeAttempt.Deployment.Pricing)
		if costErr != nil {
			return preparedRequest{}, publicError(contract.ErrorInvalidRequest, "usage estimate exceeds supported range", 400, false, costErr)
		}
		if cost > estimate.CostMicros {
			estimate.CostMicros = cost
		}
	}
	bindings, err := policyBindings(prepared.runtime.catalog, principal, prepared.model)
	if err != nil {
		return preparedRequest{}, publicError(contract.ErrorInternal, "invalid effective policy", 500, false, err)
	}
	reservations, err := accounting.Reservations(bindings, accounting.Measures{
		Requests: 1, InputTokens: estimate.InputTokens, OutputTokens: estimate.OutputTokens,
		TotalTokens: estimate.TotalTokens, CostMicros: estimate.CostMicros, Concurrency: 1,
	}, prepared.request.StartedAt)
	if err != nil {
		return preparedRequest{}, publicError(contract.ErrorInternal, "invalid effective policy", 500, false, err)
	}
	token, err := e.accounting.Admit(ctx, contract.Admission{
		RequestID: prepared.request.ID, Principal: principal, ModelID: prepared.model.ID, Operation: prepared.request.Operation,
		StartedAt: prepared.request.StartedAt, EstimatedUsage: estimate, LimitReservations: reservations,
	})
	if err != nil {
		if errors.Is(err, accounting.ErrLimitExceeded) {
			return preparedRequest{}, publicError(contract.ErrorInsufficientQuota, "insufficient quota", 402, false, err)
		}
		return preparedRequest{}, publicError(contract.ErrorInternal, "accounting admission failed", 500, false, err)
	}
	prepared.request.EstimatedUsage = estimate
	prepared.plan = plan
	prepared.token = token
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
	defer e.activeMu.Unlock()
	if e.activeRequests < 1 {
		panic("llmgateway: unbalanced request lifecycle")
	}
	e.activeRequests--
	if e.activeRequests == 0 {
		close(e.drained)
	}
}

func (e *Engine) finalize(parent context.Context, completion contract.Completion) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(parent), e.finalizationTimeout)
	defer cancel()
	if err := e.accounting.Finalize(ctx, completion); err != nil {
		return publicError(contract.ErrorInternal, "accounting finalization failed", 500, false, err)
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
	if err := e.accounting.Cancel(ctx, cancellation); err != nil {
		return publicError(contract.ErrorInternal, "accounting cancellation failed", 500, false, err)
	}
	return nil
}

func (e *Engine) cancelPrepared(parent context.Context, prepared preparedRequest, cancellation contract.Cancellation) error {
	err := e.cancel(parent, cancellation)
	e.recordTerminal(parent, prepared, "cancelled", contract.ErrorProvider, "", cancellation.Usage, cancellation.Attempts, cancellation.EndedAt, err)
	return err
}

func (e *Engine) recordTerminal(parent context.Context, prepared preparedRequest, status string, code contract.ErrorCode, cacheStatus string,
	usage contract.Usage, attempts []contract.Attempt, ended time.Time, accountingErr error) {
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
	accountingStatus := "ok"
	if accountingErr != nil {
		accountingStatus = "failed"
	}
	e.telemetry.Record(ctx, TelemetryEvent{Name: "request.completed", RequestID: prepared.request.ID,
		Revision: prepared.runtime.catalog.Revision(), Attributes: map[string]string{
			"operation": string(prepared.request.Operation), "model": prepared.request.PublicModel, "status": status,
			"error_code": string(code), "cache_status": cacheStatus, "key_id": string(prepared.principal.KeyID),
			"key_prefix": prepared.principal.KeyPrefix, "owner_id": string(prepared.principal.OwnerID),
			"team_id": string(prepared.principal.TeamID), "accounting": accountingStatus,
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

func policyBindings(snapshot *catalog.Snapshot, principal contract.Principal, model catalog.Model) ([]accounting.Binding, error) {
	result := make([]accounting.Binding, 0, len(principal.PolicyBindings)+1)
	for _, reference := range principal.PolicyBindings {
		policy, ok := snapshot.Policy(reference.PolicyID)
		if !ok {
			return nil, fmt.Errorf("principal references unknown policy %q", reference.PolicyID)
		}
		result = append(result, accounting.Binding{ScopeKind: reference.ScopeKind, ScopeID: reference.ScopeID, Policy: policy})
	}
	if model.PolicyID != "" {
		policy, ok := snapshot.Policy(model.PolicyID)
		if !ok {
			return nil, fmt.Errorf("model references unknown policy %q", model.PolicyID)
		}
		result = append(result, accounting.Binding{ScopeKind: "model", ScopeID: model.ID, Policy: policy})
	}
	return result, nil
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
