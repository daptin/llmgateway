package catalog

import (
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/daptin/llmgateway/contract"
)

type Snapshot struct {
	revision    uint64
	providers   map[contract.ID]Provider
	models      map[contract.ID]Model
	modelByName map[string]contract.ID
	deployments map[contract.ID]Deployment
	byModel     map[contract.ID][]contract.ID
	policies    map[contract.ID]Policy
	guardrails  map[contract.ID]Guardrail
	defaults    map[contract.ID]modelDefaults
}

func Compile(doc Document) (*Snapshot, error) {
	if doc.Revision == 0 {
		return nil, errors.New("catalog revision must be positive")
	}
	s := &Snapshot{
		revision: doc.Revision, providers: make(map[contract.ID]Provider),
		models: make(map[contract.ID]Model), modelByName: make(map[string]contract.ID),
		deployments: make(map[contract.ID]Deployment), byModel: make(map[contract.ID][]contract.ID),
		policies: make(map[contract.ID]Policy), guardrails: make(map[contract.ID]Guardrail),
		defaults: make(map[contract.ID]modelDefaults),
	}
	for _, provider := range doc.Providers {
		if err := validateProvider(provider); err != nil {
			return nil, err
		}
		if _, exists := s.providers[provider.ID]; exists {
			return nil, fmt.Errorf("duplicate provider id %q", provider.ID)
		}
		s.providers[provider.ID] = cloneProvider(provider)
	}
	for _, policy := range doc.Policies {
		if policy.ID == "" || strings.TrimSpace(policy.Name) == "" {
			return nil, errors.New("policy id and name are required")
		}
		if _, exists := s.policies[policy.ID]; exists {
			return nil, fmt.Errorf("duplicate policy id %q", policy.ID)
		}
		if err := validateLimits(policy.Limits); err != nil {
			return nil, fmt.Errorf("policy %q: %w", policy.ID, err)
		}
		policy.Limits = append([]Limit(nil), policy.Limits...)
		s.policies[policy.ID] = policy
	}
	for _, model := range doc.Models {
		if err := validateModel(model); err != nil {
			return nil, err
		}
		defaults, err := parseModelDefaults(model)
		if err != nil {
			return nil, err
		}
		if _, exists := s.models[model.ID]; exists {
			return nil, fmt.Errorf("duplicate model id %q", model.ID)
		}
		if _, exists := s.modelByName[model.Name]; exists {
			return nil, fmt.Errorf("duplicate public model name %q", model.Name)
		}
		if model.PolicyID != "" {
			if _, exists := s.policies[model.PolicyID]; !exists {
				return nil, fmt.Errorf("model %q references unknown policy %q", model.ID, model.PolicyID)
			}
		}
		s.models[model.ID] = cloneModel(model)
		s.defaults[model.ID] = defaults
		s.modelByName[model.Name] = model.ID
	}
	if err := validateFallbacks(s.models); err != nil {
		return nil, err
	}
	for _, deployment := range doc.Deployments {
		if deployment.RequestTimeout == 0 {
			deployment.RequestTimeout = 120 * time.Second
		}
		if deployment.ConnectTimeout == 0 {
			deployment.ConnectTimeout = 5 * time.Second
		}
		if deployment.HealthCheck.Enabled {
			if deployment.HealthCheck.Interval == 0 {
				deployment.HealthCheck.Interval = 30 * time.Second
			}
			if deployment.HealthCheck.Timeout == 0 {
				deployment.HealthCheck.Timeout = 5 * time.Second
			}
			if deployment.HealthCheck.FailureThreshold == 0 {
				deployment.HealthCheck.FailureThreshold = 3
			}
		}
		if err := validateDeployment(deployment, s.models, s.providers); err != nil {
			return nil, err
		}
		if _, exists := s.deployments[deployment.ID]; exists {
			return nil, fmt.Errorf("duplicate deployment id %q", deployment.ID)
		}
		s.deployments[deployment.ID] = cloneDeployment(deployment)
		s.byModel[deployment.ModelID] = append(s.byModel[deployment.ModelID], deployment.ID)
	}
	for modelID := range s.byModel {
		sort.Slice(s.byModel[modelID], func(i, j int) bool {
			a := s.deployments[s.byModel[modelID][i]]
			b := s.deployments[s.byModel[modelID][j]]
			if a.Priority == b.Priority {
				return a.ID < b.ID
			}
			return a.Priority < b.Priority
		})
	}
	for _, guardrail := range doc.Guardrails {
		if guardrail.ID == "" || strings.TrimSpace(guardrail.Name) == "" || strings.TrimSpace(guardrail.Kind) == "" {
			return nil, errors.New("guardrail id, name, and kind are required")
		}
		if guardrail.Phase != "input" && guardrail.Phase != "output" && guardrail.Phase != "both" {
			return nil, fmt.Errorf("guardrail %q has invalid phase %q", guardrail.ID, guardrail.Phase)
		}
		if guardrail.FailMode != "open" && guardrail.FailMode != "closed" {
			return nil, fmt.Errorf("guardrail %q has invalid fail mode %q", guardrail.ID, guardrail.FailMode)
		}
		if _, exists := s.guardrails[guardrail.ID]; exists {
			return nil, fmt.Errorf("duplicate guardrail id %q", guardrail.ID)
		}
		s.guardrails[guardrail.ID] = cloneGuardrail(guardrail)
	}
	for modelID, model := range s.models {
		seen := make(map[contract.ID]struct{}, len(model.GuardrailIDs))
		for _, guardrailID := range model.GuardrailIDs {
			if _, duplicate := seen[guardrailID]; duplicate {
				return nil, fmt.Errorf("model %q has duplicate guardrail %q", modelID, guardrailID)
			}
			seen[guardrailID] = struct{}{}
			if _, exists := s.guardrails[guardrailID]; !exists {
				return nil, fmt.Errorf("model %q references unknown guardrail %q", modelID, guardrailID)
			}
		}
	}
	return s, nil
}

