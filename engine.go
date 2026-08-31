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
	Selector   Selector
	Clock      Clock
}

type Options struct {
	MaxAttempts         int
	FinalizationTimeout time.Duration
	BaseRetryDelay      time.Duration
	MaxRetryDelay       time.Duration
	CircuitFailures     int64
	CircuitWindow       time.Duration
	CircuitCooldown     time.Duration
}

type runtimeSnapshot struct {
	catalog  *catalog.Snapshot
	adapters map[contract.ID]adapter.Adapter
}

func (r *runtimeSnapshot) Capabilities(providerID contract.ID) (adapter.Capabilities, bool) {
	value, ok := r.adapters[providerID]
	if !ok {
		return adapter.Capabilities{}, false
	}
	return value.Capabilities(), true
}

type Engine struct {
	catalog             CatalogSource
	secrets             SecretResolver
	adapters            *adapter.Registry
	authorizer          Authorizer
	accounting          AccountingStore
	counters            CounterStore
	selector            Selector
	clock               Clock
	maxAttempts         int
	finalizationTimeout time.Duration
	baseRetryDelay      time.Duration
	maxRetryDelay       time.Duration
	circuitFailures     int64
	circuitWindow       time.Duration
	circuitCooldown     time.Duration
	snapshot            atomic.Pointer[runtimeSnapshot]
	draining            atomic.Bool
	reloadMu            sync.Mutex
}

