package routing

import (
	"errors"
	"fmt"
	"sort"

	"github.com/daptin/llmgateway/adapter"
	"github.com/daptin/llmgateway/catalog"
	"github.com/daptin/llmgateway/contract"
)

var ErrNoDeployment = errors.New("no eligible deployment")

type Selector interface {
	Intn(int) int
}

type CapabilitySource interface {
	Capabilities(contract.ID) (adapter.Capabilities, bool)
}

type Attempt struct {
	Model      catalog.Model
	Deployment catalog.Deployment
	Provider   catalog.Provider
}

type Plan struct {
	RequestedModel catalog.Model
	Attempts       []Attempt
}

func Build(snapshot *catalog.Snapshot, publicModel string, operation contract.Operation, capabilities CapabilitySource, selector Selector) (Plan, error) {
	if snapshot == nil || capabilities == nil || selector == nil {
		return Plan{}, errors.New("snapshot, capabilities, and selector are required")
	}
	if !operation.Valid() {
		return Plan{}, fmt.Errorf("invalid operation %q", operation)
	}
	requested, ok := snapshot.ModelByName(publicModel)
	if !ok || !requested.Enabled {
		return Plan{}, fmt.Errorf("%w: model %q", ErrNoDeployment, publicModel)
	}
	if !supports(requested.Operations, operation) {
		return Plan{}, fmt.Errorf("%w: model %q does not support %q", ErrNoDeployment, publicModel, operation)
	}
	plan := Plan{RequestedModel: requested}
	visited := make(map[contract.ID]bool)
	var appendModel func(catalog.Model) error
	appendModel = func(model catalog.Model) error {
		if visited[model.ID] {
			return nil
		}
		visited[model.ID] = true
		plan.Attempts = append(plan.Attempts, attemptsForModel(snapshot, model, operation, capabilities, selector)...)
		for _, fallbackID := range model.FallbackModelIDs {
			fallback, exists := snapshot.Model(fallbackID)
			if !exists {
				return fmt.Errorf("compiled catalog lost fallback model %q", fallbackID)
			}
			if fallback.Enabled && supports(fallback.Operations, operation) {
				if err := appendModel(fallback); err != nil {
					return err
				}
			}
		}
		return nil
	}
	if err := appendModel(requested); err != nil {
		return Plan{}, err
	}
	if len(plan.Attempts) == 0 {
		return Plan{}, fmt.Errorf("%w: model %q operation %q", ErrNoDeployment, publicModel, operation)
	}
	return plan, nil
}

func attemptsForModel(snapshot *catalog.Snapshot, model catalog.Model, operation contract.Operation, capabilities CapabilitySource, selector Selector) []Attempt {
	tiers := make(map[int][]Attempt)
	priorities := make([]int, 0)
	for _, deployment := range snapshot.DeploymentsForModel(model.ID) {
		if !deployment.Enabled || !supports(deployment.Operations, operation) {
			continue
		}
		provider, ok := snapshot.Provider(deployment.ProviderID)
		if !ok || !provider.Enabled {
			continue
		}
		providerCapabilities, ok := capabilities.Capabilities(provider.ID)
		if !ok || !providerCapabilities.Supports(operation) {
			continue
		}
		if _, exists := tiers[deployment.Priority]; !exists {
			priorities = append(priorities, deployment.Priority)
		}
		tiers[deployment.Priority] = append(tiers[deployment.Priority], Attempt{Model: model, Deployment: deployment, Provider: provider})
	}
	sort.Ints(priorities)
	var ordered []Attempt
	for _, priority := range priorities {
		ordered = append(ordered, weightedPermutation(tiers[priority], selector)...)
	}
	return ordered
}

func weightedPermutation(candidates []Attempt, selector Selector) []Attempt {
	remaining := append([]Attempt(nil), candidates...)
	ordered := make([]Attempt, 0, len(remaining))
	for len(remaining) > 0 {
		total := 0
		for _, candidate := range remaining {
			total += candidate.Deployment.Weight
		}
		draw := selector.Intn(total)
		selected := 0
		for index, candidate := range remaining {
			if draw < candidate.Deployment.Weight {
				selected = index
				break
			}
			draw -= candidate.Deployment.Weight
		}
		ordered = append(ordered, remaining[selected])
		remaining = append(remaining[:selected], remaining[selected+1:]...)
	}
	return ordered
}

func supports(operations []contract.Operation, operation contract.Operation) bool {
	for _, candidate := range operations {
		if candidate == operation {
			return true
		}
	}
	return false
}