func (s *Snapshot) Revision() uint64 { return s.revision }

func (s *Snapshot) ModelByName(name string) (Model, bool) {
	id, ok := s.modelByName[name]
	if !ok {
		return Model{}, false
	}
	model := s.models[id]
	return cloneModel(model), true
}

func (s *Snapshot) Model(id contract.ID) (Model, bool) {
	model, ok := s.models[id]
	if !ok {
		return Model{}, false
	}
	return cloneModel(model), true
}

func (s *Snapshot) Models() []Model {
	ids := make([]contract.ID, 0, len(s.models))
	for id := range s.models {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	models := make([]Model, 0, len(ids))
	for _, id := range ids {
		models = append(models, cloneModel(s.models[id]))
	}
	return models
}

func (s *Snapshot) Provider(id contract.ID) (Provider, bool) {
	provider, ok := s.providers[id]
	if !ok {
		return Provider{}, false
	}
	return cloneProvider(provider), true
}

func (s *Snapshot) Policy(id contract.ID) (Policy, bool) {
	policy, ok := s.policies[id]
	if !ok {
		return Policy{}, false
	}
	policy.Limits = append([]Limit(nil), policy.Limits...)
	return policy, true
}

func (s *Snapshot) Providers() []Provider {
	ids := make([]contract.ID, 0, len(s.providers))
	for id := range s.providers {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	providers := make([]Provider, 0, len(ids))
	for _, id := range ids {
		providers = append(providers, cloneProvider(s.providers[id]))
	}
	return providers
}

func (s *Snapshot) GuardrailsForModel(id contract.ID) []Guardrail {
	model, ok := s.models[id]
	if !ok {
		return nil
	}
	result := make([]Guardrail, 0, len(model.GuardrailIDs))
	for _, guardrailID := range model.GuardrailIDs {
		guardrail := s.guardrails[guardrailID]
		if guardrail.Enabled {
			result = append(result, cloneGuardrail(guardrail))
		}
	}
	sort.SliceStable(result, func(i, j int) bool {
		if result[i].Priority == result[j].Priority {
			return result[i].ID < result[j].ID
		}
		return result[i].Priority < result[j].Priority
	})
	return result
}

func (s *Snapshot) DeploymentsForModel(id contract.ID) []Deployment {
	ids := s.byModel[id]
	result := make([]Deployment, 0, len(ids))
	for _, deploymentID := range ids {
		result = append(result, cloneDeployment(s.deployments[deploymentID]))
	}
	return result
}

func (s *Snapshot) Deployments() []Deployment {
	ids := make([]contract.ID, 0, len(s.deployments))
	for id := range s.deployments {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	result := make([]Deployment, 0, len(ids))
	for _, id := range ids {
		result = append(result, cloneDeployment(s.deployments[id]))
	}
	return result
}

func validateProvider(provider Provider) error {
	if provider.ID == "" || strings.TrimSpace(provider.Name) == "" || strings.TrimSpace(provider.Type) == "" {
		return errors.New("provider id, name, and type are required")
	}
	if provider.BaseURL != "" {
		parsed, err := url.Parse(provider.BaseURL)
		if err != nil || parsed.Scheme == "" || parsed.Host == "" {
			return fmt.Errorf("provider %q has invalid base URL", provider.ID)
		}
		if parsed.Scheme != "https" && parsed.Scheme != "http" {
			return fmt.Errorf("provider %q uses unsupported URL scheme %q", provider.ID, parsed.Scheme)
		}
		if parsed.Scheme == "http" && !provider.AllowInsecure {
			return fmt.Errorf("provider %q must explicitly allow an insecure base URL", provider.ID)
		}
	}
	return nil
}

func validateModel(model Model) error {
	if model.ID == "" || strings.TrimSpace(model.Name) == "" {
		return errors.New("model id and name are required")
	}
	if len(model.Operations) == 0 {
		return fmt.Errorf("model %q must declare an operation", model.ID)
	}
	for _, operation := range model.Operations {
		if !operation.Valid() {
			return fmt.Errorf("model %q has invalid operation %q", model.ID, operation)
		}
	}
	if model.RoutingStrategy != "priority_weighted" {
		return fmt.Errorf("model %q has unsupported routing strategy %q", model.ID, model.RoutingStrategy)
	}
	switch model.UnsupportedParameterPolicy {
	case "reject", "drop", "passthrough":
	default:
		return fmt.Errorf("model %q has unsupported parameter policy %q", model.ID, model.UnsupportedParameterPolicy)
	}
	if err := validateModelCapabilities(model); err != nil {
		return err
	}
	return nil
}

func validateModelCapabilities(model Model) error {
	allowed := map[string]struct{}{
		"audio": {}, "dimensions": {}, "exact_cache": {}, "json_schema": {}, "logprobs": {},
		"public_cache": {}, "token_ids": {}, "tools": {}, "vision": {},
	}
	for capability := range model.Capabilities {
		if _, ok := allowed[capability]; !ok {
			return fmt.Errorf("model %q has unknown capability %q", model.ID, capability)
		}
	}
	if model.Capabilities["public_cache"] && !model.Capabilities["exact_cache"] {
		return fmt.Errorf("model %q public_cache requires exact_cache", model.ID)
	}
	return nil
}

func validateDeployment(deployment Deployment, models map[contract.ID]Model, providers map[contract.ID]Provider) error {
	if deployment.ID == "" || strings.TrimSpace(deployment.Name) == "" || strings.TrimSpace(deployment.UpstreamModel) == "" {
		return errors.New("deployment id, name, and upstream model are required")
	}
	model, modelExists := models[deployment.ModelID]
	if !modelExists {
		return fmt.Errorf("deployment %q references unknown model %q", deployment.ID, deployment.ModelID)
	}
	if _, providerExists := providers[deployment.ProviderID]; !providerExists {
		return fmt.Errorf("deployment %q references unknown provider %q", deployment.ID, deployment.ProviderID)
	}
	if deployment.Weight <= 0 {
		return fmt.Errorf("deployment %q weight must be positive", deployment.ID)
	}
	if deployment.MaxConcurrency < -1 || deployment.RPM < -1 || deployment.TPM < -1 {
		return fmt.Errorf("deployment %q limits must be -1 or non-negative", deployment.ID)
	}
	if deployment.RequestTimeout < time.Millisecond || deployment.ConnectTimeout < time.Millisecond || deployment.ConnectTimeout > deployment.RequestTimeout {
		return fmt.Errorf("deployment %q timeouts must be positive and connect cannot exceed request", deployment.ID)
	}
	if deployment.HealthCheck.Enabled && (deployment.HealthCheck.Interval < time.Second || deployment.HealthCheck.Timeout < time.Millisecond ||
		deployment.HealthCheck.Timeout > deployment.HealthCheck.Interval || deployment.HealthCheck.FailureThreshold < 1) {
		return fmt.Errorf("deployment %q health check interval, timeout, and failure threshold are invalid", deployment.ID)
	}
	allowed := make(map[contract.Operation]struct{}, len(model.Operations))
	for _, operation := range model.Operations {
		allowed[operation] = struct{}{}
	}
	if len(deployment.Operations) == 0 {
		return fmt.Errorf("deployment %q must declare an operation", deployment.ID)
	}
	for _, operation := range deployment.Operations {
		if _, ok := allowed[operation]; !ok {
			return fmt.Errorf("deployment %q operation %q is not declared by model", deployment.ID, operation)
		}
	}
	for _, value := range []int64{deployment.Pricing.InputMicrosPerMillion, deployment.Pricing.OutputMicrosPerMillion,
		deployment.Pricing.CacheReadMicrosPerMillion, deployment.Pricing.CacheWriteMicrosPerMillion,
		deployment.Pricing.ReasoningMicrosPerMillion} {
		if value < 0 {
			return fmt.Errorf("deployment %q pricing cannot be negative", deployment.ID)
		}
	}
	return nil
}

func validateLimits(limits []Limit) error {
	seen := make(map[string]struct{}, len(limits))
	for _, limit := range limits {
		if strings.TrimSpace(limit.Metric) == "" {
			return errors.New("limit metric is required")
		}
		if limit.Metric != "concurrency" && strings.TrimSpace(limit.Window) == "" {
			return errors.New("limit window is required except for concurrency")
		}
		if limit.Maximum < 0 {
			return errors.New("limit maximum cannot be negative")
		}
		if limit.Mode != "hard" && limit.Mode != "soft" {
			return fmt.Errorf("invalid limit mode %q", limit.Mode)
		}
		key := limit.Metric + "\x00" + limit.Window
		if _, exists := seen[key]; exists {
			return fmt.Errorf("duplicate limit %s/%s", limit.Metric, limit.Window)
		}
		seen[key] = struct{}{}
	}
	return nil
}

func validateFallbacks(models map[contract.ID]Model) error {
	state := make(map[contract.ID]uint8, len(models))
	var visit func(contract.ID) error
	visit = func(id contract.ID) error {
		switch state[id] {
		case 1:
			return fmt.Errorf("fallback cycle contains model %q", id)
		case 2:
			return nil
		}
		state[id] = 1
		for _, fallback := range models[id].FallbackModelIDs {
			if _, exists := models[fallback]; !exists {
				return fmt.Errorf("model %q references unknown fallback %q", id, fallback)
			}
			if err := visit(fallback); err != nil {
				return err
			}
		}
		state[id] = 2
		return nil
	}
	for id := range models {
		if err := visit(id); err != nil {
			return err
		}
	}
	return nil
}

func cloneProvider(value Provider) Provider {
	value.Parameters = append([]byte(nil), value.Parameters...)
	return value
}

func cloneModel(value Model) Model {
	value.Operations = append([]contract.Operation(nil), value.Operations...)
	value.FallbackModelIDs = append([]contract.ID(nil), value.FallbackModelIDs...)
	value.GuardrailIDs = append([]contract.ID(nil), value.GuardrailIDs...)
	value.DefaultParameters = append([]byte(nil), value.DefaultParameters...)
	if value.Capabilities != nil {
		source := value.Capabilities
		value.Capabilities = make(map[string]bool, len(source))
		for key, enabled := range source {
			value.Capabilities[key] = enabled
		}
	}
	return value
}

func cloneDeployment(value Deployment) Deployment {
	value.Operations = append([]contract.Operation(nil), value.Operations...)
	value.Parameters = append([]byte(nil), value.Parameters...)
	return value
}

func cloneGuardrail(value Guardrail) Guardrail {
	value.Config = append([]byte(nil), value.Config...)
	return value
}