func New(dependencies Dependencies, options Options) (*Engine, error) {
	if dependencies.Catalog == nil {
		return nil, errors.New("catalog source is required")
	}
	if dependencies.Adapters == nil || dependencies.Authorizer == nil || dependencies.Accounting == nil || dependencies.Counters == nil || dependencies.Selector == nil || dependencies.Clock == nil {
		return nil, errors.New("adapters, authorizer, accounting, counters, selector, and clock are required")
	}
	if options.MaxAttempts == 0 {
		options.MaxAttempts = 3
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
	dependencies.Adapters.Freeze()
	return &Engine{
		catalog: dependencies.Catalog, secrets: dependencies.Secrets, adapters: dependencies.Adapters,
		authorizer: dependencies.Authorizer, accounting: dependencies.Accounting, counters: dependencies.Counters, selector: dependencies.Selector,
		clock: dependencies.Clock, maxAttempts: options.MaxAttempts, finalizationTimeout: options.FinalizationTimeout,
		baseRetryDelay: options.BaseRetryDelay, maxRetryDelay: options.MaxRetryDelay,
		circuitFailures: options.CircuitFailures, circuitWindow: options.CircuitWindow, circuitCooldown: options.CircuitCooldown,
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
		return err
	}
	compiled, err := catalog.Compile(document)
	if err != nil {
		return err
	}
	instances := make(map[contract.ID]adapter.Adapter)
	for _, provider := range compiled.Providers() {
		if !provider.Enabled {
			continue
		}
		factory, ok := e.adapters.Factory(provider.Type)
		if !ok {
			return fmt.Errorf("provider %q uses unregistered adapter %q", provider.ID, provider.Type)
		}
		var secretBytes []byte
		if provider.SecretRef != "" {
			if e.secrets == nil {
				return fmt.Errorf("provider %q requires a secret resolver", provider.ID)
			}
			secretBytes, err = e.secrets.ResolveSecret(ctx, provider.SecretRef)
			if err != nil {
				return fmt.Errorf("resolve provider %q secret: %w", provider.ID, err)
			}
		}
		instance, buildErr := factory.Build(ctx, provider, adapter.NewSecret(secretBytes))
		for index := range secretBytes {
			secretBytes[index] = 0
		}
		if buildErr != nil {
			return fmt.Errorf("build provider %q adapter: %w", provider.ID, buildErr)
		}
		if instance == nil {
			return fmt.Errorf("provider %q adapter factory returned nil", provider.ID)
		}
		instances[provider.ID] = instance
	}
	next := &runtimeSnapshot{catalog: compiled, adapters: instances}
	current := e.snapshot.Load()
	if current != nil && next.catalog.Revision() <= current.catalog.Revision() {
		return catalog.ErrStaleRevision
	}
	e.snapshot.Store(next)
	return nil
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

func (e *Engine) Drain(context.Context) error {
	e.draining.Store(true)
	return nil
}

func (e *Engine) Invoke(ctx context.Context, principal contract.Principal, request contract.Request) (contract.Response, error) {
	if request.Stream {
		return contract.Response{}, publicError(contract.ErrorInvalidRequest, "invalid non-streaming request", 400, false, nil)
	}
	prepared, err := e.prepare(ctx, principal, request)
	if err != nil {
		return contract.Response{}, err
	}
	settled := false
	defer func() {
		if !settled {
			_ = e.cancel(ctx, contract.Cancellation{Token: prepared.token, Reason: "invoke_abandoned", EndedAt: e.clock.Now()})
		}
	}()
	finish := func(completion contract.Completion) error {
		if finishErr := e.finalize(ctx, completion); finishErr != nil {
			return finishErr
		}
		settled = true
		return nil
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
		usage := response.Usage
		if usage.TotalTokens == 0 {
			usage.TotalTokens = usage.InputTokens + usage.OutputTokens
		}
		cost, costErr := accounting.CostMicros(usage, routeAttempt.Deployment.Pricing)
		if costErr != nil || !usage.Valid() {
			normalized := publicError(contract.ErrorProvider, "provider returned invalid usage", 502, false, costErr)
			attempts = append(attempts, contract.Attempt{Number: index + 1, ProviderID: routeAttempt.Provider.ID, DeploymentID: routeAttempt.Deployment.ID, StartedAt: started, EndedAt: ended, Outcome: "failed", ErrorCode: normalized.Code, HTTPStatus: normalized.HTTPStatus})
			if finishErr := finish(contract.Completion{Token: prepared.token, Status: "failed", HTTPStatus: 502, ErrorCode: normalized.Code, Attempts: attempts, EndedAt: ended}); finishErr != nil {
				return contract.Response{}, finishErr
			}
			return contract.Response{}, normalized
		}
		usage.CostMicros = cost
		response.RequestID = prepared.request.ID
		response.Model = prepared.request.PublicModel
		response.Usage = usage
		attempts = append(attempts, contract.Attempt{Number: index + 1, ProviderID: routeAttempt.Provider.ID, DeploymentID: routeAttempt.Deployment.ID, StartedAt: started, EndedAt: ended, Outcome: "succeeded", HTTPStatus: 200, Usage: usage})
		if err := finish(contract.Completion{Token: prepared.token, Status: "succeeded", HTTPStatus: 200, Usage: usage, Attempts: attempts, EndedAt: ended}); err != nil {
			return contract.Response{}, err
		}
		return response, nil
	}
	return contract.Response{}, publicError(contract.ErrorUnavailable, "no healthy deployment", 503, true, nil)
}

type preparedRequest struct {
	runtime *runtimeSnapshot
	request contract.Request
	model   catalog.Model
	plan    routing.Plan
	token   contract.ReservationToken
}

func (e *Engine) prepare(ctx context.Context, principal contract.Principal, request contract.Request) (preparedRequest, error) {
	runtime, err := e.currentRuntime()
	if err != nil {
		return preparedRequest{}, err
	}
	if err := request.Validate(); err != nil {
		return preparedRequest{}, publicError(contract.ErrorInvalidRequest, "invalid request", 400, false, err)
	}
	if request.StartedAt.IsZero() {
		request.StartedAt = e.clock.Now()
	}
	model, ok := runtime.catalog.ModelByName(request.PublicModel)
	if !ok || !model.Enabled {
		return preparedRequest{}, publicError(contract.ErrorModelNotFound, "model not found", 404, false, nil)
	}
	if err := e.authorizer.Authorize(ctx, principal, model); err != nil {
		return preparedRequest{}, normalizeError(err, contract.ErrorPermission, 403, false)
	}
	plan, err := routing.Build(runtime.catalog, request.PublicModel, request.Operation, runtime, e.selector)
	if err != nil {
		return preparedRequest{}, publicError(contract.ErrorUnavailable, "no healthy deployment", 503, true, err)
	}
	if len(plan.Attempts) > e.maxAttempts {
		plan.Attempts = plan.Attempts[:e.maxAttempts]
	}
	estimate := request.EstimatedUsage
	if estimate.TotalTokens == 0 {
		estimate.TotalTokens = estimate.InputTokens + estimate.OutputTokens
	}
	if !estimate.Valid() {
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
	bindings, err := policyBindings(runtime.catalog, principal, model)
	if err != nil {
		return preparedRequest{}, publicError(contract.ErrorInternal, "invalid effective policy", 500, false, err)
	}
	reservations, err := accounting.Reservations(bindings, accounting.Measures{
		Requests: 1, InputTokens: estimate.InputTokens, OutputTokens: estimate.OutputTokens,
		TotalTokens: estimate.TotalTokens, CostMicros: estimate.CostMicros, Concurrency: 1,
	}, request.StartedAt)
	if err != nil {
		return preparedRequest{}, publicError(contract.ErrorInternal, "invalid effective policy", 500, false, err)
	}
	token, err := e.accounting.Admit(ctx, contract.Admission{
		RequestID: request.ID, Principal: principal, ModelID: model.ID, Operation: request.Operation,
		StartedAt: request.StartedAt, EstimatedUsage: estimate, LimitReservations: reservations,
	})
	if err != nil {
		if errors.Is(err, accounting.ErrLimitExceeded) {
			return preparedRequest{}, publicError(contract.ErrorInsufficientQuota, "insufficient quota", 402, false, err)
		}
		return preparedRequest{}, publicError(contract.ErrorInternal, "accounting admission failed", 500, false, err)
	}
	request.EstimatedUsage = estimate
	return preparedRequest{runtime: runtime, request: request, model: model, plan: plan, token: token}, nil
}

func (e *Engine) currentRuntime() (*runtimeSnapshot, error) {
	if e.draining.Load() {
		return nil, ErrDraining
	}
	current := e.snapshot.Load()
	if current == nil {
		return nil, ErrNotReady
	}
	return current, nil
}

func (e *Engine) finalize(parent context.Context, completion contract.Completion) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(parent), e.finalizationTimeout)
	defer cancel()
	if err := e.accounting.Finalize(ctx, completion); err != nil {
		return publicError(contract.ErrorInternal, "accounting finalization failed", 500, false, err)
	}
	return nil
}

func (e *Engine) cancel(parent context.Context, cancellation contract.Cancellation) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(parent), e.finalizationTimeout)
	defer cancel()
	if err := e.accounting.Cancel(ctx, cancellation); err != nil {
		return publicError(contract.ErrorInternal, "accounting cancellation failed", 500, false, err)
	}
	return nil
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
